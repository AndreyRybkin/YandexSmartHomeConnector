package hclocal

import (
	"context"
	"crypto/aes"
	"encoding/json"
	"errors"
	"fmt"
)

// ModePlain - WebSocket без шифрования, сообщения открытым текстом (ws://, порт 80).
// Прибор в режиме pairing по SAP принимает открытый текст, пока не установлены ключи AES.
const ModePlain = "plain"

// ProbeIdentity - PSK-identity приложения Home Connect по умолчанию.
const ProbeIdentity = identity

// ProbeMessage - сообщение прибора для отладочного вывода.
type ProbeMessage struct {
	SID, MsgID int64
	Resource   string
	Version    int
	Action     string
	Data       []json.RawMessage
	// Code - код ошибки из ответа, если прибор отклонил запрос; иначе пусто.
	Code string
}

// Probe - «сырое» соединение с прибором для отладки pairing. Открывает канал в
// заданном режиме, отвечает на /ei/initialValues как приложение и дальше шлёт
// произвольные сообщения, а все входящие отдаёт в канал Incoming. В отличие от
// Appliance не разбирает состояние прибора и ничего не делает сам.
type Probe struct {
	s *session
	// in - все входящие сообщения прибора; закрывается при обрыве.
	in chan ProbeMessage
	// ready получает номер сессии после ответа на /ei/initialValues.
	ready chan int64
}

// Dial подключается к прибору host в режиме mode (ModePlain, ModeAES, ModeTLS).
// psk нужен для ModeAES и ModeTLS, iv - только для ModeAES. identity - PSK-identity
// для ModeTLS (обычно ProbeIdentity). Порт по умолчанию: 80 для ModePlain и ModeAES,
// 443 для ModeTLS; задать другой можно как host:port.
func Dial(ctx context.Context, host, mode string, psk, iv []byte, identity string) (*Probe, error) {
	switch mode {
	case ModePlain:
	case ModeAES:
		if len(psk) == 0 {
			return nil, errors.New("режим AES: нужен ключ")
		}
		if len(iv) != aes.BlockSize {
			return nil, fmt.Errorf("режим AES: iv должен быть %d байт", aes.BlockSize)
		}
	case ModeTLS:
		if len(psk) == 0 {
			return nil, errors.New("режим TLS: нужен ключ")
		}
	default:
		return nil, fmt.Errorf("режим %q: нужно %s, %s или %s", mode, ModePlain, ModeAES, ModeTLS)
	}
	l, err := dial(ctx, host, keys{mode: mode, psk: psk, iv: iv}, identity)
	if err != nil {
		return nil, err
	}
	p := &Probe{s: newSession(l), in: make(chan ProbeMessage, 64), ready: make(chan int64, 1)}
	go p.readLoop()
	return p, nil
}

func toProbe(m *message) ProbeMessage {
	code := ""
	if len(m.Code) > 0 && string(m.Code) != "null" {
		code = string(m.Code)
	}
	return ProbeMessage{SID: m.SID, MsgID: m.MsgID, Resource: m.Resource, Version: m.Version,
		Action: m.Action, Data: m.Data, Code: code}
}

// readLoop читает сообщения прибора: ответы отдаёт ожидающим запросам, на
// /ei/initialValues отвечает, а каждое сообщение кладёт в канал Incoming.
func (p *Probe) readLoop() {
	s := p.s
	defer close(p.in)
	for {
		data, err := s.link.recv()
		if err != nil {
			s.fail(err)
			return
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Action == actionResponse {
			s.deliver(&m)
		} else if m.Action == actionPost && m.Resource == "/ei/initialValues" {
			p.handleInitial(&m)
		}
		select {
		case p.in <- toProbe(&m):
		case <-s.closed:
			return
		}
	}
}

// handleInitial запоминает номера сессии и отвечает прибору, как приложение.
func (p *Probe) handleInitial(m *message) {
	var iv struct {
		EdMsgID int64 `json:"edMsgID"`
	}
	if len(m.Data) > 0 {
		json.Unmarshal(m.Data[0], &iv)
	}
	p.s.begin(m.SID, iv.EdMsgID)
	var devType any = "Application"
	if m.Version == 1 {
		devType = 2
	}
	p.s.reply(m, map[string]any{"deviceType": devType, "deviceName": deviceName, "deviceID": deviceID})
	select {
	case p.ready <- m.SID:
	default:
	}
}

// Handshake ждёт /ei/initialValues от прибора и возвращает номер сессии. Пока
// прибор его не прислал, запросы шлюза идут с номером сессии 0.
func (p *Probe) Handshake(ctx context.Context) (int64, error) {
	select {
	case sid := <-p.ready:
		return sid, nil
	case <-p.s.closed:
		return 0, p.s.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Incoming - канал всех входящих сообщений прибора; закрывается при обрыве.
func (p *Probe) Incoming() <-chan ProbeMessage { return p.in }

// Send шлёт запрос и не ждёт ответа. data - тело сообщения (объект) или nil;
// прибор заворачивает его в список, как у приложения Home Connect.
func (p *Probe) Send(action, resource string, version int, data any) error {
	return p.s.post(action, resource, version, data)
}

// Call шлёт запрос и ждёт ответа прибора. Если прибор отклонил запрос кодом,
// возвращает сообщение и ошибку с этим кодом.
func (p *Probe) Call(ctx context.Context, action, resource string, version int, data any) (ProbeMessage, error) {
	m, err := p.s.call(ctx, action, resource, version, data)
	if m == nil {
		return ProbeMessage{}, err
	}
	return toProbe(m), err
}

// Err - причина обрыва соединения или nil, если оно ещё живо.
func (p *Probe) Err() error {
	select {
	case <-p.s.closed:
		return p.s.err
	default:
		return nil
	}
}

// Close закрывает соединение.
func (p *Probe) Close() error {
	p.s.fail(errClosed)
	return nil
}
