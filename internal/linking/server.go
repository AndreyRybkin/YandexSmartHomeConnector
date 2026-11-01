package linking

import (
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/auth"
	"hcgateway/internal/metrics"
)

const (
	// codeTTL - срок жизни кода авторизации.
	codeTTL = 5 * time.Minute
	// maxCodes - предел числа неиспользованных кодов.
	maxCodes = 10
	// maxFormSize - предел тела запросов привязки.
	maxFormSize = 16 << 10
	// maxParamLen - предел длины state и redirect_uri.
	maxParamLen = 1024
)

// Config - настройки привязки. Меняются без перезапуска через SetConfig.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURIs []string
}

// Login проверяет логин и пароль владельца: тот же пользователь и тот же бан, что у веб-интерфейса.
type Login interface {
	Login(r *http.Request, where, name, password string) (auth.Result, auth.User)
}

type code struct {
	hash     string
	redirect string
	expires  time.Time
}

// Server обслуживает /auth/authorize и /auth/token.
type Server struct {
	login Login
	store *Store
	log   *slog.Logger
	now   func() time.Time

	mu    sync.Mutex
	cfg   Config
	codes []code
}

// NewServer создаёт обработчики привязки.
func NewServer(cfg Config, login Login, store *Store, log *slog.Logger) *Server {
	return &Server{cfg: cfg, login: login, store: store, log: log, now: time.Now}
}

// SetConfig меняет настройки привязки без перезапуска.
func (s *Server) SetConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

func (s *Server) config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Register добавляет обработчики в mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /auth/authorize", metrics.YandexHandler("authorize", http.HandlerFunc(s.authorizePage)))
	mux.Handle("POST /auth/authorize", metrics.YandexHandler("authorize", http.HandlerFunc(s.authorizeSubmit)))
	mux.Handle("POST /auth/token", metrics.YandexHandler("token", http.HandlerFunc(s.token)))
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Привязка шлюза</title>
<style>
body{font-family:system-ui,sans-serif;max-width:360px;margin:48px auto;padding:0 16px;color:#222;background:#fff}
input,button{font:inherit;width:100%;box-sizing:border-box;padding:10px;margin-top:8px}
.err{color:#b00020}
</style></head><body>
<h1>Шлюз умного дома для Алисы</h1>
<p>Войдите тем же логином и паролем, что и в веб-интерфейс шлюза, чтобы Алиса могла управлять приборами.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/auth/authorize">
<input type="hidden" name="response_type" value="code">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="state" value="{{.State}}">
<input name="username" autocomplete="username" placeholder="Логин" required autofocus>
<input type="password" name="password" autocomplete="current-password" placeholder="Пароль" required>
<button type="submit">Привязать</button>
</form></body></html>`))

type pageData struct {
	ClientID, RedirectURI, State, Error string
}

// request - проверенные параметры запроса авторизации.
func (s *Server) request(v url.Values) (pageData, bool) {
	cfg := s.config()
	d := pageData{ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), State: v.Get("state")}
	ok := v.Get("response_type") == "code" &&
		equal(d.ClientID, cfg.ClientID) &&
		len(d.RedirectURI) <= maxParamLen && slices.Contains(cfg.RedirectURIs, d.RedirectURI) &&
		len(d.State) <= maxParamLen && origin(d.RedirectURI) != ""
	return d, ok
}

// originHostRe - хост адреса возврата, который можно вписать в заголовок CSP: имя или IP,
// порт по желанию, без пробелов, кавычек и точки с запятой.
var originHostRe = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]{2,45}\]|[A-Za-z0-9.-]{1,253})(:[0-9]{1,5})?$`)

// formOrigins - сайты всех адресов возврата из настроек для form-action: браузер применяет
// его ко всей цепочке перенаправлений после формы, а Яндекс ведёт её через несколько своих
// сайтов, например social.yandex.net и social.yandex.ru. Адреса, которые нельзя вписать
// в заголовок, пропускаются.
func formOrigins(redirects []string) []string {
	var out []string
	for _, r := range redirects {
		if o := origin(r); o != "" && !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	slices.Sort(out)
	return out
}

// origin - схема и хост адреса возврата для form-action: после верного пароля форма
// перенаправляет туда, а браузер применяет form-action и к перенаправлению. Пусто -
// адрес не годится.
func origin(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !originHostRe.MatchString(u.Host) {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (s *Server) render(w http.ResponseWriter, status int, d pageData) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' "+
		strings.Join(formOrigins(s.config().RedirectURIs), " ")+"; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	if err := page.Execute(w, d); err != nil {
		s.log.Error("страница привязки", "err", err)
	}
}

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	d, ok := s.request(r.URL.Query())
	if !ok {
		// На чужой redirect_uri не перенаправляем: ссылку мог подделать кто угодно.
		http.Error(w, "Неверный запрос", http.StatusBadRequest)
		return
	}
	s.render(w, http.StatusOK, d)
}

func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Неверный запрос", http.StatusBadRequest)
		return
	}
	d, ok := s.request(r.PostForm)
	if !ok {
		http.Error(w, "Неверный запрос", http.StatusBadRequest)
		return
	}
	switch res, _ := s.login.Login(r, auth.WhereLinking, r.PostForm.Get("username"), r.PostForm.Get("password")); res {
	case auth.OK:
	case auth.Banned:
		d.Error = "Слишком много попыток, попробуйте позже"
		s.render(w, http.StatusTooManyRequests, d)
		return
	case auth.Busy:
		d.Error = "Сервер занят, попробуйте позже"
		s.render(w, http.StatusServiceUnavailable, d)
		return
	default:
		d.Error = "Неверный логин или пароль"
		s.render(w, http.StatusUnauthorized, d)
		return
	}
	c, err := s.newCode(d.RedirectURI)
	if err != nil {
		s.log.Error("код авторизации", "err", err)
		http.Error(w, "Внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	target, _ := url.Parse(d.RedirectURI)
	q := target.Query()
	q.Set("code", c)
	q.Set("state", d.State)
	q.Set("client_id", d.ClientID)
	target.RawQuery = q.Encode()
	s.log.Info("аккаунт Яндекса привязывается")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (s *Server) newCode(redirect string) (string, error) {
	c, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.codes = slices.DeleteFunc(s.codes, func(c code) bool { return !now.Before(c.expires) })
	if len(s.codes) >= maxCodes {
		s.codes = s.codes[1:]
	}
	s.codes = append(s.codes, code{hash: hash(c), redirect: redirect, expires: now.Add(codeTTL)})
	return c, nil
}

// takeCode проверяет код и удаляет его: код одноразовый.
func (s *Server) takeCode(c, redirect string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := []byte(hash(c))
	found := -1
	for i, e := range s.codes {
		if subtle.ConstantTimeCompare([]byte(e.hash), h) == 1 {
			found = i
		}
	}
	if found < 0 {
		return false
	}
	e := s.codes[found]
	s.codes = slices.Delete(s.codes, found, found+1)
	return s.now().Before(e.expires) && (redirect == "" || redirect == e.redirect)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	// И client_id, и client_secret сравниваются всегда, чтобы время ответа не зависело от того, какой неверен.
	// Бана здесь нет: запрос приходит с серверов Яндекса, и ошибка в консоли навыка забанила бы Яндекс.
	cfg := s.config()
	idOK := equal(id, cfg.ClientID)
	secretOK := equal(secret, cfg.ClientSecret)
	if !idOK || !secretOK {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var (
		t   Tokens
		err error
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if !s.takeCode(r.PostForm.Get("code"), r.PostForm.Get("redirect_uri")) {
			oauthError(w, "invalid_grant")
			return
		}
		t, err = s.store.Issue()
		if err == nil {
			s.log.Info("аккаунт Яндекса привязан")
		}
	case "refresh_token":
		var found bool
		t, found, err = s.store.Refresh(r.PostForm.Get("refresh_token"))
		if err == nil && !found {
			oauthError(w, "invalid_grant")
			return
		}
	default:
		oauthError(w, "unsupported_grant_type")
		return
	}
	if err != nil {
		s.log.Error("выдача токенов", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":  t.Access,
		"token_type":    "bearer",
		"expires_in":    int(AccessTTL.Seconds()),
		"refresh_token": t.Refresh,
	})
}

func oauthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// equal сравнивает строки за постоянное время. Пустое ожидаемое значение ничему не равно.
func equal(got, want string) bool {
	if want == "" {
		return false
	}
	a, b := sha256Sum(got), sha256Sum(want)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
