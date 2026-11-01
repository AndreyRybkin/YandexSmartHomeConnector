package hcapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Запросы к приборам. Адреса и форматы - по документации и спецификации OpenAPI облака.

// HomeAppliance - прибор аккаунта.
type HomeAppliance struct {
	HaID      string `json:"haId"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Brand     string `json:"brand"`
	Connected bool   `json:"connected"`
}

// Value - состояние, настройка или параметр программы.
type Value struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

// Program - программа и её параметры.
type Program struct {
	Key     string  `json:"key"`
	Options []Value `json:"options,omitempty"`
}

// OptionInfo - параметр программы и его допустимые значения.
type OptionInfo struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Unit        string `json:"unit"`
	Constraints struct {
		Min           *float64 `json:"min"`
		Max           *float64 `json:"max"`
		StepSize      *float64 `json:"stepsize"`
		AllowedValues []string `json:"allowedvalues"`
	} `json:"constraints"`
}

func appliancePath(haID string) string {
	return "/api/homeappliances/" + url.PathEscape(haID)
}

// Appliances - приборы аккаунта.
func (c *Client) Appliances(ctx context.Context) ([]HomeAppliance, error) {
	var out struct {
		Data struct {
			HomeAppliances []HomeAppliance `json:"homeappliances"`
		} `json:"data"`
	}
	err := c.call(ctx, http.MethodGet, "/api/homeappliances", nil, &out)
	return out.Data.HomeAppliances, err
}

// Status - состояния прибора.
func (c *Client) Status(ctx context.Context, haID string) ([]Value, error) {
	var out struct {
		Data struct {
			Status []Value `json:"status"`
		} `json:"data"`
	}
	err := c.call(ctx, http.MethodGet, appliancePath(haID)+"/status", nil, &out)
	return out.Data.Status, err
}

// Settings - настройки прибора.
func (c *Client) Settings(ctx context.Context, haID string) ([]Value, error) {
	var out struct {
		Data struct {
			Settings []Value `json:"settings"`
		} `json:"data"`
	}
	err := c.call(ctx, http.MethodGet, appliancePath(haID)+"/settings", nil, &out)
	return out.Data.Settings, err
}

// SelectedProgram - выбранная программа или nil, если её нет.
func (c *Client) SelectedProgram(ctx context.Context, haID string) (*Program, error) {
	return c.program(ctx, appliancePath(haID)+"/programs/selected")
}

// ActiveProgram - работающая программа или nil, если её нет.
func (c *Client) ActiveProgram(ctx context.Context, haID string) (*Program, error) {
	return c.program(ctx, appliancePath(haID)+"/programs/active")
}

func (c *Client) program(ctx context.Context, path string) (*Program, error) {
	var out struct {
		Data Program `json:"data"`
	}
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil || out.Data.Key == "" {
		return nil, err
	}
	return &out.Data, nil
}

// AvailablePrograms - программы, которые прибор может выполнить сейчас.
func (c *Client) AvailablePrograms(ctx context.Context, haID string) ([]string, error) {
	var out struct {
		Data struct {
			Programs []struct {
				Key string `json:"key"`
			} `json:"programs"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, appliancePath(haID)+"/programs/available", nil, &out); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(out.Data.Programs))
	for _, p := range out.Data.Programs {
		keys = append(keys, p.Key)
	}
	return keys, nil
}

// ProgramOptions - параметры программы и их допустимые значения.
func (c *Client) ProgramOptions(ctx context.Context, haID, program string) ([]OptionInfo, error) {
	var out struct {
		Data struct {
			Options []OptionInfo `json:"options"`
		} `json:"data"`
	}
	err := c.call(ctx, http.MethodGet, appliancePath(haID)+"/programs/available/"+url.PathEscape(program), nil, &out)
	return out.Data.Options, err
}

// Commands - команды прибора, например пауза.
func (c *Client) Commands(ctx context.Context, haID string) ([]string, error) {
	var out struct {
		Data struct {
			Commands []struct {
				Key string `json:"key"`
			} `json:"commands"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, appliancePath(haID)+"/commands", nil, &out); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(out.Data.Commands))
	for _, k := range out.Data.Commands {
		keys = append(keys, k.Key)
	}
	return keys, nil
}

// StartProgram запускает программу. Шестой запуск за минуту не отправляется: ErrBusy.
func (c *Client) StartProgram(ctx context.Context, haID, program string, options []Value) error {
	if !c.lim.action("start") {
		return ErrBusy
	}
	return c.call(ctx, http.MethodPut, appliancePath(haID)+"/programs/active",
		map[string]any{"data": Program{Key: program, Options: options}}, nil)
}

// StopProgram останавливает программу. Шестая остановка за минуту не отправляется: ErrBusy.
func (c *Client) StopProgram(ctx context.Context, haID string) error {
	if !c.lim.action("stop") {
		return ErrBusy
	}
	return c.call(ctx, http.MethodDelete, appliancePath(haID)+"/programs/active", nil, nil)
}

// SelectProgram выбирает программу, не запуская её.
func (c *Client) SelectProgram(ctx context.Context, haID, program string) error {
	return c.call(ctx, http.MethodPut, appliancePath(haID)+"/programs/selected",
		map[string]any{"data": Program{Key: program}}, nil)
}

// SetOption меняет параметр работающей программы.
func (c *Client) SetOption(ctx context.Context, haID, key string, value any) error {
	return c.call(ctx, http.MethodPut, appliancePath(haID)+"/programs/active/options/"+url.PathEscape(key),
		map[string]any{"data": Value{Key: key, Value: value}}, nil)
}

// SetSetting меняет настройку прибора.
func (c *Client) SetSetting(ctx context.Context, haID, key string, value any) error {
	return c.call(ctx, http.MethodPut, appliancePath(haID)+"/settings/"+url.PathEscape(key),
		map[string]any{"data": Value{Key: key, Value: value}}, nil)
}

// Command выполняет команду, например BSH.Common.Command.PauseProgram.
func (c *Client) Command(ctx context.Context, haID, key string) error {
	return c.call(ctx, http.MethodPut, appliancePath(haID)+"/commands/"+url.PathEscape(key),
		map[string]any{"data": Value{Key: key, Value: true}}, nil)
}
