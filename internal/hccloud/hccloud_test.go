package hccloud

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + ".sig"
}

func TestLoginURL(t *testing.T) {
	l, err := NewLogin(Global, ScopeKeys)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := l.URL()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	sum := sha256.Sum256([]byte(l.verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE: %v", q)
	}
	if q.Get("scope") != ScopeKeys {
		t.Fatalf("область: %v", q.Get("scope"))
	}
	if q.Get("client_id") != appClientID || q.Get("redirect_uri") != redirectURI || q.Get("state") != l.state {
		t.Fatalf("параметры: %v", q)
	}
}

func TestExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.FormValue("code") != "CODE" || r.FormValue("code_verifier") == "" {
			t.Errorf("запрос: %s %v", r.URL.Path, r.Form)
		}
		w.Write([]byte(`{"access_token":"` + fakeJWT(`{"sub":"user-1"}`) + `"}`))
	}))
	defer srv.Close()
	l, _ := NewLogin(Global, ScopeKeys)
	l.authBase = srv.URL + "/"
	g, err := l.Exchange(context.Background(), " hcauth://auth/prod?code=CODE&state="+l.state+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if g.Subject != "user-1" || g.Access == "" {
		t.Fatalf("вход: %+v", g)
	}
}

func TestParseRedirectErrors(t *testing.T) {
	l, _ := NewLogin(Global, ScopeKeys)
	cases := map[string]string{
		"чужой state":  "hcauth://auth/prod?code=C&state=other",
		"нет code":     "hcauth://auth/prod?state=" + l.state,
		"не hcauth":    "https://example.com/?code=C&state=" + l.state,
		"ошибка входа": "hcauth://auth/prod?error=access_denied&state=" + l.state,
		"длинный":      "hcauth://auth/prod?code=" + strings.Repeat("a", maxRedirectLen),
	}
	for name, redirect := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := l.parseRedirect(redirect); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
}

func TestTokenSubjectRejectsBadSub(t *testing.T) {
	for _, payload := range []string{`{"sub":"../x"}`, `{}`, `not json`} {
		if _, err := tokenSubject(fakeJWT(payload)); err == nil {
			t.Fatalf("%s: ожидалась ошибка", payload)
		}
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestDiscoverFallsBackToNextRegion(t *testing.T) {
	eu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer eu.Close()
	na := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/api/account/v2/accounts/user-1/paired-appliances" {
			t.Errorf("запрос: %s %v", r.URL.Path, r.Header)
		}
		w.Write([]byte(`{"appliances":[{"haId":"BOSCH-OVEN-1","brand":"BOSCH","haType":"Oven"}]}`))
	}))
	defer na.Close()
	a, paired, err := discover(context.Background(), []string{eu.URL, na.URL}, "tok", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.base != na.URL || paired.Host != na.URL || len(paired.Appliances) != 1 || paired.Appliances[0].HaType != "Oven" || len(paired.Raw) == 0 {
		t.Fatalf("аккаунт %s, ответ %+v", a.base, paired)
	}
}

func testAccount(srv *httptest.Server) *Account {
	return &Account{base: srv.URL, token: "tok", http: srv.Client(), sleep: noSleep}
}

func TestEncryption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/appliance/v2/appliances/TLS1/encryption-information":
			w.Write([]byte(`{"tls":{"key":"k1"}}`))
		case "/api/appliance/v2/appliances/AES1/encryption-information":
			w.Write([]byte(`{"aes":{"key":"k2","iv":"iv2"}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	a := testAccount(srv)
	if e, err := a.Encryption(context.Background(), "TLS1"); err != nil || e.Mode != "tls" || e.Key != "k1" {
		t.Fatalf("tls: %+v %v", e, err)
	}
	if e, err := a.Encryption(context.Background(), "AES1"); err != nil || e.Mode != "aes" || e.IV != "iv2" {
		t.Fatalf("aes: %+v %v", e, err)
	}
	if _, err := a.Encryption(context.Background(), "NONE"); err == nil {
		t.Fatal("ожидалась ошибка: нет ключей")
	}
	if _, err := a.Encryption(context.Background(), "../x"); err == nil {
		t.Fatal("ожидалась ошибка: недопустимый haId")
	}
}

func TestGetRetries5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"tls":{"key":"k"}}`))
	}))
	defer srv.Close()
	if _, err := testAccount(srv).Encryption(context.Background(), "X"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("запросов %d", calls.Load())
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackDescription(t *testing.T) {
	data := makeZip(t, map[string]string{
		"H1_FeatureMapping.xml":    "<fm/>",
		"H1_DeviceDescription.xml": "<dd/>",
		"other.txt":                "x",
	})
	d, err := unpackDescription(data, "H1")
	if err != nil {
		t.Fatal(err)
	}
	if string(d.FeatureMapping) != "<fm/>" || string(d.DeviceDescription) != "<dd/>" {
		t.Fatalf("файлы: %q %q", d.FeatureMapping, d.DeviceDescription)
	}
	if _, err := unpackDescription(makeZip(t, map[string]string{"H1_FeatureMapping.xml": "<fm/>"}), "H1"); err == nil {
		t.Fatal("ожидалась ошибка: нет DeviceDescription.xml")
	}
	if _, err := unpackDescription([]byte("not zip"), "H1"); err == nil {
		t.Fatal("ожидалась ошибка: не архив")
	}
}

func TestRegions(t *testing.T) {
	for name, want := range map[string]Region{"": Global, "global": Global, "eu": EU, "na": NA, "cn": China} {
		if r, ok := RegionByName(name); !ok || r.Name != want.Name {
			t.Errorf("%q: %+v %v", name, r, ok)
		}
	}
	if _, ok := RegionByName("mars"); ok {
		t.Fatal("чужой регион принят")
	}
	l, err := NewLogin(China, ScopeFull)
	if err != nil {
		t.Fatal(err)
	}
	link, err := l.URL()
	if err != nil || !strings.HasPrefix(link, "https://api.home-connect.cn/security/oauth/authorize?") ||
		!strings.Contains(link, "scope=Control+DeleteAppliance") {
		t.Fatalf("адрес входа: %s %v", link, err)
	}
}

// Данные токена - строгий JSON, вид из консоли браузера или сам токен.
func TestParseToken(t *testing.T) {
	now := time.Unix(1000, 0)
	jwt := fakeJWT(`{"sub":"user-1","exp":2000}`)
	for name, data := range map[string]string{
		"JSON":    `{"access_token":"` + jwt + `","expires_in":86400}`,
		"консоль": "{\n  access_token: '" + jwt + "',\n  expires_in: 86400,\n  id_token: 'x'\n}\n",
		"токен":   " " + jwt + "\n",
	} {
		if g, err := ParseToken(data, now); err != nil || g.Access != jwt || g.Subject != "user-1" || g.Expires.Unix() != 2000 {
			t.Errorf("%s: %+v %v", name, g, err)
		}
	}
	if g, err := ParseToken("{\n  access_token: '"+jwt+"',\n  refresh_token: 'rt',\n  scope: 'A B'\n}", now); err != nil || g.Refresh != "rt" || g.Scope != "A B" {
		t.Errorf("поля: %+v %v", g, err)
	}
	if g, err := ParseToken(fakeJWT(`{"sub":"user-1","exp":2000,"azp":"app","region":"CN"}`), now); err != nil || g.ClientID != "app" || g.Region != "CN" {
		t.Errorf("приложение и регион: %+v %v", g, err)
	}
	if _, err := ParseToken(jwt, time.Unix(2000, 0)); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("истёкший токен: %v", err)
	}
	for _, data := range []string{"", "{ access_token: '<token>' }", fakeJWT(`{"sub":"a/b"}`), strings.Repeat("a", maxTokenData+1)} {
		if _, err := ParseToken(data, now); !errors.Is(err, ErrBadToken) {
			t.Errorf("%.40q: %v", data, err)
		}
	}
}
