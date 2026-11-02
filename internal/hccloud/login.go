// Package hccloud загружает из облака Home Connect ключи и описания приборов
// для связи с ними по локальной сети. Облако нужно один раз, при добавлении прибора.
package hccloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// appClientID - client_id официального приложения Home Connect.
	// Ключи шифрования приборов облако отдаёт только ему.
	appClientID = "9B75AC9EC512F36C84256AC47D813E2C1DD0D6520DF774B020E1E6E2EB29B1F3"
	redirectURI = "hcauth://auth/prod"
	// ScopeKeys - только ключи приборов: токен нужен один раз.
	ScopeKeys = "ReadOrigApi"
	// ScopeFull - области, как у приложения: токен сохраняется и управляет приборами через облако.
	ScopeFull = "Control DeleteAppliance IdentifyAppliance Images Monitor ReadAccount ReadOrigApi Settings WriteAppliance WriteOrigApi"
	// requestTimeout - таймаут одного запроса к облаку.
	requestTimeout = 30 * time.Second
	// maxJSONSize - предел размера JSON-ответа облака.
	maxJSONSize = 1 << 20
	// maxRedirectLen - предел длины адреса, который вставляет пользователь.
	maxRedirectLen = 4096
	// maxTokenData - предел длины данных токена, которые вставляет пользователь.
	maxTokenData = 32 << 10
)

// subjectRe - допустимый идентификатор аккаунта: он попадает в путь запроса.
var subjectRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// tokenFieldRe находит поля токенов в JSON и в виде из консоли браузера, где ключи без кавычек,
// а строки - в одинарных кавычках.
var tokenFieldRe = regexp.MustCompile(`(access_token|refresh_token|scope)["']?\s*:\s*["']([^"']+)["']`)

// Grant - токены аккаунта после входа.
type Grant struct {
	Access, Refresh, Scope, Subject string
	Expires                         time.Time
	// ClientID и Region - приложение и регион из вставленного токена; пусто - токен их не называет.
	ClientID, Region string
	// kept - токен уже лежит в клиенте облака.
	kept bool
}

var (
	// ErrBadToken - в данных нет токена в формате JWT с идентификатором аккаунта.
	ErrBadToken = errors.New("в данных нет access token")
	// ErrTokenExpired - срок токена истёк.
	ErrTokenExpired = errors.New("срок access token истёк")
)

// Login - вход в аккаунт Home Connect по OAuth с PKCE.
// Пользователь входит в браузере, а адрес перенаправления hcauth://… копирует в шлюз.
type Login struct {
	authBase string
	scope    string
	verifier string
	state    string
	http     *http.Client
}

// NewLogin начинает вход в аккаунт региона region с областями scope.
func NewLogin(region Region, scope string) (*Login, error) {
	verifier, err := randomString(32)
	if err != nil {
		return nil, err
	}
	state, err := randomString(16)
	if err != nil {
		return nil, err
	}
	return &Login{
		authBase: region.Base + "/security/oauth/",
		scope:    scope,
		verifier: verifier,
		state:    state,
		http:     &http.Client{Timeout: requestTimeout},
	}, nil
}

// URL - адрес страницы входа.
func (l *Login) URL() (string, error) {
	nonce, err := randomString(16)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(l.verifier))
	q := url.Values{
		"response_type":         {"code"},
		"prompt":                {"login"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"client_id":             {appClientID},
		"scope":                 {l.scope},
		"nonce":                 {nonce},
		"state":                 {l.state},
		"redirect_uri":          {redirectURI},
	}
	return l.authBase + "authorize?" + q.Encode(), nil
}

// Exchange разбирает адрес перенаправления, проверяет state и получает access token.
func (l *Login) Exchange(ctx context.Context, redirect string) (*Grant, error) {
	code, err := l.parseRedirect(redirect)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {appClientID},
		"code_verifier": {l.verifier},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.authBase+"token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONSize))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return nil, fmt.Errorf("вход в Home Connect: HTTP %d %s", resp.StatusCode, e.Error)
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return nil, errors.New("вход в Home Connect: в ответе нет access token")
	}
	subject, err := tokenSubject(t.AccessToken)
	if err != nil {
		return nil, err
	}
	return &Grant{Access: t.AccessToken, Refresh: t.RefreshToken, Scope: t.Scope, Subject: subject,
		Expires: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)}, nil
}

func (l *Login) parseRedirect(redirect string) (string, error) {
	redirect = strings.TrimSpace(redirect)
	if len(redirect) > maxRedirectLen {
		return "", errors.New("адрес слишком длинный")
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "hcauth" {
		return "", errors.New("нужен адрес, который начинается с hcauth://")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("вход отклонён: %s", e)
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.state)) != 1 {
		return "", errors.New("адрес не от этого входа: state не совпадает")
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("в адресе нет code")
	}
	return code, nil
}

// ParseToken достаёт токены из вставленных данных: JSON с токенами, вид из консоли браузера или
// сам access token. Значения токенов в текст ошибки не попадают.
func ParseToken(data string, now time.Time) (*Grant, error) {
	if len(data) > maxTokenData {
		return nil, ErrBadToken
	}
	g := &Grant{Access: strings.TrimSpace(data)}
	if m := tokenFieldRe.FindAllStringSubmatch(data, -1); m != nil {
		g.Access = ""
		for _, f := range m {
			switch f[1] {
			case "access_token":
				g.Access = f[2]
			case "refresh_token":
				g.Refresh = f[2]
			case "scope":
				g.Scope = f[2]
			}
		}
	}
	c, err := tokenClaims(g.Access)
	if err != nil {
		return nil, ErrBadToken
	}
	if c.Exp == 0 {
		// Без срока токен считается истёкшим: шлюз сразу его продлит.
		g.Expires = now
	} else if g.Expires = time.Unix(c.Exp, 0); !now.Before(g.Expires) {
		return nil, ErrTokenExpired
	}
	g.Subject, g.ClientID, g.Region = c.Sub, c.Azp, c.Region
	return g, nil
}

// tokenSubject достаёт идентификатор аккаунта из access token в формате JWT.
func tokenSubject(token string) (string, error) {
	claims, err := tokenClaims(token)
	if err != nil {
		return "", err
	}
	return claims.Sub, nil
}

// claims - нужные шлюзу поля access token.
type claims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
	// Azp - приложение, которому выдан токен; Region - регион аккаунта, например CN.
	Azp    string `json:"azp"`
	Region string `json:"region"`
}

// tokenClaims разбирает access token в формате JWT.
// Подпись не проверяется: токен проверит облако при первом же запросе.
func tokenClaims(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("access token не в формате JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return claims{}, errors.New("access token не в формате JWT")
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil || !subjectRe.MatchString(c.Sub) {
		return claims{}, errors.New("в access token нет идентификатора аккаунта")
	}
	return c, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
