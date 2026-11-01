package delonghi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/metrics"
)

// Вход в аккаунт De'Longhi - сервис Gigya (SAP Customer Data Cloud), как в приложении
// My Coffee Lounge (com.delonghigroup.daedalus): accounts.login, затем accounts.getJWT.

// DefaultGigyaURL - адрес Gigya приложения.
const DefaultGigyaURL = "https://accounts.eu1.gigya.com"

// gigyaPools - пулы аккаунтов в порядке перебора: аккаунт живёт ровно в одном.
// Ключи - открытые идентификаторы приложения из его манифеста.
var gigyaPools = []struct{ name, apiKey string }{
	{"EU", "4_mXSplGaqrFT0H88TAjqJuA"},
	{"EU_US", "3_e5qn7USZK-QtsIso1wCelqUKAK_IVEsYshRIssQ-X-k55haiZXmKWDHDRul2e5Y2"},
	{"CH", "3_WP_c8OVu_yOoqYXN3Dq-Oi7nNkbS2bwqS3rQXJ6SPkodgE4FOpyuE_UVlrCuSGEm"},
}

const (
	// gigyaWrongPool - Gigya отвечает так, если аккаунта нет в пуле.
	gigyaWrongPool = 400093
	// maxGigyaBody - предел ответа Gigya и облака.
	maxGigyaBody = 1 << 20
)

// gigyaRateLimit - коды «слишком часто»: повторять сразу бесполезно.
var gigyaRateLimit = map[int]bool{403048: true, 403120: true}

// Ошибки входа. Текст Gigya в них не попадает: он уходит только в журнал.
var (
	ErrBadCredentials = errors.New("неверная почта или пароль De'Longhi")
	ErrRateLimited    = errors.New("Gigya просит повторить вход позже")
	ErrNoAccount      = errors.New("аккаунт De'Longhi не найден")
)

// session - сессия Gigya: из неё выпускаются JWT без пароля.
type session struct {
	Pool   string `json:"pool"`
	APIKey string `json:"api_key"`
	Token  string `json:"token"`
	Secret string `json:"secret"`
}

type gigyaResp struct {
	ErrorCode    int    `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
	SessionInfo  struct {
		SessionToken  string `json:"sessionToken"`
		SessionSecret string `json:"sessionSecret"`
	} `json:"sessionInfo"`
	IDToken string `json:"id_token"`
}

// gigyaError - ошибка Gigya с кодом: текст Gigya виден только в журнале.
type gigyaError struct {
	method string
	code   int
	text   string
}

func (e *gigyaError) Error() string {
	return fmt.Sprintf("%s: ошибка Gigya %d: %s", e.method, e.code, e.text)
}

// gigyaCall отправляет запрос Gigya. Свои ошибки Gigya отдаёт с HTTP 200 и errorCode;
// после 5xx и сбоя сети запрос повторяется не больше 2 раз.
func (a *Account) gigyaCall(ctx context.Context, method string, params url.Values) (*gigyaResp, error) {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		r, retry, err := a.gigyaOnce(ctx, method, params)
		if err == nil || !retry {
			return r, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (a *Account) gigyaOnce(ctx context.Context, method string, params url.Values) (*gigyaResp, bool, error) {
	base, _, _ := a.urls()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+method, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		metrics.SourceRequest(Source, "error")
		return nil, true, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	metrics.SourceRequest(Source, strconv.Itoa(resp.StatusCode))
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGigyaBody+1))
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || len(body) > maxGigyaBody {
		return nil, false, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	var r gigyaResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, false, fmt.Errorf("%s: ответ не JSON", method)
	}
	return &r, false, nil
}

// gigyaLogin входит в аккаунт, перебирая пулы.
func (a *Account) gigyaLogin(ctx context.Context, email, password string) (session, error) {
	for _, p := range gigyaPools {
		r, err := a.gigyaCall(ctx, "accounts.login", url.Values{
			"loginID":           {email},
			"password":          {password},
			"apiKey":            {p.apiKey},
			"targetEnv":         {"mobile"},
			"sessionExpiration": {"-1"},
		})
		if err != nil {
			return session{}, err
		}
		switch {
		case r.ErrorCode == 0:
			if r.SessionInfo.SessionToken == "" || r.SessionInfo.SessionSecret == "" {
				return session{}, errors.New("accounts.login: в ответе нет сессии")
			}
			return session{Pool: p.name, APIKey: p.apiKey, Token: r.SessionInfo.SessionToken,
				Secret: r.SessionInfo.SessionSecret}, nil
		case r.ErrorCode == gigyaWrongPool:
			continue
		case gigyaRateLimit[r.ErrorCode]:
			a.log.Warn("De'Longhi: Gigya ограничил частоту входа", "code", r.ErrorCode, "text", r.ErrorMessage)
			return session{}, ErrRateLimited
		default:
			a.log.Warn("De'Longhi: вход не выполнен", "pool", p.name, "code", r.ErrorCode, "text", r.ErrorMessage)
			return session{}, ErrBadCredentials
		}
	}
	return session{}, ErrNoAccount
}

// gigyaJWT выпускает JWT на ttl. Запрос подписывается секретом сессии: без подписи
// Gigya отвечает 403005.
func (a *Account) gigyaJWT(ctx context.Context, s session, ttl time.Duration) (string, time.Time, error) {
	const method = "accounts.getJWT"
	base, _, _ := a.urls()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	params := url.Values{
		"apiKey":      {s.APIKey},
		"oauth_token": {s.Token},
		"expiration":  {strconv.Itoa(int(ttl.Seconds()))},
		"timestamp":   {strconv.FormatInt(a.now().Unix(), 10)},
		"nonce":       {hex.EncodeToString(nonce)},
	}
	sig, err := gigyaSign(http.MethodPost, base+"/"+method, params, s.Secret)
	if err != nil {
		return "", time.Time{}, err
	}
	params.Set("sig", sig)
	r, err := a.gigyaCall(ctx, method, params)
	if err != nil {
		return "", time.Time{}, err
	}
	if r.ErrorCode != 0 {
		return "", time.Time{}, &gigyaError{method: method, code: r.ErrorCode, text: r.ErrorMessage}
	}
	if r.IDToken == "" {
		return "", time.Time{}, errors.New("accounts.getJWT: в ответе нет id_token")
	}
	exp, err := jwtExpiry(r.IDToken)
	if err != nil {
		return "", time.Time{}, err
	}
	return r.IDToken, exp, nil
}

// gigyaSign - подпись OAuth1 HMAC-SHA1 по строке «МЕТОД&адрес&параметры»;
// ключ - секрет сессии, раскодированный из base64.
func gigyaSign(method, rawURL string, params url.Values, secret string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		if key, err = base64.RawStdEncoding.DecodeString(secret); err != nil {
			return "", errors.New("секрет сессии Gigya не в base64")
		}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, rfc3986(k)+"="+rfc3986(params.Get(k)))
	}
	base := strings.ToUpper(method) + "&" + rfc3986(rawURL) + "&" + rfc3986(strings.Join(pairs, "&"))
	mac := hmac.New(sha1.New, key)
	mac.Write([]byte(base))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// rfc3986 кодирует всё, кроме букв, цифр и «-._~», пробел - как %20.
func rfc3986(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// jwtExpiry читает срок JWT из поля exp. Подпись не проверяется: токен проверяют
// облако и машина, шлюзу нужен только срок для продления.
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("токен De'Longhi не похож на JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("токен De'Longhi: поля не в base64")
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(b, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, errors.New("токен De'Longhi: нет срока exp")
	}
	return time.Unix(int64(claims.Exp), 0), nil
}
