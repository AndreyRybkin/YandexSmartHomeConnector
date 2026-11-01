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
	defaultAuthBase = "https://api.home-connect.com/security/oauth/"
	// appClientID - client_id официального приложения Home Connect.
	// Ключи шифрования приборов облако отдаёт только ему.
	appClientID = "9B75AC9EC512F36C84256AC47D813E2C1DD0D6520DF774B020E1E6E2EB29B1F3"
	redirectURI = "hcauth://auth/prod"
	loginScope  = "ReadOrigApi"
	// requestTimeout - таймаут одного запроса к облаку.
	requestTimeout = 30 * time.Second
	// maxJSONSize - предел размера JSON-ответа облака.
	maxJSONSize = 1 << 20
	// maxRedirectLen - предел длины адреса, который вставляет пользователь.
	maxRedirectLen = 4096
)

// subjectRe - допустимый идентификатор аккаунта: он попадает в путь запроса.
var subjectRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Login - вход в аккаунт Home Connect по OAuth с PKCE.
// Пользователь входит в браузере, а адрес перенаправления hcauth://… копирует в шлюз.
type Login struct {
	authBase string
	verifier string
	state    string
	http     *http.Client
}

func NewLogin() (*Login, error) {
	verifier, err := randomString(32)
	if err != nil {
		return nil, err
	}
	state, err := randomString(16)
	if err != nil {
		return nil, err
	}
	return &Login{
		authBase: defaultAuthBase,
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
		"scope":                 {loginScope},
		"nonce":                 {nonce},
		"state":                 {l.state},
		"redirect_uri":          {redirectURI},
	}
	return l.authBase + "authorize?" + q.Encode(), nil
}

// Exchange разбирает адрес перенаправления, проверяет state и получает access token.
// Возвращает токен и идентификатор аккаунта из него.
func (l *Login) Exchange(ctx context.Context, redirect string) (token, subject string, err error) {
	code, err := l.parseRedirect(redirect)
	if err != nil {
		return "", "", err
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
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := l.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONSize))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return "", "", fmt.Errorf("вход в Home Connect: HTTP %d %s", resp.StatusCode, e.Error)
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", "", errors.New("вход в Home Connect: в ответе нет access token")
	}
	subject, err = tokenSubject(t.AccessToken)
	if err != nil {
		return "", "", err
	}
	return t.AccessToken, subject, nil
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

// tokenSubject достаёт идентификатор аккаунта из access token в формате JWT.
// Подпись не проверяется: токен проверит облако при первом же запросе.
func tokenSubject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("access token не в формате JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", errors.New("access token не в формате JWT")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || !subjectRe.MatchString(claims.Sub) {
		return "", errors.New("в access token нет идентификатора аккаунта")
	}
	return claims.Sub, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
