package hclocal

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// waitMsg ждёт от проба входящее сообщение, которое проходит cond, не дольше секунды.
func waitMsg(t *testing.T, p *Probe, what string, cond func(ProbeMessage) bool) ProbeMessage {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-p.Incoming():
			if !ok {
				t.Fatalf("соединение закрыто, не дождавшись %s: %v", what, p.Err())
			}
			if cond(m) {
				return m
			}
		case <-timer.C:
			t.Fatalf("не дождались %s", what)
		}
	}
}

// TestProbe проверяет проб во всех режимах: рукопожатие, отправку запроса и
// получение ответа и уведомления прибора.
func TestProbe(t *testing.T) {
	for _, mode := range []string{ModePlain, ModeAES, ModeTLS} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t, mode)
			ctx := context.Background()
			p, err := Dial(ctx, f.addr, mode, testPSK, testIV, identity)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer p.Close()

			sid, err := p.Handshake(ctx)
			if err != nil {
				t.Fatalf("Handshake: %v", err)
			}
			if sid != 7 {
				t.Fatalf("номер сессии %d, ждали 7", sid)
			}
			// Прибор запомнил ответ приложения на /ei/initialValues.
			eventually(t, "ответ на /ei/initialValues", func() bool {
				_, reply := f.last("/ei/initialValues")
				return reply["deviceName"] == deviceName
			})

			if _, err := p.Call(ctx, actionGet, "/ci/services", 1, nil); err != nil {
				t.Fatalf("Call /ci/services: %v", err)
			}
			// /ci/services виден и во входящих как RESPONSE с версиями служб.
			m := waitMsg(t, p, "ответ /ci/services", func(m ProbeMessage) bool {
				return m.Action == actionResponse && m.Resource == "/ci/services"
			})
			if len(m.Data) == 0 {
				t.Fatal("в ответе /ci/services нет данных")
			}

			// Уведомление прибора доходит до проба.
			f.notify(map[int]any{uidOp: 3})
			waitMsg(t, p, "уведомление /ro/values", func(m ProbeMessage) bool {
				return m.Action == actionNotify && m.Resource == "/ro/values"
			})
		})
	}
}

// TestProbeReject проверяет, что код отказа прибора доходит до проба.
func TestProbeReject(t *testing.T) {
	f := newFake(t, ModePlain)
	f.reject = "/ce/hubInformation"
	ctx := context.Background()
	p, err := Dial(ctx, f.addr, ModePlain, nil, nil, identity)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer p.Close()
	if _, err := p.Handshake(ctx); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	data, _ := json.Marshal(map[string]any{"hub": "CN-PRODUCTION"})
	m, err := p.Call(ctx, actionPost, "/ce/hubInformation", 2, json.RawMessage(data))
	if err == nil {
		t.Fatal("ждали ошибку отказа")
	}
	if m.Code != "400" {
		t.Fatalf("код отказа %q, ждали 400", m.Code)
	}
}

// TestProbeDialErrors проверяет отказ Dial на неверных параметрах режимов.
func TestProbeDialErrors(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		mode    string
		psk, iv []byte
	}{
		"неизвестный режим": {mode: "nope"},
		"tls без ключа":     {mode: ModeTLS},
		"aes без ключа":     {mode: ModeAES, iv: testIV},
		"aes без iv":        {mode: ModeAES, psk: testPSK},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Dial(ctx, "127.0.0.1:0", c.mode, c.psk, c.iv, identity); err == nil {
				t.Fatal("ждали ошибку")
			}
		})
	}
}
