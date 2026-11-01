package webos

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// probeTimeout - предел проверки адреса при поиске в сети.
const probeTimeout = 2 * time.Second

// Probe проверяет, что по адресу host - телевизор LG webOS: он отвечает на hello. Модель -
// из сведений о системе, если телевизор отдаёт их без разрешения; иначе пусто. Разрешения
// Probe не просит: на экране телевизора ничего не появляется.
func Probe(ctx context.Context, host string) (model string, err error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	t := &TV{cfg: Config{Host: host}}
	ws, err := t.dial(ctx)
	if err != nil {
		return "", err
	}
	defer ws.Close()
	deadline, _ := ctx.Deadline()
	_ = ws.SetReadDeadline(deadline)
	_ = ws.SetWriteDeadline(deadline)
	if err := ws.WriteJSON(message{Type: "hello", ID: "hello"}); err != nil {
		return "", err
	}
	var m message
	if err := ws.ReadJSON(&m); err != nil {
		return "", err
	}
	if m.Type != "hello" {
		return "", errors.New("не телевизор LG webOS")
	}
	if ws.WriteJSON(message{Type: "request", ID: "sys", URI: uriSystemInfo}) != nil {
		return "", nil
	}
	var r message
	if ws.ReadJSON(&r) != nil || r.ID != "sys" || r.Type != "response" {
		return "", nil
	}
	var info struct {
		ModelName string `json:"modelName"`
	}
	if json.Unmarshal(r.Payload, &info) == nil {
		model = info.ModelName
	}
	return model, nil
}
