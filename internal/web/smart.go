package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/config"
	"hcgateway/internal/gateway"
	"hcgateway/internal/i18n"
	"hcgateway/internal/smartthings"
)

// Телевизоры в домашней сети и устройства SmartThings в интерфейсе.

// stCallbackPath - адрес возврата после входа в SmartThings. Его владелец вписывает в
// приложение API Access.
const stCallbackPath = "/connections/smartthings/callback"

// stUsageWarn - с какого расхода запросов за месяц страница предупреждает: в плане
// разработчика SmartThings их 100 000.
const (
	stQuota     = 100000
	stUsageWarn = 90000
)

// tvView - связь с телевизором на странице прибора.
type tvView struct {
	gateway.TVLink
	// Samsung - телевизор Samsung: громкость - по UPnP.
	Samsung bool
	// CertText и NewCertText - отпечатки сертификата группами по два знака.
	CertText, NewCertText string
}

func (s *Server) tvView(id string, source string) *tvView {
	t, ok := s.gw.Television(id)
	if !ok {
		return nil
	}
	l := t.Link()
	return &tvView{TVLink: l, Samsung: source == config.SourceSamsungTV, CertText: groups(l.Cert), NewCertText: groups(l.NewCert)}
}

// tvRetry подключается к телевизору заново сразу.
func (s *Server) tvRetry(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	t, ok := s.gw.Television(id)
	if !ok {
		s.notFound(w, sess)
		return
	}
	t.Retry()
	s.done(w, r, sess, "/devices/"+id+"#tv", "Шлюз подключается к телевизору. Если телевизор спросит разрешение, нажмите «Разрешить».")
}

// tvForgetPage спрашивает подтверждение: без ключа телевизор снова спросит разрешение.
func (s *Server) tvForgetPage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	if _, ok := s.gw.Television(id); !ok {
		s.notFound(w, sess)
		return
	}
	cfg, _ := s.file.Current()
	s.render(w, http.StatusOK, "confirm", s.newPage(sess, "devices", "Ключ телевизора", confirmView{
		Title:  "Забыть ключ телевизора «" + cfg.Devices[id].Name + "»?",
		Text:   "При следующем подключении телевизор снова спросит разрешение на экране. Пока разрешения нет, Алиса телевизором не управляет.",
		Action: "/devices/" + id + "/tv/forget", Button: "Забыть ключ", Back: "/devices/" + id + "#danger"}))
}

// tvForget забывает ключ телевизора: при следующем подключении он снова спросит разрешение.
func (s *Server) tvForget(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	t, ok := s.gw.Television(id)
	if !ok {
		s.notFound(w, sess)
		return
	}
	if err := t.Forget(); err != nil {
		s.log.Error("ключ телевизора не удалён", "device", id, "err", err)
		s.failed(w, r, sess, "/devices/"+id+"#danger", "Ключ телевизора не удалён, подробности в журнале шлюза.")
		return
	}
	s.log.Info("ключ телевизора удалён", "device", id)
	s.done(w, r, sess, "/devices/"+id+"#danger", "Ключ телевизора удалён. При следующем подключении телевизор спросит разрешение.")
}

// tvCertPage спрашивает подтверждение: новый сертификат получит ключ телевизора.
func (s *Server) tvCertPage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	t, ok := s.gw.Television(id)
	if !ok {
		s.notFound(w, sess)
		return
	}
	l := t.Link()
	if l.NewCert == "" {
		s.failed(w, r, sess, "/devices/"+id+"#tv", "Сертификат телевизора не менялся.")
		return
	}
	cfg, _ := s.file.Current()
	s.render(w, http.StatusOK, "confirm", s.newPage(sess, "devices", "Сертификат телевизора", confirmView{
		Title: "Принять новый сертификат телевизора «" + cfg.Devices[id].Name + "»?",
		Text: "Новый отпечаток: " + groups(l.NewCert) + ". Сертификат меняется после обновления прошивки или сброса телевизора. " +
			"Если вы этого не делали, кто-то в домашней сети может подменять телевизор, и после подтверждения он получит ключ.",
		Action: "/devices/" + id + "/tv/cert", Button: "Принять сертификат", Back: "/devices/" + id + "#tv",
		Fields: []kv{{"cert", l.NewCert}}}))
}

// tvCert запоминает новый сертификат телевизора. Подойдёт только тот, что показан на странице.
func (s *Server) tvCert(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	t, ok := s.gw.Television(id)
	if !ok {
		s.notFound(w, sess)
		return
	}
	to := "/devices/" + id + "#tv"
	if err := t.AcceptCert(r.PostForm.Get("cert")); err != nil {
		s.log.Warn("сертификат телевизора не принят", "device", id, "err", err)
		s.failed(w, r, sess, to, "Отпечаток изменился или не сохранён: откройте страницу заново и сверьте его.")
		return
	}
	s.log.Info("новый сертификат телевизора принят", "device", id)
	s.done(w, r, sess, to, "Сертификат принят: шлюз подключается к телевизору.")
}

// stView - SmartThings на странице «Подключения».
type stView struct {
	Configured, LoggedIn, Connected bool
	ClientID                        string
	SecretSet                       bool
	// Redirect - адрес возврата для приложения API Access.
	Redirect string
	// LoginURL - страница входа SmartThings после «Войти»; пусто - вход не начат.
	LoginURL string
	Err      string
	Usage    smartthings.Usage
	Quota    int
	Warn     bool
	Devices  []stRow
}

// stRow - компонент устройства аккаунта и прибор настроек, если он уже добавлен.
type stRow struct {
	ID, Name, Type, Component, Device string
}

func (s *Server) stView(cfg *config.Config, r *http.Request, sess *session) stView {
	st := s.gw.SmartThings
	v := stView{Configured: cfg.SmartThings.Ready(), LoggedIn: st.API.LoggedIn(), Connected: st.Connected(),
		ClientID: cfg.SmartThings.ClientID, SecretSet: cfg.SmartThings.ClientSecret != "", Redirect: stRedirect(r),
		Err: st.LastError(), Usage: st.API.Usage(), Quota: stQuota}
	v.Warn = v.Usage.Calls >= stUsageWarn
	if sess != nil && v.Configured && !v.LoggedIn {
		if u, ok := s.sessions.smartThingsLogin(sess); ok {
			v.LoginURL = u
		}
	}
	used := map[string]string{}
	for id, d := range cfg.Devices {
		if d.Source == config.SourceSmartThings {
			comp := d.Location
			if comp == "" {
				comp = "main"
			}
			used[d.DeviceID+"/"+comp] = id
		}
	}
	for _, d := range st.Account() {
		for _, part := range d.Parts() {
			name := d.DisplayName()
			if part != "main" {
				name += " - " + part
			}
			v.Devices = append(v.Devices, stRow{ID: d.ID, Name: name, Type: typeName(d.Type(part)), Component: part,
				Device: used[d.ID+"/"+part]})
		}
	}
	return v
}

// stRedirect - адрес возврата после входа в SmartThings: https и адрес, по которому
// владелец открыл страницу.
func stRedirect(r *http.Request) string {
	return "https://" + gatewayHost(r) + stCallbackPath
}

func (s *Server) saveSmartThings(w http.ResponseWriter, r *http.Request, sess *session) {
	f := r.PostForm
	err := s.change(f.Get("version"), "smartthings", func(c *config.Config) error {
		c.SmartThings.ClientID = strings.TrimSpace(f.Get("client_id"))
		switch secret := strings.TrimSpace(f.Get("client_secret")); {
		case f.Get("client_secret_clear") != "":
			c.SmartThings.ClientSecret = ""
		case secret != "":
			c.SmartThings.ClientSecret = secret
		}
		return nil
	})
	if err != nil {
		s.failed(w, r, sess, "/connections#smartthings", s.explain("smartthings", err))
		return
	}
	s.done(w, r, sess, "/connections#smartthings", "Настройки SmartThings сохранены.")
}

// stLogin начинает вход в SmartThings: страница «Подключения» показывает ссылку на страницу
// входа. Перейти по ней - ссылкой, а не перенаправлением формы: форма может вести только на
// сам шлюз.
func (s *Server) stLogin(w http.ResponseWriter, r *http.Request, sess *session) {
	u, err := s.gw.SmartThings.BeginLogin(stRedirect(r))
	if err != nil {
		s.failed(w, r, sess, "/connections#smartthings", "Вход не начат: задайте идентификатор и секрет приложения SmartThings.")
		return
	}
	s.sessions.setSmartThings(sess, u, time.Now().Add(10*time.Minute))
	s.log.Info("SmartThings: вход начат", "redirect", stRedirect(r))
	http.Redirect(w, r, "/connections#smartthings", http.StatusSeeOther)
}

// stCallback - возврат со страницы входа SmartThings. Cookie сессии сюда не приходит:
// браузер не отправляет cookie с SameSite=Strict после перехода с чужого сайта. Поэтому вход
// подтверждает state, который шлюз выдал вошедшему владельцу, а неизвестный state - 404.
func (s *Server) stCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, code := q.Get("state"), q.Get("code")
	if len(state) > 64 || len(code) > 1024 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := s.gw.SmartThings.FinishLogin(ctx, state, code)
	if errors.Is(err, smartthings.ErrUnknownState) {
		http.NotFound(w, r)
		return
	}
	text := "Вход в SmartThings выполнен. Вернитесь на страницу «Подключения» и обновите список устройств."
	if err != nil {
		s.log.Warn("SmartThings: вход не выполнен", "err", err)
		text = "Вход в SmartThings не выполнен, подробности в журнале шлюза."
	} else {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := s.gw.SmartThings.RefreshAccount(ctx); err != nil {
				s.log.Warn("SmartThings: список устройств не загружен", "err", err)
			}
		}()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := fmt.Sprintf(`<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">`+
		`<title>SmartThings</title></head><body><p>%s</p><p><a href="/connections#smartthings">Вернуться к подключениям</a></p></body></html>`,
		template.HTMLEscapeString(text))
	_, _ = w.Write(i18n.HTML(langOf(w), []byte(page)))
}

func (s *Server) stLogout(w http.ResponseWriter, r *http.Request, sess *session) {
	if err := s.gw.SmartThings.Logout(); err != nil {
		s.log.Error("SmartThings: токены не удалены", "err", err)
		s.failed(w, r, sess, "/connections#smartthings", "Не удалось удалить токены, подробности в журнале шлюза.")
		return
	}
	s.log.Info("SmartThings: выход выполнен")
	s.done(w, r, sess, "/connections#smartthings", "Выход из SmartThings выполнен.")
}

func (s *Server) stDevices(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.gw.SmartThings.RefreshAccount(ctx); err != nil {
		s.log.Warn("SmartThings: список устройств не загружен", "err", err)
		s.failed(w, r, sess, "/connections#smartthings", "Список устройств SmartThings не загружен, подробности в журнале шлюза.")
		return
	}
	s.done(w, r, sess, "/connections#smartthings", "Список устройств SmartThings обновлён.")
}

// stChoices - устройства аккаунта SmartThings для выбора. Устройство с компонентами остаётся в
// списке: каждый компонент добавляют отдельно.
func (s *Server) stChoices(cur string) []choice {
	var list []string
	labels := map[string]string{}
	for _, d := range s.gw.SmartThings.Account() {
		list = append(list, d.ID)
		labels[d.ID] = d.DisplayName() + ", " + typeName(d.Type("main"))
	}
	return choices(list, cur, blankIf(cur == "", "-"), labels)
}

// stFill - форма добавления, заполненная ссылкой «Добавить» со страницы «Подключения».
func (s *Server) stFill(id, component string) config.Device {
	form := config.Device{Source: config.SourceSmartThings, DeviceID: id, Location: component}
	if component == "main" {
		form.Location = ""
	}
	for _, d := range s.gw.SmartThings.Account() {
		if d.ID == id {
			form.Name = d.DisplayName()
			if form.Location != "" {
				form.Name += " - " + form.Location
			}
		}
	}
	return form
}

// stUsageText - расход запросов за месяц словами.
func stUsageText(u smartthings.Usage) string {
	return strconv.Itoa(u.Calls) + " из " + strconv.Itoa(stQuota) + " за " + u.Month
}

// tvSource сообщает, что источник - телевизор в домашней сети.
func tvSource(source string) bool {
	return slices.Contains([]string{config.SourceWebOS, config.SourceSamsungTV}, source)
}
