package delonghi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// lanPath - WebSocket машины в домашней сети.
	lanPath = "/ws/lan2lan"
	// appID - AppId приложения My Coffee Lounge в командах.
	appID = "MCL"
	// maxLANMessage - предел сообщения машины.
	maxLANMessage = 1 << 20
	// lanTimeout - предел подключения, входа и ответа на команду.
	lanTimeout = 15 * time.Second
)

// Ответы машины на вход.
var (
	errAuthRefused = errors.New("машина не приняла вход: токен или серийный номер не подходят")
	errPinMismatch = errors.New("сертификат машины не совпал с подтверждённым отпечатком")
)

// Fingerprint снимает отпечаток SHA-256 сертификата машины по адресу host: только
// рукопожатие TLS, без токена. Отпечаток - 64 шестнадцатеричных знака заглавными.
func Fingerprint(ctx context.Context, host string) (string, error) {
	var fp string
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: lanTimeout}, Config: &tls.Config{
		// Сертификат машины выдан на «AWS IoT Certificate», а не на её адрес: цепочку не
		// проверить. Отпечаток владелец подтверждает в интерфейсе.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("машина не предъявила сертификат")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			fp = strings.ToUpper(hex.EncodeToString(sum[:]))
			return nil
		},
	}}
	ctx, cancel := context.WithTimeout(ctx, lanTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", withPort(host))
	if err != nil {
		return "", err
	}
	conn.Close()
	return fp, nil
}

func withPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), "443")
}

// lanConn - вход в машину по домашней сети: сообщения JSON целиком.
type lanConn struct {
	ws *websocket.Conn
	id int
	// mu - отправка по одному сообщению.
	mu sync.Mutex
	// waiting - ответы на команды по RequestId.
	wmu     sync.Mutex
	waiting map[string]chan map[string]any
}

// dialLAN подключается к машине, сверяет сертификат с pin и входит с токеном.
// Без совпадения отпечатка токен не уходит.
func dialLAN(ctx context.Context, host string, pin []byte, serial, token string) (*lanConn, error) {
	d := websocket.Dialer{HandshakeTimeout: lanTimeout, TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("машина не предъявила сертификат")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return errPinMismatch
			}
			return nil
		},
	}}
	// Порт по умолчанию 443 gorilla добавляет сам: заголовок Host - как у приложения, без порта.
	ws, resp, err := d.DialContext(ctx, "wss://"+host+lanPath, http.Header{})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		if errors.Is(err, errPinMismatch) {
			return nil, errPinMismatch
		}
		return nil, err
	}
	ws.SetReadLimit(maxLANMessage)
	c := &lanConn{ws: ws, waiting: map[string]chan map[string]any{}}
	// Приложение шлёт auth строчными: на "AUTH" машина всегда отвечает UNAUTHORIZED.
	if err := c.send(map[string]any{"Message": "auth", "SerialNo": serial, "AuthToken": token}); err != nil {
		ws.Close()
		return nil, err
	}
	_ = ws.SetReadDeadline(time.Now().Add(lanTimeout))
	var m struct {
		Message      string `json:"Message"`
		Response     string `json:"Response"`
		ConnectionID *int   `json:"ConnectionId"`
	}
	if err := ws.ReadJSON(&m); err != nil {
		ws.Close()
		return nil, fmt.Errorf("ответ на вход: %w", err)
	}
	_ = ws.SetReadDeadline(time.Time{})
	if m.Message != "auth" || m.Response != "OK" || m.ConnectionID == nil {
		ws.Close()
		return nil, errAuthRefused
	}
	c.id = *m.ConnectionID
	return c, nil
}

func (c *lanConn) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(lanTimeout))
	return c.ws.WriteJSON(v)
}

func (c *lanConn) close() {
	c.ws.Close()
}

// command отправляет команду и ждёт ответа с тем же RequestId.
func (c *lanConn) command(ctx context.Context, msg map[string]any) (map[string]any, error) {
	id := requestID()
	msg["AppId"], msg["RequestId"], msg["ConnectionId"] = appID, id, c.id
	ch := make(chan map[string]any, 1)
	c.wmu.Lock()
	c.waiting[id] = ch
	c.wmu.Unlock()
	defer func() {
		c.wmu.Lock()
		delete(c.waiting, id)
		c.wmu.Unlock()
	}()
	if err := c.send(msg); err != nil {
		return nil, err
	}
	t := time.NewTimer(lanTimeout)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, errors.New("машина не ответила на команду")
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("соединение с машиной закрылось")
		}
		return r, nil
	}
}

// read читает сообщения машины: ответы на команды отдаёт ждущим, остальное - в handle.
// Возвращается, когда соединение закрылось.
func (c *lanConn) read(handle func(map[string]any)) error {
	defer func() {
		c.wmu.Lock()
		for id, ch := range c.waiting {
			close(ch)
			delete(c.waiting, id)
		}
		c.wmu.Unlock()
	}()
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return err
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if id, ok := m["RequestId"].(string); ok {
			c.wmu.Lock()
			ch := c.waiting[id]
			delete(c.waiting, id)
			c.wmu.Unlock()
			if ch != nil {
				ch <- m
				continue
			}
		}
		handle(m)
	}
}

// requestID - 5 случайных букв и цифр, как RequestId приложения.
func requestID() string {
	const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = abc[int(b[i])%len(abc)]
	}
	return string(b)
}

func hexDecode(s string) ([]byte, error) {
	return hex.DecodeString(s)
}
