package hclocal

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/tlspsk"
)

const (
	// identity - идентификатор PSK приложения Home Connect, как у открытого клиента hcpy.
	identity = "HCCOM_Local_App"
	// wsPath - адрес WebSocket прибора.
	wsPath = "/homeconnect"
	// maxMessage - предел сообщения прибора.
	maxMessage = 1 << 20
	// writeTimeout - предел отправки сообщения.
	writeTimeout = 10 * time.Second
	// macSize - HMAC сообщения в режиме AES, укороченный до 16 байт.
	macSize = 16
)

// Направление в HMAC режима AES: E - от приложения к прибору, C - от прибора к приложению.
const (
	dirApp       = 0x45
	dirAppliance = 0x43
)

// link - соединение WebSocket с прибором: сообщения JSON целиком.
type link struct {
	ws *websocket.Conn
	// mu - отправка: по одному сообщению. В режиме AES от порядка отправки зависит HMAC.
	mu sync.Mutex
	// aes - шифрование сообщений в режиме AES; nil в режиме TLS.
	aes *aesCodec
}

// dial подключается к прибору: wss://host/homeconnect по TLS-PSK или ws://host/homeconnect
// с шифрованием AES или без шифрования. identity - PSK-identity для TLS. Порт по
// умолчанию - 443 и 80.
func dial(ctx context.Context, host string, k keys, identity string) (*link, error) {
	d := websocket.Dialer{HandshakeTimeout: dialTimeout}
	u, origin := "ws://"+host+wsPath, "http://"+host
	if k.mode == ModeTLS {
		u, origin = "wss://"+host+wsPath, "https://"+host
		d.NetDialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tlspsk.Dial(ctx, network, addr, identity, k.psk)
		}
	}
	// Origin - как у клиента hcpy, с которым приборы работают.
	ws, resp, err := d.DialContext(ctx, u, http.Header{"Origin": {origin}})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxMessage)
	l := &link{ws: ws}
	if k.mode == ModeAES {
		if l.aes, err = newAESCodec(k.psk, k.iv, false); err != nil {
			ws.Close()
			return nil, err
		}
	}
	return l, nil
}

func (l *link) send(msg []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	typ := websocket.TextMessage
	if l.aes != nil {
		msg, typ = l.aes.seal(msg), websocket.BinaryMessage
	}
	l.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return l.ws.WriteMessage(typ, msg)
}

// recv читает следующее сообщение. Вызывается из одной горутины.
func (l *link) recv() ([]byte, error) {
	_, data, err := l.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	if l.aes != nil {
		return l.aes.open(data)
	}
	return data, nil
}

func (l *link) ping() error {
	return l.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
}

// aesCodec - шифрование сообщений в режиме AES. Ключи шифрования и HMAC выводятся из PSK
// через HMAC-SHA256 с метками ENC и MAC. Шифр - AES-256-CBC, цепочка блоков продолжается из
// сообщения в сообщение. HMAC-SHA256 укорочен до 16 байт и включает IV, направление и
// предыдущий HMAC того же направления.
type aesCodec struct {
	enc, dec       cipher.BlockMode
	mac, iv        []byte
	lastTx, lastRx []byte
	txDir, rxDir   byte
}

// newAESCodec - шифрование для приложения, а при appliance - для прибора: в тестах.
func newAESCodec(psk, iv []byte, appliance bool) (*aesCodec, error) {
	block, err := aes.NewCipher(sum(psk, []byte("ENC")))
	if err != nil {
		return nil, err
	}
	c := &aesCodec{enc: cipher.NewCBCEncrypter(block, iv), dec: cipher.NewCBCDecrypter(block, iv),
		mac: sum(psk, []byte("MAC")), iv: slices.Clone(iv), lastTx: make([]byte, macSize), lastRx: make([]byte, macSize),
		txDir: dirApp, rxDir: dirAppliance}
	if appliance {
		c.txDir, c.rxDir = dirAppliance, dirApp
	}
	return c, nil
}

func sum(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func (c *aesCodec) tag(dir byte, last, ct []byte) []byte {
	return sum(c.mac, c.iv, []byte{dir}, last, ct)[:macSize]
}

// seal дополняет сообщение до целых блоков - байт 0, случайные байты и длина дополнения
// последним байтом, от 2 до 17 байт, - шифрует и добавляет HMAC.
func (c *aesCodec) seal(msg []byte) []byte {
	pad := aes.BlockSize - len(msg)%aes.BlockSize
	if pad == 1 {
		pad += aes.BlockSize
	}
	buf := make([]byte, len(msg)+pad, len(msg)+pad+macSize)
	copy(buf, msg)
	rand.Read(buf[len(msg)+1 : len(buf)-1])
	buf[len(buf)-1] = byte(pad)
	c.enc.CryptBlocks(buf, buf)
	c.lastTx = c.tag(c.txDir, c.lastTx, buf)
	return append(buf, c.lastTx...)
}

func (c *aesCodec) open(data []byte) ([]byte, error) {
	if len(data) < aes.BlockSize+macSize || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("сообщение AES неверной длины")
	}
	ct, tag := data[:len(data)-macSize], data[len(data)-macSize:]
	if !hmac.Equal(tag, c.tag(c.rxDir, c.lastRx, ct)) {
		return nil, errors.New("HMAC сообщения не сходится: другой ключ или потерянное сообщение")
	}
	c.lastRx = slices.Clone(tag)
	plain := make([]byte, len(ct))
	c.dec.CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > len(plain) {
		return nil, errors.New("неверное дополнение сообщения AES")
	}
	return plain[:len(plain)-pad], nil
}
