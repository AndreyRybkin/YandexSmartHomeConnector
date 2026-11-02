package hccloud

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

const (
	// maxZipSize - предел размера архива с описанием прибора.
	maxZipSize = 10 << 20
	// maxXMLSize - предел размера одного XML-файла из архива.
	maxXMLSize = 10 << 20
	// maxZipEntries - предел числа файлов в архиве.
	maxZipEntries = 100
	// maxRetries - сколько раз повторять запрос после 5xx или сбоя сети.
	maxRetries = 2
)

// ErrUnauthorized - облако не приняло токен.
var ErrUnauthorized = errors.New("облако Home Connect не приняло токен")

// haIDRe - допустимый haId: он попадает в путь запроса и в имя каталога.
var haIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidHaID сообщает, можно ли использовать haId в путях.
func ValidHaID(id string) bool {
	return haIDRe.MatchString(id)
}

// PairedAppliance - прибор, привязанный к аккаунту.
type PairedAppliance struct {
	HaID   string `json:"haId"`
	Brand  string `json:"brand"`
	HaType string `json:"haType"`
}

// Encryption - ключи для локальной связи с прибором.
// Mode "tls" - TLS-PSK на порту 443, "aes" - AES-CBC с HMAC на порту 80.
type Encryption struct {
	Mode string `json:"mode"`
	Key  string `json:"key"`
	IV   string `json:"iv,omitempty"`
}

// Description - XML-файлы описания прибора из архива.
type Description struct {
	FeatureMapping    []byte
	DeviceDescription []byte
}

// Account - доступ к данным аккаунта на сервере его региона.
type Account struct {
	base  string
	token string
	http  *http.Client
	sleep func(ctx context.Context, d time.Duration) error
}

// Paired - ответ облака со списком приборов аккаунта.
type Paired struct {
	Appliances []PairedAppliance
	// Host - сервер региона, который ответил.
	Host string
	// Raw - ответ как есть, для разбора, если список пуст.
	Raw []byte
}

// Discover находит сервер аккаунта в регионе region и возвращает его приборы.
func Discover(ctx context.Context, region Region, token, subject string) (*Account, *Paired, error) {
	return discover(ctx, region.Hosts, token, subject)
}

func discover(ctx context.Context, hosts []string, token, subject string) (*Account, *Paired, error) {
	if !subjectRe.MatchString(subject) {
		return nil, nil, errors.New("недопустимый идентификатор аккаунта")
	}
	var lastErr error
	for _, host := range hosts {
		a := &Account{base: host, token: token, http: &http.Client{Timeout: requestTimeout}, sleep: sleepCtx}
		raw, err := a.get(ctx, "/api/account/v2/accounts/"+url.PathEscape(subject)+"/paired-appliances", maxJSONSize)
		if err == nil {
			var out struct {
				Appliances []PairedAppliance `json:"appliances"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, nil, fmt.Errorf("ответ облака не JSON: %w", err)
			}
			return a, &Paired{Appliances: out.Appliances, Host: host, Raw: raw}, nil
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, nil, fmt.Errorf("аккаунт не найден в этом регионе: %w", lastErr)
}

// Encryption возвращает ключи прибора.
func (a *Account) Encryption(ctx context.Context, haID string) (*Encryption, error) {
	if !ValidHaID(haID) {
		return nil, errors.New("недопустимый haId")
	}
	var out struct {
		TLS *struct {
			Key string `json:"key"`
		} `json:"tls"`
		AES *struct {
			Key string `json:"key"`
			IV  string `json:"iv"`
		} `json:"aes"`
	}
	if err := a.getJSON(ctx, "/api/appliance/v2/appliances/"+haID+"/encryption-information", &out); err != nil {
		return nil, err
	}
	switch {
	case out.TLS != nil && out.TLS.Key != "":
		return &Encryption{Mode: "tls", Key: out.TLS.Key}, nil
	case out.AES != nil && out.AES.Key != "" && out.AES.IV != "":
		return &Encryption{Mode: "aes", Key: out.AES.Key, IV: out.AES.IV}, nil
	default:
		return nil, errors.New("в ответе облака нет ключей прибора")
	}
}

// Description загружает архив описания прибора и достаёт из него два XML-файла.
func (a *Account) Description(ctx context.Context, haID string) (*Description, error) {
	if !ValidHaID(haID) {
		return nil, errors.New("недопустимый haId")
	}
	data, err := a.get(ctx, "/api/iddf/v1/iddf/"+haID, maxZipSize)
	if err != nil {
		return nil, err
	}
	return unpackDescription(data, haID)
}

func unpackDescription(data []byte, haID string) (*Description, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("архив описания: %w", err)
	}
	if len(zr.File) > maxZipEntries {
		return nil, fmt.Errorf("в архиве описания больше %d файлов", maxZipEntries)
	}
	var d Description
	for _, f := range zr.File {
		var dst *[]byte
		switch f.Name {
		case haID + "_FeatureMapping.xml":
			dst = &d.FeatureMapping
		case haID + "_DeviceDescription.xml":
			dst = &d.DeviceDescription
		default:
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxXMLSize+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > maxXMLSize {
			return nil, fmt.Errorf("%s больше %d байт", f.Name, maxXMLSize)
		}
		*dst = b
	}
	if d.FeatureMapping == nil || d.DeviceDescription == nil {
		return nil, errors.New("в архиве описания нет FeatureMapping.xml или DeviceDescription.xml")
	}
	return &d, nil
}

func (a *Account) getJSON(ctx context.Context, path string, out any) error {
	data, err := a.get(ctx, path, maxJSONSize)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ответ облака не JSON: %w", err)
	}
	return nil
}

// get выполняет GET и повторяет его после 5xx или сбоя сети.
func (a *Account) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		data, status, err := a.getOnce(ctx, path, limit)
		retry := err != nil || status >= 500
		if !retry && status == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		if !retry && status != http.StatusOK {
			return nil, fmt.Errorf("облако Home Connect: HTTP %d", status)
		}
		if !retry {
			return data, nil
		}
		if attempt >= maxRetries || ctx.Err() != nil {
			if err == nil {
				err = fmt.Errorf("облако Home Connect: HTTP %d", status)
			}
			return nil, err
		}
		if err := a.sleep(ctx, time.Duration(2<<attempt)*time.Second); err != nil {
			return nil, err
		}
	}
}

func (a *Account) getOnce(ctx context.Context, path string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > limit {
		return nil, 0, fmt.Errorf("ответ облака больше %d байт", limit)
	}
	return data, resp.StatusCode, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
