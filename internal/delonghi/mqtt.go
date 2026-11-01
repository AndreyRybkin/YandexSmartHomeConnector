package delonghi

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/metrics"
)

// Облако AWS IoT, как у приложения: MQTT 5 поверх WebSocket, авторизатор с JWT вместо
// пароля. Шлюз только читает именованную тень машины: подписывается на ответ, просит
// тень и отключается.

const (
	// DefaultIoTURL - брокер AWS IoT приложения (prod EU).
	DefaultIoTURL = "wss://a2612mo23mfrw1-ats.iot.eu-central-1.amazonaws.com/mqtt"
	// iotAuthorizer - авторизатор брокера: имя пользователя MQTT и параметр адреса.
	iotAuthorizer = "dlg-prod-token-authorizer"
	// maxMQTTPacket - предел пакета MQTT.
	maxMQTTPacket = 1 << 20
)

// wsReader склеивает двоичные сообщения WebSocket в поток для MQTT: пакет может лежать в
// нескольких сообщениях, а сообщение - нести несколько пакетов.
type wsReader struct {
	ws  *websocket.Conn
	cur io.Reader
}

func (r *wsReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			_, rd, err := r.ws.NextReader()
			if err != nil {
				return 0, err
			}
			r.cur = rd
		}
		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			r.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

type mqttConn struct {
	ws   *websocket.Conn
	r    *bufio.Reader
	wmu  sync.Mutex
	stop func() bool
}

func appendVarInt(b []byte, n int) []byte {
	for {
		c := byte(n % 128)
		n /= 128
		if n > 0 {
			c |= 0x80
		}
		b = append(b, c)
		if n == 0 {
			return b
		}
	}
}

func readVarInt(b []byte) (int, int, error) {
	n, mul := 0, 1
	for i := 0; i < 4 && i < len(b); i++ {
		n += int(b[i]&0x7F) * mul
		if b[i]&0x80 == 0 {
			return n, i + 1, nil
		}
		mul *= 128
	}
	return 0, 0, errors.New("MQTT: неверное переменное число")
}

func appendStr(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

func (m *mqttConn) send(typ byte, body []byte) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	_ = m.ws.SetWriteDeadline(time.Now().Add(lanTimeout))
	return m.ws.WriteMessage(websocket.BinaryMessage, append(appendVarInt([]byte{typ}, len(body)), body...))
}

func (m *mqttConn) recv() (byte, byte, []byte, error) {
	h, err := m.r.ReadByte()
	if err != nil {
		return 0, 0, nil, err
	}
	n, mul := 0, 1
	for i := 0; ; i++ {
		c, err := m.r.ReadByte()
		if err != nil {
			return 0, 0, nil, err
		}
		n += int(c&0x7F) * mul
		if c&0x80 == 0 {
			break
		}
		if mul *= 128; i == 3 {
			return 0, 0, nil, errors.New("MQTT: длина пакета больше 4 байт")
		}
	}
	if n > maxMQTTPacket {
		return 0, 0, nil, fmt.Errorf("MQTT: пакет %d байт больше предела", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(m.r, body); err != nil {
		return 0, 0, nil, err
	}
	return h & 0xF0, h & 0x0F, body, nil
}

func randomClientID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0F | 0x40
	b[8] = b[8]&0x3F | 0x80
	h := hex.EncodeToString(b)
	return "dlg-appliance-kit-android-" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// openIoT подключается к брокеру с JWT. Свой clientId - чтобы брокер не отключил
// приложение с тем же идентификатором. Соединение закрывается, когда ctx отменён.
func (a *Account) openIoT(ctx context.Context) (*mqttConn, error) {
	token, err := a.Token(ctx)
	if err != nil {
		return nil, err
	}
	_, _, iot := a.urls()
	dctx, cancel := context.WithTimeout(ctx, lanTimeout)
	defer cancel()
	d := websocket.Dialer{HandshakeTimeout: lanTimeout, Subprotocols: []string{"mqtt"},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	ws, resp, err := d.DialContext(dctx, iot+"?x-amz-customauthorizer-name="+iotAuthorizer, http.Header{})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		metrics.SourceRequest(Source, "error")
		return nil, fmt.Errorf("AWS IoT: %w", err)
	}
	ws.SetReadLimit(maxMQTTPacket)
	m := &mqttConn{ws: ws, r: bufio.NewReader(&wsReader{ws: ws})}
	m.stop = context.AfterFunc(ctx, func() { ws.Close() })
	body := appendStr(nil, "MQTT")
	body = append(body, 5, 0xC2) // версия 5; имя, пароль, чистый старт
	body = binary.BigEndian.AppendUint16(body, 60)
	body = append(body, 0) // без свойств
	body = appendStr(body, randomClientID())
	body = appendStr(body, "x-amz-customauthorizer-name="+iotAuthorizer)
	body = appendStr(body, token)
	_ = ws.SetReadDeadline(time.Now().Add(lanTimeout))
	if err := m.send(0x10, body); err != nil {
		m.close()
		return nil, err
	}
	typ, _, p, err := m.recv()
	if err != nil {
		m.close()
		return nil, fmt.Errorf("AWS IoT: нет CONNACK: %w", err)
	}
	_ = ws.SetReadDeadline(time.Time{})
	if typ != 0x20 || len(p) < 2 {
		m.close()
		return nil, fmt.Errorf("AWS IoT: вместо CONNACK пакет %#x", typ)
	}
	metrics.SourceRequest(Source, fmt.Sprintf("mqtt_%#x", p[1]))
	if p[1] != 0 {
		m.close()
		return nil, fmt.Errorf("AWS IoT отказал в подключении: код %#x", p[1])
	}
	return m, nil
}

// close отключается от брокера.
func (m *mqttConn) close() {
	_ = m.send(0xE0, []byte{0})
	if m.stop != nil {
		m.stop()
	}
	m.ws.Close()
}

// subscribe подписывается на точные топики с QoS 0: фильтры с + и # брокер не разрешает.
// SUBACK разбирает читатель.
func (m *mqttConn) subscribe(topics ...string) error {
	sub := binary.BigEndian.AppendUint16(nil, 1)
	sub = append(sub, 0)
	for _, t := range topics {
		sub = append(appendStr(sub, t), 0)
	}
	return m.send(0x82, sub)
}

func (m *mqttConn) publish(topic string, payload []byte) error {
	pub := append(appendStr(nil, topic), 0)
	return m.send(0x30, append(pub, payload...))
}

// shadowDoc - reported тени и время документа: у get/accepted - state и timestamp, у
// update/documents - current.state и timestamp.
func shadowDoc(payload []byte) (map[string]any, time.Time, bool) {
	var doc struct {
		State struct {
			Reported map[string]any `json:"reported"`
		} `json:"state"`
		Current struct {
			State struct {
				Reported map[string]any `json:"reported"`
			} `json:"state"`
		} `json:"current"`
		Timestamp float64 `json:"timestamp"`
	}
	if json.Unmarshal(payload, &doc) != nil {
		return nil, time.Time{}, false
	}
	r := doc.State.Reported
	if r == nil {
		r = doc.Current.State.Reported
	}
	if r == nil {
		return nil, time.Time{}, false
	}
	var ts time.Time
	if doc.Timestamp > 0 {
		ts = time.Unix(int64(doc.Timestamp), 0)
	}
	return r, ts, true
}

func shadowTopic(thing, name string) string {
	return "$aws/things/" + thing + "/shadow/name/" + name
}

// readShadow читает reported именованной тени машины thing и отключается.
func (a *Account) readShadow(ctx context.Context, thing, name string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, lanTimeout)
	defer cancel()
	m, err := a.openIoT(ctx)
	if err != nil {
		return nil, err
	}
	defer m.close()
	base := shadowTopic(thing, name) + "/get"
	if err := m.subscribe(base+"/accepted", base+"/rejected"); err != nil {
		return nil, err
	}
	sent := false
	for {
		typ, flags, p, err := m.recv()
		if err != nil {
			return nil, fmt.Errorf("AWS IoT: %w", err)
		}
		switch typ {
		case 0x90: // SUBACK: подписка есть - просим тень
			if sent {
				continue
			}
			if code := subackCode(p); code >= 0x80 {
				metrics.SourceRequest(Source, fmt.Sprintf("mqtt_%#x", code))
				return nil, fmt.Errorf("AWS IoT: подписка отклонена, код %#x", code)
			}
			if err := m.publish(base, []byte("{}")); err != nil {
				return nil, err
			}
			sent = true
		case 0x30:
			topic, payload, ok := parsePublish(p, flags)
			if !ok {
				continue
			}
			if topic == base+"/rejected" {
				return nil, fmt.Errorf("AWS IoT: тени %s нет", name)
			}
			if topic != base+"/accepted" {
				continue
			}
			r, _, ok := shadowDoc(payload)
			if !ok {
				return nil, errors.New("AWS IoT: тень не в ожидаемом виде")
			}
			return r, nil
		case 0xE0:
			return nil, errors.New("AWS IoT: брокер отключил")
		}
	}
}

// watchShadow держит одно соединение, пока until() не прошло: каждые every просит тень
// name и отдаёт в apply ответы и изменения тени со временем документа.
func (a *Account) watchShadow(ctx context.Context, thing, name string, every time.Duration,
	until func() time.Time, apply func(map[string]any, time.Time)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m, err := a.openIoT(ctx)
	if err != nil {
		return err
	}
	defer m.close()
	base := shadowTopic(thing, name)
	if err := m.subscribe(base+"/get/accepted", base+"/update/documents"); err != nil {
		return err
	}
	type packet struct {
		typ, flags byte
		p          []byte
	}
	// Чтение - в своей горутине: после таймаута чтения соединение gorilla не годится.
	packets, errc := make(chan packet, 16), make(chan error, 1)
	go func() {
		for {
			typ, flags, p, err := m.recv()
			if err != nil {
				errc <- err
				return
			}
			select {
			case packets <- packet{typ, flags, p}:
			case <-ctx.Done():
				return
			}
		}
	}()
	tick := time.NewTicker(every)
	defer tick.Stop()
	ping := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return fmt.Errorf("AWS IoT: %w", err)
		case pk := <-packets:
			switch pk.typ {
			case 0x90:
				if code := subackCode(pk.p); code >= 0x80 {
					metrics.SourceRequest(Source, fmt.Sprintf("mqtt_%#x", code))
					return fmt.Errorf("AWS IoT: подписка отклонена, код %#x", code)
				}
				if err := m.publish(base+"/get", []byte("{}")); err != nil {
					return err
				}
			case 0x30:
				if _, payload, ok := parsePublish(pk.p, pk.flags); ok {
					if r, ts, ok := shadowDoc(payload); ok {
						apply(r, ts)
					}
				}
			case 0xE0:
				return errors.New("AWS IoT: брокер отключил")
			}
		case now := <-tick.C:
			if !now.Before(until()) {
				return nil
			}
			if err := m.publish(base+"/get", []byte("{}")); err != nil {
				return err
			}
			// Запросы и так идут чаще keep alive 60 с; PINGREQ - на случай длинного окна.
			if now.Sub(ping) > 30*time.Second {
				_ = m.send(0xC0, nil)
				ping = now
			}
		}
	}
}

// subackCode - худший код SUBACK: от 0x80 - отказ.
func subackCode(p []byte) byte {
	if len(p) < 3 {
		return 0xFF
	}
	n, l, err := readVarInt(p[2:])
	if err != nil || 2+l+n >= len(p) {
		return 0xFF
	}
	var worst byte
	for _, c := range p[2+l+n:] {
		worst = max(worst, c)
	}
	return worst
}

// parsePublish разбирает PUBLISH с QoS 0: подписка шлюза - с QoS 0.
func parsePublish(p []byte, flags byte) (string, []byte, bool) {
	if len(p) < 2 || (flags>>1)&3 != 0 {
		return "", nil, false
	}
	tl := int(binary.BigEndian.Uint16(p))
	if 2+tl > len(p) {
		return "", nil, false
	}
	topic, rest := string(p[2:2+tl]), p[2+tl:]
	n, l, err := readVarInt(rest)
	if err != nil || l+n > len(rest) {
		return "", nil, false
	}
	return topic, rest[l+n:], true
}
