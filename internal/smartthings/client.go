// Package smartthings - устройства из облака Samsung SmartThings: вход OAuth через
// приложение API Access владельца, запросы API, события по потоку SSE и устройства как
// приборы по профилю. Устройство SmartThings - компоненты с capability; имя функции прибора -
// «capability.атрибут» его компонента, например switch.switch или audioVolume.volume.
package smartthings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/atomicfile"
	"hcgateway/internal/metrics"
)

// SourceName - источник в метриках.
const SourceName = "smartthings"

// Scopes - области доступа, которые шлюз просит при входе: чтение устройств и команды им,
// чтение мест и поток событий SSE. С ними же владелец создаёт приложение API Access.
const Scopes = "r:devices:* x:devices:* r:locations:* sse"

const (
	// requestTimeout - таймаут запроса к облаку.
	requestTimeout = 15 * time.Second
	// getRetries - сколько раз повторять чтение после 5xx или сбоя сети.
	getRetries = 2
	// maxBody - предел ответа облака.
	maxBody = 4 << 20
	// refreshBefore - за сколько до конца срока обновлять access token: он живёт 24 часа.
	refreshBefore = 5 * time.Minute
	// maxTokenFile и maxUsageFile - пределы файлов токенов и расхода запросов.
	maxTokenFile = 64 << 10
	maxUsageFile = 4 << 10
	// userAgent - SmartThings требует понятный User-Agent у запросов SSE.
	userAgent = "hcgateway (Yandex Alice gateway)"
)

// Ошибки входа.
var (
	// ErrNotConfigured - не заданы client_id и client_secret.
	ErrNotConfigured = errors.New("не заданы smartthings.client_id и smartthings.client_secret")
	// ErrLoginRequired - входа нет или облако отвергло refresh token: нужно войти заново.
	ErrLoginRequired = errors.New("нужно войти в SmartThings")
)

// APIError - ответ облака с ошибкой.
type APIError struct {
	Status int
	Code   string
	// RetryAfter - сколько ждать после 429, если облако сообщило.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("SmartThings: HTTP %d, код %s", e.Status, e.Code)
	}
	return fmt.Sprintf("SmartThings: HTTP %d", e.Status)
}

// Tokens - вход в SmartThings: токены и установленное приложение. Лежат в
// smartthings-tokens.json.
type Tokens struct {
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	Expiry         time.Time `json:"expiry"`
	InstalledAppID string    `json:"installed_app_id,omitempty"`
}

// Usage - запросы к облаку за месяц: план разработчика SmartThings даёт 100 000 в месяц.
type Usage struct {
	Month string `json:"month"`
	Calls int    `json:"calls"`
}

// Client - запросы к API SmartThings от имени владельца.
type Client struct {
	http      *http.Client
	path      string
	usagePath string
	log       *slog.Logger

	mu           sync.Mutex
	base         string
	clientID     string
	clientSecret string
	tokens       *Tokens
	usage        Usage
	// refreshMu - обновление токена по одному: два запроса с истёкшим токеном не должны
	// обновить его дважды.
	refreshMu sync.Mutex
}

// NewClient открывает файл токенов path и файл расхода usagePath.
func NewClient(path, usagePath string, log *slog.Logger) (*Client, error) {
	c := &Client{http: &http.Client{Timeout: requestTimeout}, path: path, usagePath: usagePath, log: log}
	if err := readJSON(path, maxTokenFile, &c.tokens); err != nil {
		return nil, err
	}
	if c.tokens != nil && c.tokens.RefreshToken == "" {
		c.tokens = nil
	}
	if err := readJSON(usagePath, maxUsageFile, &c.usage); err != nil {
		c.log.Warn("SmartThings: файл расхода запросов не прочитан, счёт начнётся заново", "err", err)
		c.usage = Usage{}
	}
	return c, nil
}

// readJSON читает JSON-файл не больше limit байт; файла нет - не ошибка.
func readJSON(path string, limit int, v any) error {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(data) > limit:
		return fmt.Errorf("%s больше %d байт", path, limit)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Configure задаёт приложение и адрес облака.
func (c *Client) Configure(clientID, clientSecret, base string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientID, c.clientSecret, c.base = clientID, clientSecret, strings.TrimSuffix(base, "/")
}

// Configured сообщает, заданы ли client_id и client_secret.
func (c *Client) Configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientID != "" && c.clientSecret != "" && c.base != ""
}

// LoggedIn сообщает, есть ли вход.
func (c *Client) LoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens != nil
}

// Usage - расход запросов за текущий месяц.
func (c *Client) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.usage.Month != month(time.Now()) {
		return Usage{Month: month(time.Now())}
	}
	return c.usage
}

func month(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// count добавляет запрос к расходу месяца и сохраняет расход.
func (c *Client) count() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := month(time.Now()); c.usage.Month != m {
		c.usage = Usage{Month: m}
	}
	c.usage.Calls++
	data, err := json.Marshal(c.usage)
	if err == nil {
		err = atomicfile.Write(c.usagePath, data)
	}
	if err != nil {
		c.log.Warn("SmartThings: расход запросов не сохранён", "err", err)
	}
}

// AuthorizeURL - страница входа SmartThings для приложения владельца.
func (c *Client) AuthorizeURL(redirect, state string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clientID == "" || c.base == "" {
		return "", ErrNotConfigured
	}
	q := url.Values{"client_id": {c.clientID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {Scopes}, "state": {state}}
	return c.base + "/oauth/authorize?" + q.Encode(), nil
}

// tokenResponse - ответ /oauth/token.
type tokenResponse struct {
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	ExpiresIn      int    `json:"expires_in"`
	InstalledAppID string `json:"installed_app_id"`
	Error          string `json:"error"`
}

// Exchange меняет код со страницы входа на токены и сохраняет их.
func (c *Client) Exchange(ctx context.Context, code, redirect string) error {
	t, err := c.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}})
	if err != nil {
		return err
	}
	return c.save(t)
}

// Logout удаляет токены.
func (c *Client) Logout() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = nil
	if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (c *Client) save(t *Tokens) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := atomicfile.Write(c.path, data); err != nil {
		return err
	}
	c.tokens = t
	return nil
}

// tokenRequest - запрос /oauth/token с client_id и client_secret в заголовке Basic.
func (c *Client) tokenRequest(ctx context.Context, form url.Values) (*Tokens, error) {
	c.mu.Lock()
	base, id, secret := c.base, c.clientID, c.clientSecret
	c.mu.Unlock()
	if id == "" || secret == "" || base == "" {
		return nil, ErrNotConfigured
	}
	form.Set("client_id", id)
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	c.count()
	if err != nil {
		metrics.SourceRequest(SourceName, "error")
		return nil, err
	}
	defer resp.Body.Close()
	metrics.SourceRequest(SourceName, strconv.Itoa(resp.StatusCode))
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	var tr tokenResponse
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" || tr.RefreshToken == "" {
		if tr.Error == "invalid_grant" || resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: облако отвергло вход (%s)", ErrLoginRequired, tr.Error)
		}
		return nil, &APIError{Status: resp.StatusCode, Code: tr.Error}
	}
	expires := time.Duration(tr.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 24 * time.Hour
	}
	return &Tokens{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, Expiry: time.Now().Add(expires),
		InstalledAppID: tr.InstalledAppID}, nil
}

// token - действующий access token; за 5 минут до конца срока шлюз его обновляет. Если
// облако отвергло refresh token, шлюз удаляет токены: нужен новый вход.
func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	t := c.tokens
	c.mu.Unlock()
	if t == nil {
		return "", ErrLoginRequired
	}
	if !force && time.Until(t.Expiry) > refreshBefore {
		return t.AccessToken, nil
	}
	n, err := c.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
	if errors.Is(err, ErrLoginRequired) {
		c.log.Warn("SmartThings: облако отвергло refresh token, нужен новый вход", "err", err)
		if rmErr := c.Logout(); rmErr != nil {
			c.log.Error("SmartThings: токены не удалены", "err", rmErr)
		}
		return "", err
	}
	if err != nil {
		return "", err
	}
	if n.InstalledAppID == "" {
		n.InstalledAppID = t.InstalledAppID
	}
	if err := c.save(n); err != nil {
		return "", err
	}
	return n.AccessToken, nil
}

// do выполняет запрос к API. Чтение после 5xx или сбоя сети повторяется не больше 2 раз с
// паузой 1 и 2 с; команды не повторяются. На 401 шлюз обновляет токен и повторяет запрос
// один раз.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	if base == "" {
		return ErrNotConfigured
	}
	target := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		target = base + path
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	retries := 0
	if method == http.MethodGet {
		retries = getRetries
	}
	refreshed := false
	for attempt := 0; ; attempt++ {
		token, err := c.token(ctx, false)
		if err != nil {
			return err
		}
		status, err := c.once(ctx, method, target, token, payload, out)
		if status == http.StatusUnauthorized && !refreshed {
			refreshed = true
			if _, err := c.token(ctx, true); err != nil {
				return err
			}
			attempt--
			continue
		}
		var ae *APIError
		retry := err != nil && ctx.Err() == nil && (!errors.As(err, &ae) || ae.Status >= 500)
		if !retry || attempt >= retries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
}

func (c *Client) once(ctx context.Context, method, target, token string, payload []byte, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json;charset=utf-8")
	}
	resp, err := c.http.Do(req)
	c.count()
	if err != nil {
		metrics.SourceRequest(SourceName, "error")
		return 0, err
	}
	defer resp.Body.Close()
	metrics.SourceRequest(SourceName, strconv.Itoa(resp.StatusCode))
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode}
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil {
			ae.Code = e.Error.Code
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ae.RetryAfter = time.Duration(s) * time.Second
		} else if ms, err := strconv.Atoi(resp.Header.Get("x-ratelimit-reset")); err == nil && ms > 0 {
			ae.RetryAfter = time.Duration(ms) * time.Millisecond
		}
		return resp.StatusCode, ae
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("ответ SmartThings не разобран: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// NewState - случайная строка state для входа: 16 байт в hex.
func NewState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
