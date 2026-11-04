// Package thinq - приборы LG из облака ThinQ Connect: запросы API по личному токену (PAT),
// события по MQTT и стиральная машина в Алисе. Адреса, заголовки и тела запросов -
// как в официальной библиотеке LG pythinqconnect.
package thinq

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/metrics"
)

// SourceName - источник в метриках.
const SourceName = "thinq"

const (
	// apiKey - открытый ключ API ThinQ Connect из библиотеки LG.
	apiKey = "v6GFvkweNo7DK7yD3ylIZ9w52aKBU0eJ7wLXkSR3"
	// clientPrefix - начало идентификатора клиента, как в библиотеке LG.
	clientPrefix = "thinq-open-"
	// requestTimeout - таймаут запроса, как в библиотеке LG.
	requestTimeout = 15 * time.Second
	// getRetries - сколько раз повторять чтение после 5xx или сбоя сети.
	getRetries = 2
	// maxBody - предел ответа облака.
	maxBody = 1 << 20
)

// Коды ошибок ThinQ Connect, которые шлюз различает.
const (
	codeAlreadySubscribedPush = "1207"
	codeNotConnectedDevice    = "1222"
	codeExceededCalls         = "1306"
	codeResponseDelay         = "2209"
	codeRemoteOff             = "2301"
	codeNotInState            = "2302"
	codeInError               = "2303"
	codePowerOff              = "2304"
	codeNotInMode             = "2305"
)

// regions - страны каждого региона облака, как в библиотеке LG.
var regions = map[string][]string{
	"kic": strings.Fields("AU BD CN HK ID IN JP KH KR LA LK MM MY NP NZ PH SG TH TW VN"),
	"aic": strings.Fields("AG AR AW BB BO BR BS BZ CA CL CO CR CU DM DO EC GD GT GY HN HT JM KN LC MX NI PA PE PR PY " +
		"SR SV TT US UY VC VE"),
	"eic": strings.Fields("AE AF AL AM AO AT AZ BA BE BF BG BH BJ BY CD CF CG CH CI CM CV CY CZ DE DJ DK DZ EE EG ES ET " +
		"FI FR GA GB GE GH GM GN GQ GR HR HU IE IL IQ IR IS IT JO KE KG KW KZ LB LR LT LU LV LY MA MD ME MK ML MR MT " +
		"MU MW NE NG NL NO OM PK PL PS PT QA RO RS RU RW SA SD SE SI SK SL SN SO ST SY TD TG TN TR TZ UA UG UZ XK YE " +
		"ZA ZM"),
}

// Region - регион облака для страны: eic, aic или kic; false - страна не поддерживается.
func Region(country string) (string, bool) {
	for _, r := range []string{"kic", "aic", "eic"} {
		if slices.Contains(regions[r], country) {
			return r, true
		}
	}
	return "", false
}

// APIError - ответ облака с ошибкой.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ThinQ: HTTP %d, код %s", e.Status, e.Code)
}

// ErrNotConfigured - не заданы PAT или страна.
var ErrNotConfigured = errors.New("не заданы thinq.pat и thinq.country")

// Client - запросы к API ThinQ Connect.
type Client struct {
	http *http.Client

	mu       sync.Mutex
	base     string
	pat      string
	country  string
	clientID string
}

// NewClient создаёт клиента. clientID - постоянный идентификатор клиента шлюза.
func NewClient(clientID string) *Client {
	return &Client{http: &http.Client{Timeout: requestTimeout}, clientID: clientID}
}

// NewClientID - новый идентификатор клиента: приставка библиотеки LG и UUID.
func NewClientID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%s%x-%x-%x-%x-%x", clientPrefix, b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Configure задаёт PAT и страну. base - адрес облака; пустой - по региону страны.
func (c *Client) Configure(pat, country, base string) {
	if base == "" {
		if r, ok := Region(country); ok {
			base = "https://api-" + r + ".lgthinq.com"
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pat, c.country, c.base = pat, country, base
}

// Ready сообщает, заданы ли PAT и страна.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pat != "" && c.base != ""
}

func (c *Client) settings() (base, pat, country, clientID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base, c.pat, c.country, c.clientID
}

// messageID - x-message-id: UUID в base64url без дополнения, как в библиотеке LG.
func messageID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// call - запрос к API. Чтение после 5xx или сбоя сети повторяется не больше getRetries раз,
// команды не повторяются. out получает поле response ответа.
func (c *Client) call(ctx context.Context, method, path string, body any, control bool, out any) error {
	base, pat, country, clientID := c.settings()
	if pat == "" || base == "" {
		return ErrNotConfigured
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		status, data, err := c.once(ctx, method, base+"/"+path, payload, pat, country, clientID, control)
		retry := method == http.MethodGet && (err != nil || status >= 500)
		if !retry || attempt >= getRetries || ctx.Err() != nil {
			if err != nil {
				return err
			}
			return decode(status, data, out)
		}
		t := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) once(ctx context.Context, method, u string, payload []byte, pat, country, clientID string, control bool) (int, []byte, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+pat)
	h.Set("x-country", country)
	h.Set("x-message-id", messageID())
	h.Set("x-client-id", clientID)
	h.Set("x-api-key", apiKey)
	h.Set("x-service-phase", "OP")
	h.Set("Accept", "application/json")
	if payload != nil {
		h.Set("Content-Type", "application/json")
	}
	if control {
		h.Set("x-conditional-control", "true")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		metrics.SourceRequest(SourceName, "error")
		return 0, nil, err
	}
	defer resp.Body.Close()
	metrics.SourceRequest(SourceName, strconv.Itoa(resp.StatusCode))
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, data, err
}

func decode(status int, data []byte, out any) error {
	var r struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(data, &r)
	if status >= 400 || r.Error != nil {
		e := &APIError{Status: status}
		if r.Error != nil {
			e.Code, e.Message = r.Error.Code, r.Error.Message
		}
		return e
	}
	if out == nil || len(r.Response) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Response, out); err != nil {
		return fmt.Errorf("ThinQ: ответ не разобран: %w", err)
	}
	return nil
}

// DeviceInfo - прибор аккаунта LG.
type DeviceInfo struct {
	ID    string `json:"deviceId"`
	Type  string `json:"deviceType"`
	Model string `json:"modelName"`
	Alias string `json:"alias"`
}

// Devices - приборы аккаунта.
func (c *Client) Devices(ctx context.Context) ([]DeviceInfo, error) {
	var out []struct {
		DeviceID   string     `json:"deviceId"`
		DeviceInfo DeviceInfo `json:"deviceInfo"`
	}
	if err := c.call(ctx, http.MethodGet, "devices", nil, false, &out); err != nil {
		return nil, err
	}
	list := make([]DeviceInfo, 0, len(out))
	for _, d := range out {
		info := d.DeviceInfo
		info.ID = d.DeviceID
		list = append(list, info)
	}
	return list, nil
}

// Profile - профиль прибора как есть: свойства, их значения и пределы, уведомления.
func (c *Client) Profile(ctx context.Context, deviceID string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.call(ctx, http.MethodGet, "devices/"+url.PathEscape(deviceID)+"/profile", nil, false, &out)
	if err == nil && len(out) == 0 {
		err = errors.New("ThinQ: пустой профиль прибора")
	}
	return out, err
}

// State - состояние прибора как есть: объект или список частей по location.
func (c *Client) State(ctx context.Context, deviceID string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.call(ctx, http.MethodGet, "devices/"+url.PathEscape(deviceID)+"/state", nil, false, &out)
	return out, err
}

// Control отправляет команду прибору. Облако выполняет её, только если прибор может
// её выполнить сейчас: заголовок x-conditional-control.
func (c *Client) Control(ctx context.Context, deviceID string, payload map[string]any) error {
	return c.call(ctx, http.MethodPost, "devices/"+url.PathEscape(deviceID)+"/control", payload, true, nil)
}

// getOnce - чтение без повторов: показание энергии подождёт следующего часа.
func (c *Client) getOnce(ctx context.Context, path string, out any) error {
	base, pat, country, clientID := c.settings()
	if pat == "" || base == "" {
		return ErrNotConfigured
	}
	status, data, err := c.once(ctx, http.MethodGet, base+"/"+path, nil, pat, country, clientID, false)
	if err != nil {
		return err
	}
	return decode(status, data, out)
}

// EnergyProfile - свойства потребления прибора из профиля энергии.
func (c *Client) EnergyProfile(ctx context.Context, deviceID string) ([]string, error) {
	var out struct {
		Result struct {
			Property []string `json:"property"`
		} `json:"result"`
	}
	err := c.getOnce(ctx, "devices/energy/"+url.PathEscape(deviceID)+"/profile", &out)
	return out.Result.Property, err
}

// EnergyUsage - потребление за месяц month вида 200601 в ватт-часах.
func (c *Client) EnergyUsage(ctx context.Context, deviceID, property, month string) (float64, error) {
	var out struct {
		Result struct {
			DataList []map[string]any `json:"dataList"`
		} `json:"result"`
	}
	query := url.Values{"property": {property}, "period": {"MONTHLY"}, "startDate": {month}, "endDate": {month}}
	if err := c.getOnce(ctx, "devices/energy/"+url.PathEscape(deviceID)+"/usage?"+query.Encode(), &out); err != nil {
		return 0, err
	}
	var sum float64
	for _, entry := range out.Result.DataList {
		v, _ := entry[property].(float64)
		sum += v
	}
	if sum < 0 {
		return 0, errors.New("ThinQ: отрицательное потребление")
	}
	return sum, nil
}

// SubscribeEvents подписывает клиента на события прибора на 4464 часа.
func (c *Client) SubscribeEvents(ctx context.Context, deviceID string) error {
	return c.call(ctx, http.MethodPost, "event/"+url.PathEscape(deviceID)+"/subscribe",
		map[string]any{"expire": map[string]any{"unit": "HOUR", "timer": 4464}}, false, nil)
}

// SubscribePush подписывает клиента на уведомления прибора. Подписка, которая уже есть, - не ошибка.
func (c *Client) SubscribePush(ctx context.Context, deviceID string) error {
	err := c.call(ctx, http.MethodPost, "push/"+url.PathEscape(deviceID)+"/subscribe", nil, false, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == codeAlreadySubscribedPush {
		return nil
	}
	return err
}

// Route - адрес сервера MQTT: облако отдаёт его как mqtts://<host>:<port>.
func (c *Client) Route(ctx context.Context) (string, error) {
	var out struct {
		MQTTServer string `json:"mqttServer"`
	}
	if err := c.call(ctx, http.MethodGet, "route", nil, false, &out); err != nil {
		return "", err
	}
	if out.MQTTServer == "" {
		return "", errors.New("ThinQ: в ответе route нет mqttServer")
	}
	return out.MQTTServer, nil
}

// RegisterClient регистрирует клиента MQTT. Клиент, который уже зарегистрирован, - не ошибка.
func (c *Client) RegisterClient(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "client",
		map[string]any{"type": "MQTT", "service-code": "SVC202", "device-type": "607", "allowExist": true}, false, nil)
}

// IssueCertificate получает сертификат клиента MQTT по запросу на подпись csr - PEM без
// первой и последней строки, одной строкой. Возвращает сертификат и топик событий.
func (c *Client) IssueCertificate(ctx context.Context, csr string) (string, string, error) {
	var out struct {
		Result struct {
			CertificatePEM string   `json:"certificatePem"`
			Subscriptions  []string `json:"subscriptions"`
		} `json:"result"`
	}
	if err := c.call(ctx, http.MethodPost, "client/certificate",
		map[string]any{"service-code": "SVC202", "csr": csr}, false, &out); err != nil {
		return "", "", err
	}
	if out.Result.CertificatePEM == "" || len(out.Result.Subscriptions) == 0 {
		return "", "", errors.New("ThinQ: в ответе нет сертификата или топика")
	}
	return out.Result.CertificatePEM, out.Result.Subscriptions[0], nil
}
