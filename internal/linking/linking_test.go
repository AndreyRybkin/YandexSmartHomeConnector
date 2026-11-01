package linking

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/auth"
)

const (
	testRedirect = "https://social.yandex.net/broker/redirect"
	testSecret   = "0123456789abcdef"
	testPassword = "fedcba9876543210"
)

type fixture struct {
	srv    *Server
	store  *Store
	fake   *fakeLogin
	client *http.Client
	url    string
	now    time.Time
}

// fakeLogin - вход без argon2: логин owner и пароль testPassword. forced - ответ на любую попытку.
type fakeLogin struct {
	forced *auth.Result
	where  string
}

func (f *fakeLogin) Login(r *http.Request, where, name, password string) (auth.Result, auth.User) {
	f.where = where
	switch {
	case f.forced != nil:
		return *f.forced, auth.User{}
	case name == "owner" && password == testPassword:
		return auth.OK, auth.User{Name: name}
	}
	return auth.Wrong, auth.User{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, fake: &fakeLogin{}, now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	store.now = clock
	f.srv = NewServer(Config{ClientID: "yandex", ClientSecret: testSecret, RedirectURIs: []string{testRedirect}},
		f.fake, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.srv.now = clock
	mux := http.NewServeMux()
	f.srv.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	f.url = hs.URL
	f.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

func authForm(password string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {"yandex"}, "redirect_uri": {testRedirect},
		"state": {"st-1"}, "username": {"owner"}, "password": {password}}
}

// login вводит пароль и возвращает код из перенаправления.
func (f *fixture) login(t *testing.T) string {
	t.Helper()
	resp, err := f.client.PostForm(f.url+"/auth/authorize", authForm(testPassword))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("вход: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), testRedirect) || loc.Query().Get("state") != "st-1" {
		t.Fatalf("перенаправление %s", loc)
	}
	return loc.Query().Get("code")
}

func (f *fixture) token(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	form.Set("client_id", "yandex")
	if !form.Has("client_secret") {
		form.Set("client_secret", testSecret)
	}
	resp, err := f.client.PostForm(f.url+"/auth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLinkingFlow(t *testing.T) {
	f := newFixture(t)
	resp, err := f.client.Get(f.url + "/auth/authorize?" + authForm("").Encode())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `name="password"`) || !strings.Contains(string(body), `name="username"`) {
		t.Fatalf("страница входа: %d", resp.StatusCode)
	}
	// Браузер применяет form-action и к перенаправлению после формы: адрес Яндекса разрешён.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://social.yandex.net;") &&
		!strings.Contains(csp, "form-action 'self' https://social.yandex.net https://social.yandex.ru;") {
		t.Fatalf("CSP: %s", csp)
	}

	code := f.login(t)
	status, tok := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect}})
	if status != http.StatusOK || tok["token_type"] != "bearer" || tok["expires_in"] != float64(86400) {
		t.Fatalf("токен: %d %v", status, tok)
	}
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	if !f.store.Valid(access) || f.store.Valid(refresh) || !f.store.Linked() {
		t.Fatal("access token должен действовать, refresh token - нет")
	}
	// Код одноразовый.
	if status, out := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}}); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("повторный код: %d %v", status, out)
	}

	// Обновление выдаёт новую пару, старый refresh token больше не действует.
	status, tok2 := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != http.StatusOK || tok2["refresh_token"] == refresh {
		t.Fatalf("обновление: %d %v", status, tok2)
	}
	if status, _ := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); status != http.StatusBadRequest {
		t.Fatalf("старый refresh token: %d", status)
	}

	// Access token истекает через сутки.
	f.now = f.now.Add(AccessTTL)
	if f.store.Valid(tok2["access_token"].(string)) {
		t.Fatal("access token после срока")
	}

	// Токены переживают перезапуск, на диске только хэши.
	data, _ := os.ReadFile(f.store.path)
	if strings.Contains(string(data), tok2["refresh_token"].(string)) {
		t.Fatal("на диске лежит сам токен")
	}
	again, err := OpenStore(f.store.path)
	if err != nil || !again.Linked() {
		t.Fatalf("после перезапуска: %v", err)
	}
	if err := again.RevokeAll(); err != nil || again.Linked() {
		t.Fatalf("отзыв: %v", err)
	}
}

func TestLinkingRejects(t *testing.T) {
	f := newFixture(t)
	resp, _ := f.client.PostForm(f.url+"/auth/authorize", authForm("wrong-password-00"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("неверный пароль: %d", resp.StatusCode)
	}

	bad := authForm(testPassword)
	bad.Set("redirect_uri", "https://evil.example/cb")
	resp, _ = f.client.Get(f.url + "/auth/authorize?" + bad.Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("чужой redirect_uri: %d", resp.StatusCode)
	}

	code := f.login(t)
	status, out := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_secret": {"wrong"}})
	if status != http.StatusUnauthorized || out != nil {
		t.Fatalf("неверный client_secret: %d %v", status, out)
	}
	status, _ = f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://other"}})
	if status != http.StatusBadRequest {
		t.Fatalf("другой redirect_uri: %d", status)
	}
	if status, out := f.token(t, url.Values{"grant_type": {"password"}}); status != http.StatusBadRequest || out["error"] != "unsupported_grant_type" {
		t.Fatalf("grant_type: %d %v", status, out)
	}

	// Код истекает через 5 минут.
	code = f.login(t)
	f.now = f.now.Add(codeTTL)
	if status, _ := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}}); status != http.StatusBadRequest {
		t.Fatalf("истёкший код: %d", status)
	}
}

func TestLinkingLoginResults(t *testing.T) {
	f := newFixture(t)
	f.fake.forced = new(auth.Result)
	cases := map[auth.Result]struct {
		status int
		text   string
	}{
		auth.Wrong:  {http.StatusUnauthorized, "Неверный логин или пароль"},
		auth.Banned: {http.StatusTooManyRequests, "Слишком много попыток, попробуйте позже"},
		auth.Busy:   {http.StatusServiceUnavailable, "Сервер занят, попробуйте позже"},
	}
	for res, want := range cases {
		*f.fake.forced = res
		resp, err := f.client.PostForm(f.url+"/auth/authorize", authForm(testPassword))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want.status || !strings.Contains(string(body), want.text) || resp.Header.Get("Location") != "" {
			t.Errorf("%v: %d %s", res, resp.StatusCode, body)
		}
	}
	if f.fake.where != auth.WhereLinking {
		t.Fatalf("откуда вход: %q", f.fake.where)
	}
}

func TestLinkingSetConfig(t *testing.T) {
	f := newFixture(t)
	f.srv.SetConfig(Config{ClientID: "yandex-2", ClientSecret: testSecret + "-2", RedirectURIs: []string{testRedirect}})
	resp, _ := f.client.Get(f.url + "/auth/authorize?" + authForm("").Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("прежний client_id: %d", resp.StatusCode)
	}
	if status, _ := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {"x"}}); status != http.StatusUnauthorized {
		t.Fatalf("прежний client_secret: %d", status)
	}
}

func TestOrigin(t *testing.T) {
	for in, want := range map[string]string{
		testRedirect:                        "https://social.yandex.net",
		"http://127.0.0.1:8080/cb?x=1":      "http://127.0.0.1:8080",
		"https://evil.example;script-src *": "",
		"https://a b.example/":              "",
		"javascript:alert(1)":               "",
		"https://user@social.yandex.net/x":  "https://social.yandex.net",
	} {
		if got := origin(in); got != want {
			t.Errorf("%q: %q, ожидалось %q", in, got, want)
		}
	}
}

func TestFormOrigins(t *testing.T) {
	got := formOrigins([]string{"https://social.yandex.ru/broker/redirect", testRedirect, "https://social.yandex.net/other",
		"https://evil.example;script-src *", "javascript:alert(1)"})
	if strings.Join(got, " ") != "https://social.yandex.net https://social.yandex.ru" {
		t.Fatalf("сайты: %v", got)
	}
}
