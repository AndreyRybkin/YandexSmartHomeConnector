// Package hcapi - клиент открытого API облака Home Connect: вход по Device Flow, токены,
// запросы к приборам, поток событий и лимиты облака.
package hcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"hcgateway/internal/atomicfile"
	"hcgateway/internal/metrics"
)

const (
	// DefaultBaseURL - адрес облака Home Connect.
	DefaultBaseURL = "https://api.home-connect.com"
	// Scope - области доступа: опознание приборов и наблюдение, управление и настройки всех типов приборов.
	Scope = "IdentifyAppliance Monitor Control Settings"
	// SourceName - источник в метриках.
	SourceName = "homeconnect-cloud"

	mediaType = "application/vnd.bsh.sdk.v1+json"
	// requestTimeout - таймаут одного запроса, кроме потока событий.
	requestTimeout = 30 * time.Second
	// getRetries - сколько раз повторять чтение после 5xx или сбоя сети.
	getRetries = 2
	// maxBody - предел ответа облака.
	maxBody = 1 << 20
	// errorStreak и errorPause - после стольких ошибок подряд шлюз делает паузу в запросах.
	// Облако после 10 ошибок подряд за 10 минут блокирует клиента на 10 минут.
	errorStreak = 5
	errorPause  = 10 * time.Minute
)

var (
	// ErrNotLoggedIn - вход в аккаунт Home Connect не выполнен или токены больше не действуют.
	ErrNotLoggedIn = errors.New("нет входа в Home Connect")
	// ErrBusy - лимит запросов облака: запрос не отправлен.
	ErrBusy = errors.New("лимит запросов облака Home Connect")
	// ErrLoginChanged - вход сменился или удалён: поток событий старого входа закрыт.
	ErrLoginChanged = errors.New("вход в Home Connect сменился")
	// ErrPaused - пауза после ошибок подряд: запрос не отправлен.
	ErrPaused = errors.New("пауза после ошибок облака Home Connect")
	// ErrBadResponse - облако ответило успехом, но тело ответа не разобрано.
	ErrBadResponse = errors.New("облако Home Connect: ответ не разобран")
)

// APIError - ответ облака с ошибкой.
type APIError struct {
	Status      int
	Key         string
	Description string
}

func (e *APIError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("облако Home Connect: HTTP %d", e.Status)
	}
	return fmt.Sprintf("облако Home Connect: HTTP %d %s", e.Status, e.Key)
}

// Config - приложение на портале разработчика Home Connect и адрес облака.
type Config struct {
	ClientID     string
	ClientSecret string
	BaseURL      string
}

// Tokens - токены облака. Лежат в файле как есть: без них шлюз не обратится к облаку.
type Tokens struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expires time.Time `json:"expires"`
	Scope   string    `json:"scope,omitempty"`
	// ClientID - приложение Home Connect, которому выдан токен: токен продлевается с ним, без
	// секрета. Пусто - токен выдан приложению с портала разработчика через Device Flow.
	ClientID string `json:"client_id,omitempty"`
	// BaseURL - адрес облака токена; пусто - homeconnect.api_url.
	BaseURL string `json:"base_url,omitempty"`
	// Region - регион токена приложения Home Connect.
	Region string `json:"region,omitempty"`
	// Refreshed - когда токен продлён в последний раз.
	Refreshed time.Time `json:"refreshed,omitzero"`
}

// Client - клиент облака. Методы можно вызывать из разных горутин.
type Client struct {
	path   string
	log    *slog.Logger
	http   *http.Client
	stream *http.Client
	now    func() time.Time
	lim    *limiter

	// refreshMu - обновления токена по одному.
	refreshMu sync.Mutex

	mu          sync.Mutex
	cfg         Config
	tokens      *Tokens
	errors      int
	pausedUntil time.Time
	refreshes   []time.Time
	login       Login
	// keepCheck - как часто KeepFresh проверяет срок токена.
	keepCheck time.Duration
	// refreshFailed - когда не удалось последнее продление, nextRefresh - когда KeepFresh повторит.
	refreshFailed, nextRefresh time.Time
	// rejected - облако отвергло токен приложения Home Connect: нужен новый вход.
	rejected bool
	// stopStream закрывает открытый поток событий; nil - потока нет.
	stopStream context.CancelCauseFunc
}

// New создаёт клиента. path - файл токенов; если он есть, вход уже выполнен.
func New(path string, log *slog.Logger) (*Client, error) {
	metrics.TokenRefreshSource(SourceName)
	c := &Client{path: path, log: log, http: &http.Client{Timeout: requestTimeout}, stream: &http.Client{},
		now: time.Now, lim: newLimiter(), cfg: Config{BaseURL: DefaultBaseURL}, keepCheck: keepCheck}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return c, nil
	case err != nil:
		return nil, err
	case len(data) > maxBody:
		return nil, fmt.Errorf("%s больше %d байт", path, maxBody)
	}
	var t Tokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if t.Access != "" && t.Refresh != "" {
		c.tokens = &t
	}
	return c, nil
}

// Configure задаёт приложение и адрес облака без перезапуска.
func (c *Client) Configure(cfg Config) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
}

func (c *Client) config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// current - текущие токены и адрес их облака: у токена приложения Home Connect он свой.
func (c *Client) current() (*Tokens, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens != nil && c.tokens.BaseURL != "" {
		return c.tokens, c.tokens.BaseURL
	}
	return c.tokens, c.cfg.BaseURL
}

// LoggedIn сообщает, есть ли токены.
func (c *Client) LoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens != nil
}

// Logout удаляет токены.
func (c *Client) Logout() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = nil
	c.loginChangedLocked()
	if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// loginChangedLocked сбрасывает всё, что относилось к прежнему входу, и закрывает его поток событий.
func (c *Client) loginChangedLocked() {
	c.refreshFailed, c.nextRefresh, c.rejected = time.Time{}, time.Time{}, false
	if c.stopStream != nil {
		c.stopStream(ErrLoginChanged)
	}
}

func (c *Client) saveTokensLocked(t *Tokens) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(c.path, data); err != nil {
		return err
	}
	c.tokens = t
	return nil
}

// access - действующий access token и адрес его облака; если токен скоро истечёт, сначала
// обновляется. Токен и адрес берутся вместе: токен не уйдёт на адрес нового входа.
func (c *Client) access(ctx context.Context) (token, base string, err error) {
	t, base := c.current()
	if t == nil {
		return "", "", ErrNotLoggedIn
	}
	if c.now().Add(refreshBefore).Before(t.Expires) {
		return t.Access, base, nil
	}
	if err := c.refresh(ctx, t); err != nil {
		return "", "", err
	}
	if t, base = c.current(); t == nil {
		return "", "", ErrNotLoggedIn
	}
	return t.Access, base, nil
}

// call - запрос к API: очередь лимитов, токен, разбор ошибки, учёт в метриках.
// Чтение после 5xx или сбоя сети повторяется не больше getRetries раз; команды не повторяются.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	retried401 := false
	for attempt := 0; ; attempt++ {
		status, data, err := c.once(ctx, method, path, payload)
		if err == nil && status == http.StatusUnauthorized && !retried401 {
			// Токен отозван раньше срока: обновить и повторить один раз.
			retried401 = true
			c.mu.Lock()
			t := c.tokens
			c.mu.Unlock()
			if t != nil {
				if rerr := c.refresh(ctx, t); rerr != nil {
					return rerr
				}
				continue
			}
		}
		retry := method == http.MethodGet && (err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrPaused) &&
			!errors.Is(err, ErrNotLoggedIn) || status >= 500)
		if !retry || attempt >= getRetries || ctx.Err() != nil {
			if err != nil {
				return err
			}
			return decode(status, data, out)
		}
		if serr := c.lim.sleep(ctx, time.Duration(attempt+1)*time.Second); serr != nil {
			return serr
		}
	}
}

// once отправляет запрос один раз и возвращает код и тело ответа.
func (c *Client) once(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	c.mu.Lock()
	paused := c.now().Before(c.pausedUntil)
	c.mu.Unlock()
	if paused {
		return 0, nil, ErrPaused
	}
	token, base, err := c.access(ctx)
	if err != nil {
		return 0, nil, err
	}
	if err := c.lim.wait(ctx); err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", mediaType)
	if payload != nil {
		req.Header.Set("Content-Type", mediaType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		metrics.SourceRequest(SourceName, "error")
		c.result(false)
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	metrics.SourceRequest(SourceName, strconv.Itoa(resp.StatusCode))
	if resp.StatusCode == http.StatusTooManyRequests {
		c.retryAfter(resp.Header.Get("Retry-After"))
	}
	c.result(resp.StatusCode < 400 || expected(resp.StatusCode, data))
	return resp.StatusCode, data, err
}

// expected - ответ с ошибкой, который бывает в обычной работе: нет выбранной или активной программы.
func expected(status int, data []byte) bool {
	if status != http.StatusNotFound {
		return false
	}
	e := parseError(status, data)
	return e.Key == "SDK.Error.NoProgramActive" || e.Key == "SDK.Error.NoProgramSelected"
}

// result считает ошибки подряд. После errorStreak ошибок запросы встают на паузу errorPause.
func (c *Client) result(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.errors = 0
		return
	}
	c.errors++
	if c.errors >= errorStreak {
		c.errors = 0
		c.pausedUntil = c.now().Add(errorPause)
		c.log.Warn("облако Home Connect: ошибки подряд, пауза в запросах", "for", errorPause)
	}
}

// retryAfter - облако ответило 429: запросов не будет столько секунд, сколько оно просит, и не меньше минуты.
func (c *Client) retryAfter(h string) {
	d := time.Minute
	if s, err := strconv.Atoi(h); err == nil && s > 0 && time.Duration(s)*time.Second > d {
		d = time.Duration(s) * time.Second
	}
	c.lim.block(d)
	c.log.Warn("облако Home Connect: лимит запросов", "retry_after", d)
}

func decode(status int, data []byte, out any) error {
	if status >= 400 {
		return parseError(status, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %w", ErrBadResponse, err)
	}
	return nil
}

func parseError(status int, data []byte) *APIError {
	var e struct {
		Error struct {
			Key         string `json:"key"`
			Description string `json:"description"`
		} `json:"error"`
	}
	json.Unmarshal(data, &e)
	return &APIError{Status: status, Key: e.Error.Key, Description: e.Error.Description}
}
