package hcapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/metrics"
)

const (
	// refreshBefore - за сколько до истечения access token шлюз обновляет его.
	refreshBefore = 5 * time.Minute
	// Обновлений токена облако разрешает не больше 10 в минуту и 100 в сутки.
	refreshPerMinute = 10
	refreshPerDay    = 100
	// loginInterval - самый частый опрос при входе; slowDown - на сколько реже после slow_down.
	loginInterval = 5 * time.Second
	slowDown      = 5 * time.Second
)

// Состояния входа.
const (
	LoginPending = "pending"
	LoginDone    = "done"
	LoginFailed  = "failed"
)

// Login - вход по Device Flow: ссылка и код для владельца и итог входа.
type Login struct {
	Status          string
	UserCode        string
	VerificationURI string
	// VerificationURIComplete - ссылка с уже подставленным кодом.
	VerificationURIComplete string
	Expires                 time.Time
	// Error - почему вход не удался или почему токены перестали действовать.
	Error string
}

type tokenResponse struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresIn int    `json:"expires_in"`
	Scope     string `json:"scope"`
}

// LoginState - текущий вход.
func (c *Client) LoginState() Login {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.login
}

func (c *Client) setLogin(l Login) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.login = l
}

// StartLogin запрашивает код входа и в фоне опрашивает облако, пока владелец не войдёт
// или код не истечёт. done вызывается после успешного входа. Новый вход отменяет прежний.
func (c *Client) StartLogin(ctx context.Context, done func()) (Login, error) {
	cfg := c.config()
	if cfg.ClientID == "" {
		return Login{}, errors.New("не задан homeconnect.client_id")
	}
	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	status, code, err := c.oauth(ctx, "/security/oauth/device_authorization",
		url.Values{"client_id": {cfg.ClientID}, "scope": {Scope}}, &resp)
	if err != nil {
		return Login{}, err
	}
	if status != http.StatusOK || resp.DeviceCode == "" {
		return Login{}, &APIError{Status: status, Key: code}
	}
	l := Login{Status: LoginPending, UserCode: resp.UserCode, VerificationURI: resp.VerificationURI,
		VerificationURIComplete: resp.VerificationURIComplete, Expires: c.now().Add(time.Duration(resp.ExpiresIn) * time.Second)}
	c.mu.Lock()
	c.login = l
	c.mu.Unlock()
	c.log.Info("вход в Home Connect: откройте ссылку и введите код", "url", l.VerificationURI, "code", l.UserCode)
	interval := max(time.Duration(resp.Interval)*time.Second, loginInterval)
	go c.poll(cfg.ClientID, resp.DeviceCode, l, interval, done)
	return l, nil
}

// poll опрашивает облако, пока владелец входит. Опрос заканчивается сам, когда истекает код.
func (c *Client) poll(clientID, deviceCode string, l Login, interval time.Duration, done func()) {
	ctx, cancel := context.WithDeadline(context.Background(), l.Expires)
	defer cancel()
	current := func() bool { return c.LoginState() == l }
	fail := func(text string) {
		if current() {
			l.Status, l.Error = LoginFailed, text
			c.setLogin(l)
		}
	}
	for {
		if c.lim.sleep(ctx, interval) != nil {
			fail("Код истёк, нажмите «Войти» ещё раз.")
			return
		}
		if !current() {
			return
		}
		var tok tokenResponse
		status, code, err := c.oauth(ctx, "/security/oauth/token", url.Values{"grant_type": {"device_code"},
			"device_code": {deviceCode}, "client_id": {clientID}}, &tok)
		switch {
		case err != nil:
			c.log.Warn("вход в Home Connect: облако недоступно, опрос продолжается", "err", err)
		case status == http.StatusOK && tok.Access != "" && tok.Refresh != "":
			c.mu.Lock()
			err = c.saveTokensLocked(&Tokens{Access: tok.Access, Refresh: tok.Refresh, Scope: tok.Scope,
				Expires: c.now().Add(time.Duration(tok.ExpiresIn) * time.Second)})
			c.mu.Unlock()
			if err != nil {
				c.log.Error("вход в Home Connect: токены не сохранены", "err", err)
				fail("Не удалось сохранить токены, подробности в журнале шлюза.")
				return
			}
			l.Status = LoginDone
			c.setLogin(l)
			c.log.Info("вход в Home Connect выполнен")
			if done != nil {
				done()
			}
			return
		case code == "authorization_pending":
		case code == "slow_down":
			interval += slowDown
		case code == "access_denied":
			fail("Вход отклонён в аккаунте Home Connect.")
			return
		case code == "expired_token":
			fail("Код истёк, нажмите «Войти» ещё раз.")
			return
		default:
			c.log.Warn("вход в Home Connect отклонён облаком", "status", status, "error", code)
			fail("Облако Home Connect отклонило вход, подробности в журнале шлюза.")
			return
		}
	}
}

// refresh обновляет токены, если они всё ещё old: другая горутина могла обновить их раньше.
// Облако требует для обновления client_secret. Если облако отвергло refresh token, нужен новый вход.
func (c *Client) refresh(ctx context.Context, old *Tokens) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	cur := c.tokens
	if cur == nil {
		c.mu.Unlock()
		return ErrNotLoggedIn
	}
	if cur.Access != old.Access {
		c.mu.Unlock()
		return nil
	}
	now := c.now()
	c.refreshes = slices.DeleteFunc(c.refreshes, func(t time.Time) bool { return now.Sub(t) >= 24*time.Hour })
	lastMinute := 0
	for _, t := range c.refreshes {
		if now.Sub(t) < time.Minute {
			lastMinute++
		}
	}
	if len(c.refreshes) >= refreshPerDay || lastMinute >= refreshPerMinute {
		c.mu.Unlock()
		return ErrBusy
	}
	c.refreshes = append(c.refreshes, now)
	secret := c.cfg.ClientSecret
	c.mu.Unlock()

	var tok tokenResponse
	status, code, err := c.oauth(ctx, "/security/oauth/token", url.Values{"grant_type": {"refresh_token"},
		"refresh_token": {cur.Refresh}, "client_secret": {secret}}, &tok)
	if err != nil {
		return err
	}
	if status != http.StatusOK || tok.Access == "" {
		if status == http.StatusBadRequest || status == http.StatusUnauthorized {
			c.log.Error("облако Home Connect не обновило токен, нужен новый вход", "status", status, "error", code)
			c.mu.Lock()
			c.tokens = nil
			c.login = Login{Status: LoginFailed, Error: "Облако не обновило токен: проверьте client_secret и войдите заново."}
			c.mu.Unlock()
			os.Remove(c.path)
			return ErrNotLoggedIn
		}
		return &APIError{Status: status, Key: code}
	}
	t := &Tokens{Access: tok.Access, Refresh: tok.Refresh, Scope: tok.Scope,
		Expires: c.now().Add(time.Duration(tok.ExpiresIn) * time.Second)}
	if t.Refresh == "" {
		t.Refresh = cur.Refresh
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveTokensLocked(t)
}

// oauth отправляет форму на адрес входа облака. Возвращает код ответа и код ошибки OAuth.
func (c *Client) oauth(ctx context.Context, path string, form url.Values, out any) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config().BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		metrics.SourceRequest(SourceName, "error")
		return 0, "", err
	}
	defer resp.Body.Close()
	metrics.SourceRequest(SourceName, strconv.Itoa(resp.StatusCode))
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, "", err
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, out); err != nil {
			return 0, "", err
		}
		return resp.StatusCode, "", nil
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &e)
	return resp.StatusCode, e.Error, nil
}
