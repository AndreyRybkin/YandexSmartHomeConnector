package tlspsk

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"slices"

	"golang.org/x/crypto/cryptobyte"
)

// Сообщения рукопожатия.
const (
	typeHelloRequest      = 0
	typeClientHello       = 1
	typeServerHello       = 2
	typeCertificate       = 11
	typeServerKeyExchange = 12
	typeServerHelloDone   = 14
	typeClientKeyExchange = 16
	typeFinished          = 20
)

// Расширения.
const (
	extSupportedGroups      = 10
	extPointFormats         = 11
	extExtendedMasterSecret = 23
	extRenegotiationInfo    = 0xff01
)

const (
	// curveNamed - группа ECDHE задана номером.
	curveNamed = 3
	// pointUncompressed - точка кривой без сжатия.
	pointUncompressed = 0
	// finishedSize - длина проверочных данных Finished.
	finishedSize = 12
	// keySize и ivSize - ключ и IV ChaCha20-Poly1305 в key_block.
	keySize = 32
	ivSize  = 12
)

// groups - группы ECDHE клиента по порядку предпочтения: x25519, secp256r1, secp384r1.
var groups = []struct {
	id    uint16
	curve ecdh.Curve
}{{0x001d, ecdh.X25519()}, {0x0017, ecdh.P256()}, {0x0018, ecdh.P384()}}

func curveFor(id uint16) ecdh.Curve {
	for _, g := range groups {
		if g.id == id {
			return g.curve
		}
	}
	return nil
}

// ServerConfig - сторона сервера. Её использует поддельный прибор в тестах.
type ServerConfig struct {
	Identity string
	PSK      []byte
	// Hint - подсказка идентификатора в ServerKeyExchange.
	Hint string
	// Group - группа ECDHE; 0 - первая из предложенных клиентом.
	Group uint16
	// NoEMS - не включать extended master secret, даже если клиент его предлагает.
	NoEMS bool
}

func decodeError(what string) error {
	return &localError{alertDecodeError, what + " не разобран"}
}

func unexpected(msg []byte, want string) error {
	if msg[0] == typeCertificate {
		return &localError{alertUnexpectedMessage, "другая сторона прислала сертификат: она ждёт не PSK"}
	}
	return &localError{alertUnexpectedMessage, fmt.Sprintf("сообщение рукопожатия %d вместо %s", msg[0], want)}
}

func (c *Conn) writeHandshakeLocked(msg []byte) error {
	c.transcript = append(c.transcript, msg...)
	return c.writeRecordLocked(recordHandshake, msg)
}

func (c *Conn) clientHandshake() error {
	var clientRandom [32]byte
	rand.Read(clientRandom[:])
	var b cryptobyte.Builder
	b.AddUint8(typeClientHello)
	b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint16(version12)
		b.AddBytes(clientRandom[:])
		b.AddUint8(0) // без идентификатора сессии: возобновления нет
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddUint16(suiteECDHEPSKChaCha)
			b.AddUint16(suiteRenegotiationSCSV)
		})
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint8(0) }) // без сжатия
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddUint16(extSupportedGroups)
			b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
				b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
					for _, g := range groups {
						b.AddUint16(g.id)
					}
				})
			})
			b.AddUint16(extPointFormats)
			b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
				b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint8(pointUncompressed) })
			})
			b.AddUint16(extExtendedMasterSecret)
			b.AddUint16(0)
		})
	})
	if err := c.writeHandshakeLocked(b.BytesOrPanic()); err != nil {
		return err
	}

	msg, err := c.readHandshakeLocked()
	if err != nil {
		return err
	}
	if msg[0] != typeServerHello {
		return unexpected(msg, "ServerHello")
	}
	s := cryptobyte.String(msg[4:])
	var vers, suite uint16
	var serverRandom []byte
	var session cryptobyte.String
	var compression uint8
	if !s.ReadUint16(&vers) || !s.ReadBytes(&serverRandom, 32) || !s.ReadUint8LengthPrefixed(&session) ||
		!s.ReadUint16(&suite) || !s.ReadUint8(&compression) {
		return decodeError("ServerHello")
	}
	switch {
	case vers != version12:
		return &localError{alertProtocolVersion, fmt.Sprintf("сервер выбрал версию %#04x, нужна TLS 1.2", vers)}
	case suite != suiteECDHEPSKChaCha:
		return &localError{alertIllegalParameter, fmt.Sprintf("сервер выбрал набор %#04x, которого клиент не предлагал", suite)}
	case compression != 0:
		return &localError{alertIllegalParameter, "сервер выбрал сжатие"}
	}
	ems, err := serverExtensions(s)
	if err != nil {
		return err
	}
	c.transcript = append(c.transcript, msg...)
	c.out.version = version12

	// ServerKeyExchange: подсказка идентификатора и открытый ключ ECDHE сервера.
	if msg, err = c.readHandshakeLocked(); err != nil {
		return err
	}
	if msg[0] != typeServerKeyExchange {
		return unexpected(msg, "ServerKeyExchange")
	}
	s = cryptobyte.String(msg[4:])
	var hint, point cryptobyte.String
	var curveType uint8
	var group uint16
	if !s.ReadUint16LengthPrefixed(&hint) || !s.ReadUint8(&curveType) || !s.ReadUint16(&group) ||
		!s.ReadUint8LengthPrefixed(&point) || !s.Empty() {
		return decodeError("ServerKeyExchange")
	}
	curve := curveFor(group)
	if curveType != curveNamed || curve == nil {
		return &localError{alertIllegalParameter, fmt.Sprintf("сервер выбрал группу ECDHE %#04x, которой клиент не предлагал", group)}
	}
	peer, err := curve.NewPublicKey(point)
	if err != nil {
		return &localError{alertIllegalParameter, "неверный ключ ECDHE сервера"}
	}
	c.transcript = append(c.transcript, msg...)

	if msg, err = c.readHandshakeLocked(); err != nil {
		return err
	}
	if msg[0] != typeServerHelloDone {
		return unexpected(msg, "ServerHelloDone")
	}
	if len(msg) != 4 {
		return decodeError("ServerHelloDone")
	}
	c.transcript = append(c.transcript, msg...)

	// ClientKeyExchange: идентификатор и открытый ключ ECDHE клиента.
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	z, err := priv.ECDH(peer)
	if err != nil {
		return &localError{alertIllegalParameter, "неверный ключ ECDHE сервера"}
	}
	b = cryptobyte.Builder{}
	b.AddUint8(typeClientKeyExchange)
	b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte(c.identity)) })
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes(priv.PublicKey().Bytes()) })
	})
	cke := b.BytesOrPanic()
	c.transcript = append(c.transcript, cke...)
	master := masterSecret(premaster(z, c.psk), ems, clientRandom[:], serverRandom, c.transcript)
	clientKey, serverKey, clientIV, serverIV := keyBlock(master, clientRandom[:], serverRandom)

	buf, err := c.appendRecordLocked(nil, recordHandshake, cke)
	if err != nil {
		return err
	}
	if buf, err = c.appendRecordLocked(buf, recordChangeCipherSpec, []byte{1}); err != nil {
		return err
	}
	if c.out.cipher, err = newHalf(clientKey, clientIV); err != nil {
		return err
	}
	fin := finishedMessage(master, "client finished", c.transcript)
	c.transcript = append(c.transcript, fin...)
	if buf, err = c.appendRecordLocked(buf, recordHandshake, fin); err != nil {
		return err
	}
	if _, err := c.conn.Write(buf); err != nil {
		return err
	}

	if err := c.readChangeCipherSpecLocked(); err != nil {
		return err
	}
	if c.in.cipher, err = newHalf(serverKey, serverIV); err != nil {
		return err
	}
	if msg, err = c.readHandshakeLocked(); err != nil {
		return err
	}
	if msg[0] != typeFinished {
		return unexpected(msg, "Finished")
	}
	if !hmac.Equal(msg, finishedMessage(master, "server finished", c.transcript)) {
		return &localError{alertDecryptError, "Finished сервера не сходится"}
	}
	return nil
}

// serverExtensions разбирает расширения ServerHello: сообщает, включён ли extended master secret.
// Расширения, которых клиент не предлагал, клиент пропускает.
func serverExtensions(s cryptobyte.String) (bool, error) {
	if s.Empty() {
		return false, nil
	}
	var exts cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&exts) || !s.Empty() {
		return false, decodeError("ServerHello")
	}
	ems := false
	for !exts.Empty() {
		var typ uint16
		var body cryptobyte.String
		if !exts.ReadUint16(&typ) || !exts.ReadUint16LengthPrefixed(&body) {
			return false, decodeError("ServerHello")
		}
		switch typ {
		case extExtendedMasterSecret:
			if !body.Empty() {
				return false, decodeError("extended_master_secret")
			}
			ems = true
		case extRenegotiationInfo:
			var info cryptobyte.String
			if !body.ReadUint8LengthPrefixed(&info) || !info.Empty() || !body.Empty() {
				return false, &localError{alertHandshakeFailure, "неверное расширение renegotiation_info"}
			}
		}
	}
	return ems, nil
}

// clientHello - то, что сервер берёт из ClientHello.
type clientHello struct {
	random []byte
	suite  bool
	groups []uint16
	ems    bool
}

func parseClientHello(msg []byte) (*clientHello, error) {
	s := cryptobyte.String(msg[4:])
	h := &clientHello{}
	var vers uint16
	var session, suites, compression cryptobyte.String
	if !s.ReadUint16(&vers) || !s.ReadBytes(&h.random, 32) || !s.ReadUint8LengthPrefixed(&session) ||
		!s.ReadUint16LengthPrefixed(&suites) || !s.ReadUint8LengthPrefixed(&compression) {
		return nil, decodeError("ClientHello")
	}
	if vers < version12 {
		return nil, &localError{alertProtocolVersion, fmt.Sprintf("клиент предлагает версию %#04x, нужна TLS 1.2", vers)}
	}
	for !suites.Empty() {
		var cs uint16
		if !suites.ReadUint16(&cs) {
			return nil, decodeError("ClientHello")
		}
		h.suite = h.suite || cs == suiteECDHEPSKChaCha
	}
	if !slices.Contains(compression, 0) {
		return nil, &localError{alertIllegalParameter, "клиент не предлагает работу без сжатия"}
	}
	if s.Empty() {
		return h, nil
	}
	var exts cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&exts) || !s.Empty() {
		return nil, decodeError("ClientHello")
	}
	for !exts.Empty() {
		var typ uint16
		var body cryptobyte.String
		if !exts.ReadUint16(&typ) || !exts.ReadUint16LengthPrefixed(&body) {
			return nil, decodeError("ClientHello")
		}
		switch typ {
		case extSupportedGroups:
			var list cryptobyte.String
			if !body.ReadUint16LengthPrefixed(&list) {
				return nil, decodeError("supported_groups")
			}
			for !list.Empty() {
				var g uint16
				if !list.ReadUint16(&g) {
					return nil, decodeError("supported_groups")
				}
				h.groups = append(h.groups, g)
			}
		case extExtendedMasterSecret:
			h.ems = true
		}
	}
	return h, nil
}

func (c *Conn) serverHandshake() error {
	msg, err := c.readHandshakeLocked()
	if err != nil {
		return err
	}
	if msg[0] != typeClientHello {
		return unexpected(msg, "ClientHello")
	}
	hello, err := parseClientHello(msg)
	if err != nil {
		return err
	}
	if !hello.suite {
		return &localError{alertHandshakeFailure, "клиент не предлагает TLS_ECDHE_PSK_WITH_CHACHA20_POLY1305_SHA256"}
	}
	c.transcript = append(c.transcript, msg...)
	ems := hello.ems && !c.server.NoEMS
	group := c.server.Group
	if group == 0 {
		for _, g := range hello.groups {
			if curveFor(g) != nil {
				group = g
				break
			}
		}
	} else if !slices.Contains(hello.groups, group) {
		group = 0
	}
	curve := curveFor(group)
	if curve == nil {
		return &localError{alertHandshakeFailure, "нет общей группы ECDHE"}
	}
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	var serverRandom [32]byte
	rand.Read(serverRandom[:])

	var b cryptobyte.Builder
	b.AddUint8(typeServerHello)
	b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint16(version12)
		b.AddBytes(serverRandom[:])
		b.AddUint8(0)
		b.AddUint16(suiteECDHEPSKChaCha)
		b.AddUint8(0)
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddUint16(extRenegotiationInfo)
			b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint8(0) })
			if ems {
				b.AddUint16(extExtendedMasterSecret)
				b.AddUint16(0)
			}
		})
	})
	b.AddUint8(typeServerKeyExchange)
	b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte(c.server.Hint)) })
		b.AddUint8(curveNamed)
		b.AddUint16(group)
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes(priv.PublicKey().Bytes()) })
	})
	b.AddUint8(typeServerHelloDone)
	b.AddUint24(0)
	if err := c.writeHandshakeLocked(b.BytesOrPanic()); err != nil {
		return err
	}

	if msg, err = c.readHandshakeLocked(); err != nil {
		return err
	}
	if msg[0] != typeClientKeyExchange {
		return unexpected(msg, "ClientKeyExchange")
	}
	s := cryptobyte.String(msg[4:])
	var identity, point cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&identity) || !s.ReadUint8LengthPrefixed(&point) || !s.Empty() {
		return decodeError("ClientKeyExchange")
	}
	if subtle.ConstantTimeCompare(identity, []byte(c.identity)) != 1 {
		return &localError{alertUnknownPSKIdentity, "чужой идентификатор PSK"}
	}
	peer, err := curve.NewPublicKey(point)
	if err != nil {
		return &localError{alertIllegalParameter, "неверный ключ ECDHE клиента"}
	}
	z, err := priv.ECDH(peer)
	if err != nil {
		return &localError{alertIllegalParameter, "неверный ключ ECDHE клиента"}
	}
	c.transcript = append(c.transcript, msg...)
	master := masterSecret(premaster(z, c.psk), ems, hello.random, serverRandom[:], c.transcript)
	clientKey, serverKey, clientIV, serverIV := keyBlock(master, hello.random, serverRandom[:])

	if err := c.readChangeCipherSpecLocked(); err != nil {
		return err
	}
	if c.in.cipher, err = newHalf(clientKey, clientIV); err != nil {
		return err
	}
	if msg, err = c.readHandshakeLocked(); err != nil {
		return err
	}
	if msg[0] != typeFinished {
		return unexpected(msg, "Finished")
	}
	if !hmac.Equal(msg, finishedMessage(master, "client finished", c.transcript)) {
		return &localError{alertDecryptError, "Finished клиента не сходится"}
	}
	c.transcript = append(c.transcript, msg...)
	buf, err := c.appendRecordLocked(nil, recordChangeCipherSpec, []byte{1})
	if err != nil {
		return err
	}
	if c.out.cipher, err = newHalf(serverKey, serverIV); err != nil {
		return err
	}
	if buf, err = c.appendRecordLocked(buf, recordHandshake, finishedMessage(master, "server finished", c.transcript)); err != nil {
		return err
	}
	_, err = c.conn.Write(buf)
	return err
}

// premaster - общий секрет ECDHE_PSK (RFC 5489, раздел 2): длина и секрет ECDH, затем длина и PSK.
func premaster(z, psk []byte) []byte {
	out := make([]byte, 0, 4+len(z)+len(psk))
	out = binary.BigEndian.AppendUint16(out, uint16(len(z)))
	out = append(out, z...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(psk)))
	return append(out, psk...)
}

// masterSecret - главный секрет: с extended master secret (RFC 7627) - от хеша рукопожатия
// до ClientKeyExchange включительно, без него - от случайных чисел сторон.
func masterSecret(pms []byte, ems bool, clientRandom, serverRandom, transcript []byte) []byte {
	if ems {
		h := sha256.Sum256(transcript)
		return prf(pms, "extended master secret", h[:], 48)
	}
	return prf(pms, "master secret", append(slices.Clone(clientRandom), serverRandom...), 48)
}

// keyBlock - ключи и IV сторон. MAC-ключей у AEAD нет.
func keyBlock(master, clientRandom, serverRandom []byte) (clientKey, serverKey, clientIV, serverIV []byte) {
	kb := prf(master, "key expansion", append(slices.Clone(serverRandom), clientRandom...), 2*keySize+2*ivSize)
	return kb[:keySize], kb[keySize : 2*keySize], kb[2*keySize : 2*keySize+ivSize], kb[2*keySize+ivSize:]
}

func finishedMessage(master []byte, label string, transcript []byte) []byte {
	h := sha256.Sum256(transcript)
	return append([]byte{typeFinished, 0, 0, finishedSize}, prf(master, label, h[:], finishedSize)...)
}

// prf - PRF TLS 1.2 на HMAC-SHA256 (RFC 5246, раздел 5).
func prf(secret []byte, label string, seed []byte, n int) []byte {
	labelSeed := append([]byte(label), seed...)
	out := make([]byte, 0, n+sha256.Size)
	a := labelSeed
	for len(out) < n {
		m := hmac.New(sha256.New, secret)
		m.Write(a)
		a = m.Sum(nil)
		m.Reset()
		m.Write(a)
		m.Write(labelSeed)
		out = m.Sum(out)
	}
	return out[:n]
}
