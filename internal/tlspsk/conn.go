// Package tlspsk - соединение TLS 1.2 с общим ключом (PSK) вместо сертификата: набор
// TLS_ECDHE_PSK_WITH_CHACHA20_POLY1305_SHA256 из RFC 5489 и RFC 7905. Такие соединения
// принимает прибор Home Connect в домашней сети, а crypto/tls наборов PSK не знает.
// Возобновления сессий и повторного рукопожатия нет: прибору они не нужны.
package tlspsk

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	recordChangeCipherSpec = 20
	recordAlert            = 21
	recordHandshake        = 22
	recordApplicationData  = 23

	// version12 - TLS 1.2; version10 - версия записи ClientHello, как у других клиентов.
	version12 = 0x0303
	version10 = 0x0301

	// suiteECDHEPSKChaCha - TLS_ECDHE_PSK_WITH_CHACHA20_POLY1305_SHA256.
	suiteECDHEPSKChaCha = 0xccac
	// suiteRenegotiationSCSV - TLS_EMPTY_RENEGOTIATION_INFO_SCSV: повторного рукопожатия нет.
	suiteRenegotiationSCSV = 0x00ff

	// maxPlaintext - предел данных в записи; зашифрованная запись может быть длиннее.
	maxPlaintext = 1 << 14
	maxRecord    = maxPlaintext + 2048
	// maxHandshake - предел сообщения рукопожатия: у этого набора они не длиннее сотен байт.
	maxHandshake = 8 << 10
	// tagSize - метка ChaCha20-Poly1305.
	tagSize = 16
	// closeTimeout - сколько Close ждёт отправки close_notify.
	closeTimeout = time.Second
)

// alert - код предупреждения TLS.
type alert uint8

const (
	alertCloseNotify        alert = 0
	alertUnexpectedMessage  alert = 10
	alertBadRecordMAC       alert = 20
	alertRecordOverflow     alert = 22
	alertHandshakeFailure   alert = 40
	alertIllegalParameter   alert = 47
	alertDecodeError        alert = 50
	alertDecryptError       alert = 51
	alertProtocolVersion    alert = 70
	alertInternalError      alert = 80
	alertUnknownPSKIdentity alert = 115
)

var alertNames = map[alert]string{
	alertCloseNotify: "close_notify", alertUnexpectedMessage: "unexpected_message", alertBadRecordMAC: "bad_record_mac",
	alertRecordOverflow: "record_overflow", alertHandshakeFailure: "handshake_failure", 42: "bad_certificate",
	alertIllegalParameter: "illegal_parameter", alertDecodeError: "decode_error", alertDecryptError: "decrypt_error",
	alertProtocolVersion: "protocol_version", 71: "insufficient_security", alertInternalError: "internal_error",
	90: "user_canceled", 100: "no_renegotiation", 110: "unsupported_extension", alertUnknownPSKIdentity: "unknown_psk_identity",
}

func (a alert) String() string {
	if name, ok := alertNames[a]; ok {
		return name
	}
	return fmt.Sprintf("alert %d", uint8(a))
}

// AlertError - другая сторона прервала соединение предупреждением TLS.
type AlertError struct {
	Code uint8
}

func (e *AlertError) Error() string {
	msg := "tlspsk: другая сторона прислала предупреждение " + alert(e.Code).String()
	if e.wrongKey() {
		msg += ": неверный ключ PSK или идентификатор"
	}
	return msg
}

func (e *AlertError) wrongKey() bool {
	switch alert(e.Code) {
	case alertBadRecordMAC, alertDecryptError, alertUnknownPSKIdentity:
		return true
	}
	return false
}

// WrongKey - сервер говорит TLS-PSK, но не принял ключ или идентификатор: он выбрал набор
// с PSK и только потом прервал рукопожатие. Обычный сервер TLS прерывает его раньше.
func WrongKey(err error) bool {
	var a *AlertError
	return errors.As(err, &a) && a.wrongKey()
}

// localError - нарушение протокола, найденное этой стороной: другая сторона получает предупреждение.
type localError struct {
	alert alert
	msg   string
}

func (e *localError) Error() string {
	return "tlspsk: " + e.msg
}

var errNoHandshake = errors.New("tlspsk: рукопожатие не выполнено")

// half - шифрование одного направления: ключ, IV и номер следующей записи.
type half struct {
	aead cipher.AEAD
	iv   [12]byte
	seq  uint64
}

func newHalf(key, iv []byte) (*half, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	h := &half{aead: aead}
	copy(h.iv[:], iv)
	return h, nil
}

// nonce - IV, сложенный по XOR с номером записи (RFC 7905, раздел 2).
func (h *half) nonce() []byte {
	n := h.iv
	for i := range 8 {
		n[4+i] ^= byte(h.seq >> (56 - 8*i))
	}
	return n[:]
}

// additional - дополнительные данные AEAD: номер записи, тип, версия и длина открытых данных.
func additional(seq uint64, typ byte, n int) []byte {
	var ad [13]byte
	binary.BigEndian.PutUint64(ad[:8], seq)
	ad[8] = typ
	binary.BigEndian.PutUint16(ad[9:], version12)
	binary.BigEndian.PutUint16(ad[11:], uint16(n))
	return ad[:]
}

func (h *half) seal(dst []byte, typ byte, plain []byte) ([]byte, error) {
	if h.seq == math.MaxUint64 {
		return nil, errors.New("tlspsk: номера записей исчерпаны")
	}
	dst = h.aead.Seal(dst, h.nonce(), plain, additional(h.seq, typ, len(plain)))
	h.seq++
	return dst, nil
}

func (h *half) open(typ byte, payload []byte) ([]byte, error) {
	if len(payload) < tagSize {
		return nil, errors.New("запись короче метки")
	}
	plain, err := h.aead.Open(payload[:0], h.nonce(), payload, additional(h.seq, typ, len(payload)-tagSize))
	if err != nil {
		return nil, err
	}
	h.seq++
	return plain, nil
}

// Conn - соединение TLS-PSK. Read и Write можно вызывать из разных горутин. Таймаут чтения
// не портит соединение: недочитанная запись остаётся в буфере. Остальные ошибки чтения и
// любая ошибка записи, в том числе таймаут, окончательны.
type Conn struct {
	conn     net.Conn
	client   bool
	identity string
	psk      []byte
	server   ServerConfig

	done atomic.Bool

	in struct {
		sync.Mutex
		cipher *half
		// raw - прочитанные из соединения байты, ещё не разобранные в записи.
		raw []byte
		// hs - байты рукопожатия, ещё не собранные в сообщение; data - данные, ещё не отданные Read.
		hs   []byte
		data []byte
		err  error
	}
	out struct {
		sync.Mutex
		cipher  *half
		version uint16
		err     error
	}
	// transcript - сообщения рукопожатия для Finished и extended master secret.
	transcript []byte
}

// Client - сторона клиента поверх conn. Перед Read и Write нужен Handshake.
func Client(conn net.Conn, identity string, psk []byte) *Conn {
	c := &Conn{conn: conn, client: true, identity: identity, psk: slices.Clone(psk)}
	c.out.version = version10
	return c
}

// Server - сторона сервера поверх conn. Перед Read и Write нужен Handshake.
func Server(conn net.Conn, cfg ServerConfig) *Conn {
	c := &Conn{conn: conn, identity: cfg.Identity, psk: slices.Clone(cfg.PSK), server: cfg}
	c.out.version = version12
	return c
}

// Dial подключается к addr и выполняет рукопожатие клиента. ctx ограничивает и то, и другое.
func Dial(ctx context.Context, network, addr, identity string, psk []byte) (*Conn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	c := Client(raw, identity, psk)
	if err := c.Handshake(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

// Handshake выполняет рукопожатие. Срок или отмена ctx прерывают его.
func (c *Conn) Handshake(ctx context.Context) error {
	c.in.Lock()
	defer c.in.Unlock()
	c.out.Lock()
	defer c.out.Unlock()
	if c.done.Load() {
		return nil
	}
	if c.in.err != nil {
		return c.in.err
	}
	if d, ok := ctx.Deadline(); ok {
		c.conn.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Unix(1, 0)) })
	var err error
	if c.client {
		err = c.clientHandshake()
	} else {
		err = c.serverHandshake()
	}
	stop()
	c.conn.SetDeadline(time.Time{})
	c.transcript = nil
	if err != nil {
		var le *localError
		if errors.As(err, &le) {
			c.sendAlertLocked(le.alert)
		}
		// Срок соединения - срок ctx, но таймаут сокета может сработать чуть раньше таймера ctx.
		ctxErr := ctx.Err()
		if d, ok := ctx.Deadline(); ctxErr == nil && ok && !time.Now().Before(d) {
			ctxErr = context.DeadlineExceeded
		}
		if ctxErr != nil {
			err = fmt.Errorf("tlspsk: рукопожатие прервано: %w: %w", ctxErr, err)
		}
		c.in.err, c.out.err = err, err
		return err
	}
	c.done.Store(true)
	return nil
}

// fillLocked читает соединение, пока в буфере не наберётся n байт. Прочитанное до ошибки
// остаётся в буфере: после таймаута чтение продолжается с того же места.
func (c *Conn) fillLocked(n int) error {
	if cap(c.in.raw) < n {
		raw := make([]byte, len(c.in.raw), max(n, 2*cap(c.in.raw), 4096))
		copy(raw, c.in.raw)
		c.in.raw = raw
	}
	for len(c.in.raw) < n {
		m, err := c.conn.Read(c.in.raw[len(c.in.raw):cap(c.in.raw)])
		c.in.raw = c.in.raw[:len(c.in.raw)+m]
		switch {
		case err == nil, len(c.in.raw) >= n:
		case errors.Is(err, io.EOF) && len(c.in.raw) > 0:
			return io.ErrUnexpectedEOF
		default:
			return err
		}
	}
	return nil
}

// readRecordLocked читает запись и расшифровывает её, если чтение уже зашифровано.
func (c *Conn) readRecordLocked() (byte, []byte, error) {
	if err := c.fillLocked(5); err != nil {
		return 0, nil, err
	}
	typ, n := c.in.raw[0], int(binary.BigEndian.Uint16(c.in.raw[3:5]))
	switch {
	case c.in.raw[1] != 3:
		return 0, nil, &localError{alertProtocolVersion, "ответ не похож на TLS"}
	case n > maxRecord, c.in.cipher == nil && n > maxPlaintext:
		return 0, nil, &localError{alertRecordOverflow, "запись больше предела"}
	}
	if err := c.fillLocked(5 + n); err != nil {
		return 0, nil, err
	}
	data := slices.Clone(c.in.raw[5 : 5+n])
	c.in.raw = c.in.raw[:copy(c.in.raw, c.in.raw[5+n:])]
	if h := c.in.cipher; h != nil {
		var err error
		if data, err = h.open(typ, data); err != nil {
			return 0, nil, &localError{alertBadRecordMAC, "запись не расшифрована: другой ключ или повреждённые данные"}
		}
		if len(data) > maxPlaintext {
			return 0, nil, &localError{alertRecordOverflow, "запись больше предела"}
		}
	}
	return typ, data, nil
}

// readHandshakeLocked возвращает следующее сообщение рукопожатия целиком, с типом и длиной.
func (c *Conn) readHandshakeLocked() ([]byte, error) {
	for {
		if hs := c.in.hs; len(hs) >= 4 {
			n := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
			if n > maxHandshake {
				return nil, &localError{alertDecodeError, "сообщение рукопожатия больше предела"}
			}
			if len(hs) >= 4+n {
				c.in.hs = hs[4+n:]
				return hs[: 4+n : 4+n], nil
			}
		}
		typ, data, err := c.readRecordLocked()
		if err != nil {
			return nil, err
		}
		switch {
		case typ == recordAlert:
			return nil, alertError(data)
		case typ != recordHandshake || len(data) == 0:
			return nil, &localError{alertUnexpectedMessage, fmt.Sprintf("запись типа %d посреди рукопожатия", typ)}
		}
		c.in.hs = append(c.in.hs, data...)
	}
}

func (c *Conn) readChangeCipherSpecLocked() error {
	if len(c.in.hs) != 0 {
		return &localError{alertUnexpectedMessage, "ChangeCipherSpec посреди сообщения рукопожатия"}
	}
	typ, data, err := c.readRecordLocked()
	switch {
	case err != nil:
		return err
	case typ == recordAlert:
		return alertError(data)
	case typ != recordChangeCipherSpec:
		return &localError{alertUnexpectedMessage, fmt.Sprintf("запись типа %d вместо ChangeCipherSpec", typ)}
	case len(data) != 1 || data[0] != 1:
		return &localError{alertDecodeError, "неверный ChangeCipherSpec"}
	}
	return nil
}

func alertError(data []byte) error {
	switch {
	case len(data) != 2:
		return &localError{alertDecodeError, "неверная запись предупреждения"}
	case alert(data[1]) == alertCloseNotify:
		return io.EOF
	}
	return &AlertError{Code: data[1]}
}

// appendRecordLocked добавляет к buf записи с data, по maxPlaintext байт в записи.
func (c *Conn) appendRecordLocked(buf []byte, typ byte, data []byte) ([]byte, error) {
	for {
		n := min(len(data), maxPlaintext)
		chunk := data[:n]
		data = data[n:]
		start := len(buf)
		buf = append(buf, typ, byte(c.out.version>>8), byte(c.out.version), 0, 0)
		if h := c.out.cipher; h != nil {
			var err error
			if buf, err = h.seal(buf, typ, chunk); err != nil {
				return nil, err
			}
		} else {
			buf = append(buf, chunk...)
		}
		binary.BigEndian.PutUint16(buf[start+3:], uint16(len(buf)-start-5))
		if len(data) == 0 {
			return buf, nil
		}
	}
}

func (c *Conn) writeRecordLocked(typ byte, data []byte) error {
	buf, err := c.appendRecordLocked(nil, typ, data)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(buf)
	return err
}

func (c *Conn) sendAlertLocked(a alert) {
	level := byte(2)
	if a == alertCloseNotify {
		level = 1
	}
	c.writeRecordLocked(recordAlert, []byte{level, byte(a)})
}

func (c *Conn) Read(b []byte) (int, error) {
	if !c.done.Load() {
		return 0, errNoHandshake
	}
	if len(b) == 0 {
		return 0, nil
	}
	c.in.Lock()
	defer c.in.Unlock()
	for len(c.in.data) == 0 {
		if c.in.err != nil {
			return 0, c.in.err
		}
		if err := c.readDataLocked(); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return 0, err
			}
			var le *localError
			if errors.As(err, &le) {
				c.out.Lock()
				c.sendAlertLocked(le.alert)
				c.out.Unlock()
			}
			c.in.err = err
		}
	}
	n := copy(b, c.in.data)
	c.in.data = c.in.data[n:]
	return n, nil
}

// readDataLocked читает следующую запись после рукопожатия.
func (c *Conn) readDataLocked() error {
	typ, data, err := c.readRecordLocked()
	if err != nil {
		return err
	}
	switch typ {
	case recordApplicationData:
		c.in.data = data
		return nil
	case recordAlert:
		return alertError(data)
	case recordHandshake:
		// HelloRequest сервера клиент вправе пропустить (RFC 5246, раздел 7.4.1.1); другого рукопожатия нет.
		c.in.hs = append(c.in.hs, data...)
		for len(c.in.hs) >= 4 {
			if !c.client || !bytes.Equal(c.in.hs[:4], []byte{typeHelloRequest, 0, 0, 0}) {
				return &localError{alertUnexpectedMessage, "сообщение рукопожатия после рукопожатия"}
			}
			c.in.hs = c.in.hs[4:]
		}
		return nil
	}
	return &localError{alertUnexpectedMessage, fmt.Sprintf("запись типа %d", typ)}
}

func (c *Conn) Write(b []byte) (int, error) {
	if !c.done.Load() {
		return 0, errNoHandshake
	}
	if len(b) == 0 {
		return 0, nil
	}
	c.out.Lock()
	defer c.out.Unlock()
	if c.out.err != nil {
		return 0, c.out.err
	}
	if err := c.writeRecordLocked(recordApplicationData, b); err != nil {
		c.out.err = err
		return 0, err
	}
	return len(b), nil
}

// Close отправляет close_notify, если запись сейчас свободна, и закрывает соединение.
func (c *Conn) Close() error {
	if c.done.Load() && c.out.TryLock() {
		if c.out.err == nil {
			c.conn.SetWriteDeadline(time.Now().Add(closeTimeout))
			c.sendAlertLocked(alertCloseNotify)
			c.out.err = net.ErrClosed
		}
		c.out.Unlock()
	}
	return c.conn.Close()
}

func (c *Conn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
