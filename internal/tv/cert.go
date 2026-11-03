package tv

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
)

// ErrCertChanged - сертификат телевизора не совпал с запомненным: ключ ему не отправляется.
var ErrCertChanged = errors.New("сертификат телевизора изменился")

// Fingerprint - отпечаток SHA-256 сертификата: 64 шестнадцатеричных знака заглавными.
func Fingerprint(cert []byte) string {
	sum := sha256.Sum256(cert)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// CertCheck сверяет сертификат телевизора с запомненным отпечатком. Сертификат у телевизора свой,
// без цепочки, поэтому обычная проверка невозможна: шлюз доверяет первому отпечатку, а новый
// телевизор при этом спрашивает разрешение на своём экране.
type CertCheck struct {
	pin string

	mu   sync.Mutex
	seen string
}

// NewCertCheck создаёт проверку; пустой pin - отпечаток ещё не запомнен, подходит любой.
func NewCertCheck(pin string) *CertCheck {
	return &CertCheck{pin: pin}
}

// TLSConfig - настройки соединения: подмена сертификата обрывает рукопожатие, поэтому ни ключ, ни
// токен телевизора не уходят.
func (c *CertCheck) TLSConfig() *tls.Config {
	return &tls.Config{
		// Цепочки у сертификата нет, вместо неё отпечаток сверяет VerifyConnection.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("телевизор не предъявил сертификат")
			}
			fp := Fingerprint(cs.PeerCertificates[0].Raw)
			c.mu.Lock()
			c.seen = fp
			c.mu.Unlock()
			if c.pin != "" && subtle.ConstantTimeCompare([]byte(fp), []byte(c.pin)) != 1 {
				return ErrCertChanged
			}
			return nil
		},
	}
}

// Seen - отпечаток из последнего рукопожатия; пусто - рукопожатия не было.
func (c *CertCheck) Seen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen
}

// Encrypted сообщает, что адрес WebSocket защищён TLS. На адрес без TLS нельзя отправлять ключ
// телевизора с запомненным отпечатком: иначе защиту обходила бы блокировка порта wss.
func Encrypted(url string) bool {
	return strings.HasPrefix(url, "wss://")
}
