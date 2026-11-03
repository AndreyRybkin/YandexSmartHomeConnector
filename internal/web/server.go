// Package web - веб-интерфейс шлюза: приборы, сопоставления, подключения и баны.
// Страницы собирает сервер и встраивает в программу. Свой скрипт только улучшает
// страницы: без него они работают так же.
package web

import (
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/auth"
	"hcgateway/internal/config"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/gateway"
	"hcgateway/internal/metrics"
)

// maxForm - предел тела формы.
const maxForm = 64 << 10

//go:embed templates/*.html
var templateFiles embed.FS

// staticFiles - скрипт интерфейса, значки и описание приложения для установки на телефон.
//
//go:embed static
var staticFiles embed.FS

// staticTypes - файлы из static, которые шлюз отдаёт, и их типы.
var staticTypes = map[string]string{
	"app.js":               "text/javascript; charset=utf-8",
	"manifest.webmanifest": "application/manifest+json",
	"icon.svg":             "image/svg+xml",
	"icon-192.png":         "image/png",
	"icon-512.png":         "image/png",
	"apple-touch-icon.png": "image/png",
}

// Server - веб-интерфейс.
type Server struct {
	auth *auth.Auth
	gw   *gateway.Gateway
	file *config.File
	// start - настройки при запуске: data_dir, адреса и trusted_proxies действуют только из них.
	start    *config.Config
	log      *slog.Logger
	sessions *sessions
	pages    map[string]*template.Template
	mux      *http.ServeMux

	// mu - правки настроек по одной: проверка, сохранение и применение не перемешиваются.
	mu sync.Mutex

	// moving - переводы приборов по одному: у прибора и облака есть пределы соединений и запросов.
	moving sync.Mutex

	// statsMu и stats - рецепты кофемашин из облака по идентификатору прибора; в памяти.
	statsMu sync.Mutex
	stats   map[string][]delonghi.RecipeStat
}

// New создаёт веб-интерфейс. start - настройки, с которыми запущен шлюз.
func New(a *auth.Auth, gw *gateway.Gateway, file *config.File, start *config.Config, log *slog.Logger) *Server {
	s := &Server{auth: a, gw: gw, file: file, start: start, log: log, sessions: newSessions(), pages: parsePages()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.authed(s.logout))
	mux.HandleFunc("POST /reload", s.authed(s.reload))
	mux.HandleFunc("GET /{$}", s.authed(s.devicesPage))
	mux.HandleFunc("POST /devices", s.authed(s.addDevice))
	mux.HandleFunc("GET /devices/{id}", s.authed(s.devicePage))
	mux.HandleFunc("POST /devices/{id}", s.authed(s.saveDevice))
	mux.HandleFunc("GET /devices/{id}/delete", s.authed(s.deletePage))
	mux.HandleFunc("POST /devices/{id}/delete", s.authed(s.deleteDevice))
	mux.HandleFunc("POST /devices/{id}/command", s.authed(s.command))
	mux.HandleFunc("POST /devices/{id}/profile/{field}", s.authed(s.saveDeviceProfile))
	mux.HandleFunc("POST /devices/{id}/presets", s.authed(s.saveStripPresets))
	mux.HandleFunc("GET /devices/{id}/move", s.authed(s.movePage))
	mux.HandleFunc("POST /devices/{id}/move", s.authed(s.moveDevice))
	mux.HandleFunc("GET /profiles", s.authed(s.profilesPage))
	mux.HandleFunc("GET /profiles/{type}", s.authed(s.typePage))
	mux.HandleFunc("POST /profiles/{type}/{field}", s.authed(s.saveTypeProfile))
	mux.HandleFunc("GET /stream", s.authed(s.listStream))
	mux.HandleFunc("GET /connections", s.authed(s.connectionsPage))
	mux.HandleFunc("GET /static/{name}", serveStatic)
	mux.HandleFunc("POST /connections/yandex", s.authed(s.saveYandex))
	mux.HandleFunc("GET /connections/yandex/unlink", s.authed(s.confirmPage("yandex/unlink")))
	mux.HandleFunc("POST /connections/yandex/unlink", s.authed(s.unlink))
	mux.HandleFunc("POST /connections/metrics", s.authed(s.saveMetrics))
	mux.HandleFunc("POST /connections/homeconnect", s.authed(s.saveHomeConnect))
	mux.HandleFunc("POST /connections/homeconnect/login", s.authed(s.homeConnectLogin))
	mux.HandleFunc("GET /connections/homeconnect/logout", s.authed(s.confirmPage("homeconnect/logout")))
	mux.HandleFunc("POST /connections/homeconnect/logout", s.authed(s.homeConnectLogout))
	mux.HandleFunc("POST /connections/homeconnect/appliances", s.authed(s.homeConnectAppliances))
	mux.HandleFunc("POST /connections/local/token", s.authed(s.keysToken))
	mux.HandleFunc("POST /connections/local/update", s.authed(s.keysUpdate))
	mux.HandleFunc("POST /connections/local/refresh", s.authed(s.keysRefreshNow))
	mux.HandleFunc("GET /connections/local/logout", s.authed(s.confirmPage("local/logout")))
	mux.HandleFunc("POST /connections/local/logout", s.authed(s.keysLogout))
	mux.HandleFunc("POST /connections/local/login", s.authed(s.keysLogin))
	mux.HandleFunc("POST /connections/local/keys", s.authed(s.keysFinish))
	mux.HandleFunc("POST /connections/thinq", s.authed(s.saveThinQ))
	mux.HandleFunc("POST /connections/thinq/devices", s.authed(s.thinqDevices))
	mux.HandleFunc("POST /connections/delonghi/login", s.authed(s.coffeeLogin))
	mux.HandleFunc("GET /connections/delonghi/logout", s.authed(s.confirmPage("delonghi/logout")))
	mux.HandleFunc("POST /connections/delonghi/logout", s.authed(s.coffeeLogout))
	mux.HandleFunc("POST /connections/delonghi/devices", s.authed(s.coffeeDevices))
	mux.HandleFunc("POST /connections/smartthings", s.authed(s.saveSmartThings))
	mux.HandleFunc("POST /connections/smartthings/login", s.authed(s.stLogin))
	// Возврат со страницы входа SmartThings - без сессии: его подтверждает state входа.
	mux.HandleFunc("GET "+stCallbackPath, s.stCallback)
	mux.HandleFunc("GET /connections/smartthings/logout", s.authed(s.confirmPage("smartthings/logout")))
	mux.HandleFunc("POST /connections/smartthings/logout", s.authed(s.stLogout))
	mux.HandleFunc("POST /connections/smartthings/devices", s.authed(s.stDevices))
	mux.HandleFunc("POST /connections/discovery", s.authed(s.discover))
	mux.HandleFunc("POST /connections/discovery/move", s.authed(s.moveFound))
	mux.HandleFunc("POST /devices/{id}/tv/retry", s.authed(s.tvRetry))
	mux.HandleFunc("GET /devices/{id}/tv/forget", s.authed(s.tvForgetPage))
	mux.HandleFunc("POST /devices/{id}/tv/forget", s.authed(s.tvForget))
	mux.HandleFunc("GET /devices/{id}/tv/cert", s.authed(s.tvCertPage))
	mux.HandleFunc("POST /devices/{id}/tv/cert", s.authed(s.tvCert))
	mux.HandleFunc("POST /devices/{id}/pin", s.authed(s.confirmPin))
	mux.HandleFunc("POST /devices/{id}/recipes", s.authed(s.loadRecipes))
	mux.HandleFunc("POST /devices/{id}/probe", s.authed(s.probe))
	mux.HandleFunc("GET /devices/{id}/info", s.authed(s.infoPage))
	mux.HandleFunc("GET /devices/{id}/stream", s.authed(s.stream))
	mux.HandleFunc("POST /theme", s.authed(s.setTheme))
	mux.HandleFunc("GET /logins", s.authed(s.loginsPage))
	mux.HandleFunc("POST /logins/lift", s.authed(s.lift))
	s.mux = mux
	return s
}

// Handler - один HTTP-сервер шлюза: запросы Яндекса /v1.0 и /auth обслуживает yandex, всё
// остальное - веб-интерфейс. Старые адреса интерфейса /ui/... ведут на те же адреса без /ui.
func Handler(ui *Server, yandex http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1.0", yandex)
	mux.Handle("/v1.0/", yandex)
	mux.Handle("/auth/", yandex)
	mux.HandleFunc("/ui", moved)
	mux.HandleFunc("/ui/", moved)
	mux.Handle("/", ui)
	return mux
}

// moved ведёт старый адрес интерфейса на тот же адрес без /ui: так работают закладки и
// значок на телефоне. Форма, отправленная со старой страницы, уходит на новый адрес как есть.
func moved(w http.ResponseWriter, r *http.Request) {
	to := strings.TrimPrefix(r.URL.EscapedPath(), "/ui")
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.Contains(to, "\\") {
		to = "/"
	}
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	code := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		code = http.StatusMovedPermanently
	}
	http.Redirect(w, r, to, code)
}

// yandexPath сообщает, что адрес - запрос Яндекса, а не страница интерфейса.
func yandexPath(p string) bool {
	return p == "/v1.0" || strings.HasPrefix(p, "/v1.0/") || strings.HasPrefix(p, "/auth/")
}

// ServeHTTP добавляет к каждому ответу заголовки защиты и ограничивает тело формы.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// Скрипты - только свой файл /static/app.js, без встроенных; соединения, картинки и
	// описание приложения - только с шлюза.
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; "+
		"img-src 'self'; manifest-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	}
	back := "/"
	if r.Method == http.MethodGet {
		back = r.URL.RequestURI()
	}
	s.mux.ServeHTTP(&themed{ResponseWriter: w, theme: themeOf(r), back: back}, r)
}

// themeCookie - тема интерфейса в этом браузере: light или dark; без cookie - как в системе.
const themeCookie = "hcgateway-theme"

// themeAge - сколько браузер помнит тему.
const themeAge = 365 * 24 * time.Hour

// themed - ответ вместе с темой из cookie и адресом страницы. render ставит тему в разметку,
// и страница не мигает другой темой при загрузке; форма темы возвращает на эту страницу.
type themed struct {
	http.ResponseWriter
	theme, back string
}

// Unwrap нужен http.ResponseController: поток SSE сбрасывает исходный ответ и продлевает
// срок его записи.
func (t *themed) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return ""
}

// setTheme запоминает тему в cookie браузера и возвращает на страницу, где её выбрали. Со
// скриптом cookie ставит сам скрипт, а форма нужна без него.
func (s *Server) setTheme(w http.ResponseWriter, r *http.Request, _ *session) {
	c := &http.Cookie{Name: themeCookie, Path: "/", MaxAge: int(themeAge.Seconds()), Secure: true, SameSite: http.SameSiteStrictMode}
	switch v := r.PostForm.Get("theme"); v {
	case "light", "dark":
		c.Value = v
	default:
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, localPath(r.PostForm.Get("back")), http.StatusSeeOther)
}

// localPath - адрес для возврата: только страница интерфейса этого шлюза, иначе - список
// приборов. Чужой адрес превратил бы форму в перенаправление на любой сайт.
func localPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxBack || u.Scheme != "" || u.Host != "" || strings.HasPrefix(raw, "//") ||
		strings.Contains(raw, "\\") || !strings.HasPrefix(u.Path, "/") || yandexPath(u.Path) {
		return "/"
	}
	return u.RequestURI()
}

// maxBack - предел длины адреса для возврата.
const maxBack = 1024

type handler func(w http.ResponseWriter, r *http.Request, sess *session)

// authed пропускает только вошедшего владельца, а формы - только с токеном его сессии.
func (s *Server) authed(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if !parseForm(w, r) {
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.csrf)) != 1 {
				http.Error(w, "Форма устарела, откройте страницу заново", http.StatusForbidden)
				return
			}
		}
		next(w, r, sess)
	}
}

// parseForm разбирает форму. Слишком большая форма - 413, неверная - 400.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	err := r.ParseForm()
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "Форма больше 64 КБ", http.StatusRequestEntityTooLarge)
	} else {
		http.Error(w, "Неверный запрос", http.StatusBadRequest)
	}
	return false
}

// session - сессия из cookie или nil. Сессия действует, пока не сменился пользователь.
func (s *Server) session(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	u, ok := s.auth.Users.Current()
	if !ok {
		return nil
	}
	return s.sessions.find(c.Value, u)
}

// page - общие данные страницы.
type page struct {
	Title   string
	Nav     string
	User    string
	CSRF    string
	Version string
	Flash   *flash
	// Theme - тема из cookie: light, dark или пусто - как в системе; Back - адрес страницы
	// для формы темы.
	Theme, Back string
	// Notices - что в файле настроек ещё не действует; Steps - сколько шагов настройки
	// не сделано: строка о них - только в списке приборов, сам список - на «Подключениях».
	Notices []string
	Steps   int
	Data    any
	// Refresh - через сколько секунд страница обновляется сама; 0 - не обновляется. RefreshTo -
	// адрес обновления: у адреса с #раздел обновление на него же браузер считает переходом к
	// разделу и страницу не загружает, поэтому в RefreshTo каждый раз новая метка ?r=.
	Refresh   int
	RefreshTo string
}

func (s *Server) newPage(sess *session, nav, title string, data any) *page {
	cfg, version := s.file.Current()
	return &page{Title: title, Nav: nav, User: sess.user, CSRF: sess.csrf, Version: version,
		Flash: s.sessions.takeFlash(sess), Notices: s.notices(cfg), Data: data}
}

// step - шаг настройки, который ещё не сделан, и страница, где его сделать.
type step struct {
	Text, Link string
}

// steps - чего не хватает в настройках, чтобы Алиса управляла приборами.
func (s *Server) steps(cfg *config.Config) []step {
	const yandex = "/connections#yandex"
	var out []step
	switch {
	case !cfg.Yandex.Ready():
		out = append(out, step{"Не заданы идентификатор и секрет приложения: Яндекс не может привязать аккаунт.", yandex})
	case !s.gw.Tokens.Linked():
		out = append(out, step{"Аккаунт Яндекса не привязан: привяжите навык в приложении «Дом с Алисой».", yandex})
	}
	if cfg.Yandex.SkillID == "" {
		out = append(out, step{"Не заданы идентификатор навыка и токен для уведомлений: Алиса узнаёт об изменениях, " +
			"только когда сама запрашивает состояние.", yandex})
	}
	if len(cfg.Devices) == 0 {
		out = append(out, step{"Нет ни одного прибора.", "/#add"})
	}
	api := s.gw.Cloud.API
	switch {
	case api.RefreshState().Rejected:
		out = append(out, step{"Облако не продлило токен Home Connect: нужен новый вход или новый токен.", "/connections#local"})
	case !api.LoggedIn() && usesSource(cfg, config.SourceHomeConnectCloud):
		out = append(out, step{"Нет входа в Home Connect: приборы облака не на связи.", "/connections#homeconnect"})
	}
	if s.gw.StateFailing() {
		out = append(out, step{"База состояния не принимает запись: баны, связь и значения приборов лежат только в памяти. " +
			"Проверьте место на диске и права на data_dir.", "/logins"})
	}
	return out
}

// usesSource - есть ли в настройках прибор этого источника.
func usesSource(cfg *config.Config, source string) bool {
	for _, d := range cfg.Devices {
		if d.Source == source {
			return true
		}
	}
	return false
}

// notices - что в файле настроек ещё не действует.
func (s *Server) notices(cfg *config.Config) []string {
	var out []string
	if restart := s.restartNeeded(cfg); len(restart) > 0 {
		out = append(out, "В файле изменены "+strings.Join(restart, ", ")+": они действуют только после перезапуска шлюза.")
	}
	if cfg.Metrics.Token != "" && s.start.Metrics.Token == "" {
		out = append(out, "Задан metrics.token: порт метрик откроется после перезапуска шлюза.")
	}
	return out
}

// restartNeeded - параметры, которые в файле не такие, как при запуске.
func (s *Server) restartNeeded(cfg *config.Config) []string {
	var out []string
	if cfg.DataDir != s.start.DataDir {
		out = append(out, "data_dir")
	}
	if cfg.Listen != s.start.Listen {
		out = append(out, "listen")
	}
	if cfg.Metrics.Listen != s.start.Metrics.Listen {
		out = append(out, "metrics.listen")
	}
	if !slices.Equal(cfg.Proxies, s.start.Proxies) {
		out = append(out, "trusted_proxies")
	}
	return out
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p *page) {
	if t, ok := w.(*themed); ok {
		p.Theme, p.Back = t.theme, t.back
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		s.log.Error("страница веб-интерфейса", "page", name, "err", err)
	}
}

func (s *Server) notFound(w http.ResponseWriter, sess *session) {
	s.render(w, http.StatusNotFound, "notfound", s.newPage(sess, "", "Не найдено", nil))
}

// done сообщает об успехе на следующей странице и переходит на неё.
func (s *Server) done(w http.ResponseWriter, r *http.Request, sess *session, to, text string) {
	s.sessions.setFlash(sess, &flash{Text: text})
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// failed сообщает об ошибке на следующей странице и переходит на неё.
func (s *Server) failed(w http.ResponseWriter, r *http.Request, sess *session, to, text string) {
	s.sessions.setFlash(sess, &flash{Text: text, Error: true})
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// inputError - ошибка в том, что ввёл владелец: её текст показывается на странице.
type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &inputError{fmt.Sprintf(format, args...)}
}

// errNotApplied - настройки сохранены, но шлюз их не применил.
var errNotApplied = errors.New("настройки сохранены, но не применены")

// change правит копию настроек, проверяет её, сохраняет файл и применяет без перезапуска.
// version - версия, по которой открыта страница; section - что изменено, для журнала.
func (s *Server) change(version, section string, edit func(c *config.Config) error) error {
	err := s.changeLocked(version, section, edit)
	var in *inputError
	switch {
	case err == nil:
		metrics.ConfigSave(metrics.SaveOK)
	case errors.As(err, &in):
		metrics.ConfigSave(metrics.SaveInvalid)
	case errors.Is(err, config.ErrStale), errors.Is(err, config.ErrChanged):
		metrics.ConfigSave(metrics.SaveConflict)
	default:
		metrics.ConfigSave(metrics.SaveError)
	}
	return err
}

func (s *Server) changeLocked(version, section string, edit func(c *config.Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, curVersion := s.file.Current()
	if version != curVersion {
		return config.ErrStale
	}
	next := cur.Clone()
	if err := edit(next); err != nil {
		return err
	}
	if err := next.Check(); err != nil {
		return &inputError{err.Error()}
	}
	if err := s.gw.Check(next); err != nil {
		return &inputError{err.Error()}
	}
	saved, _, err := s.file.Save(next, version)
	if err != nil {
		return err
	}
	if err := s.gw.Apply(saved); err != nil {
		s.log.Error("настройки сохранены, но не применены", "section", section, "err", err)
		return errNotApplied
	}
	s.log.Info("настройки сохранены", "section", section)
	return nil
}

// explain - текст ошибки сохранения для страницы. Подробности внутренних ошибок - только в журнал.
func (s *Server) explain(section string, err error) string {
	var in *inputError
	switch {
	case errors.As(err, &in):
		return in.msg
	case errors.Is(err, config.ErrStale):
		return "Настройки изменились после того, как открылась страница. Откройте её заново и повторите правку."
	case errors.Is(err, config.ErrChanged):
		return "Файл настроек изменили вручную. Нажмите «Перечитать файл» и повторите правку."
	case errors.Is(err, errNotApplied):
		return "Настройки сохранены в файл, но не применены, подробности в журнале шлюза."
	}
	s.log.Error("сохранение настроек", "section", section, "err", err)
	return "Не удалось сохранить настройки, подробности в журнале шлюза."
}

// reload перечитывает файл настроек после ручной правки.
func (s *Server) reload(w http.ResponseWriter, r *http.Request, sess *session) {
	s.mu.Lock()
	err := s.reloadLocked()
	s.mu.Unlock()
	if err != nil {
		var pathErr *fs.PathError
		msg := err.Error()
		if errors.As(err, &pathErr) || errors.Is(err, errNotApplied) {
			s.log.Error("перечитывание настроек", "err", err)
			msg = "Не удалось прочитать файл настроек, подробности в журнале шлюза."
		}
		s.failed(w, r, sess, "/", "Файл не перечитан: "+msg)
		return
	}
	s.log.Info("настройки перечитаны из файла")
	s.done(w, r, sess, "/", "Файл настроек перечитан.")
}

func (s *Server) reloadLocked() error {
	cfg, version, err := s.file.Read()
	if err != nil {
		return err
	}
	if err := cfg.ValidateServe(); err != nil {
		return err
	}
	if err := s.gw.Check(cfg); err != nil {
		return err
	}
	if err := s.gw.Apply(cfg); err != nil {
		return fmt.Errorf("%w: %w", errNotApplied, err)
	}
	s.file.Use(cfg, version)
	return nil
}

func parsePages() map[string]*template.Template {
	base := template.Must(template.New("").Funcs(funcs).ParseFS(templateFiles, "templates/layout.html", "templates/editor.html"))
	pages := map[string]*template.Template{}
	for _, name := range []string{"login", "notfound", "devices", "device", "confirm", "profiles", "profile", "connections", "logins", "info", "move"} {
		pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFiles, "templates/"+name+".html"))
	}
	return pages
}

// serveStatic отдаёт скрипт, значки и описание приложения без входа: в них нет данных.
// Браузер запрашивает описание приложения и значки без cookie.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	typ, ok := staticTypes[name]
	data, err := staticFiles.ReadFile("static/" + name)
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", typ)
	_, _ = w.Write(data)
}
