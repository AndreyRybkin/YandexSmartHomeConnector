package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"slices"
	"sync"
	"time"

	"hcgateway/internal/auth"
)

const (
	// sessionCookie - имя cookie сессии. Приставка __Host- требует Secure и Path=/ без Domain:
	// соседний поддомен не подменит cookie.
	sessionCookie = "__Host-hcgateway"
	// sessionTTL - сколько живёт сессия.
	sessionTTL = 12 * time.Hour
	// maxSessions - предел сессий; при переполнении удаляется самая старая.
	maxSessions = 20
)

// session - вход владельца. Сессии лежат в памяти: после перезапуска шлюза нужно войти заново.
type session struct {
	// hash - SHA-256 токена из cookie: сам токен шлюз не хранит.
	hash string
	// csrf - токен форм: без него чужой сайт мог бы отправить форму от имени владельца.
	csrf string
	// user и userHash - пользователь, при котором создана сессия: новый пароль закрывает все сессии.
	user, userHash string
	expires        time.Time
	flash          *flash
	// stLogin и stLoginUntil - страница входа SmartThings, которую начал владелец, и её срок.
	stLogin      string
	stLoginUntil time.Time
}

// flash - сообщение для следующей страницы после сохранения.
type flash struct {
	Text  string
	Error bool
}

type sessions struct {
	now func() time.Time

	mu   sync.Mutex
	list []*session
}

func newSessions() *sessions {
	return &sessions{now: time.Now}
}

// create создаёт сессию пользователя u и возвращает токен для cookie.
func (s *sessions) create(u auth.User) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.list = slices.DeleteFunc(s.list, func(x *session) bool { return !now.Before(x.expires) })
	if len(s.list) >= maxSessions {
		s.list = s.list[1:]
	}
	s.list = append(s.list, &session{hash: hashToken(token), csrf: csrf, user: u.Name, userHash: u.Hash,
		expires: now.Add(sessionTTL)})
	return token, nil
}

// find ищет действующую сессию пользователя u по токену. Сравнение за постоянное время, перебор всего списка.
func (s *sessions) find(token string, u auth.User) *session {
	h := []byte(hashToken(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *session
	for _, x := range s.list {
		if subtle.ConstantTimeCompare([]byte(x.hash), h) == 1 {
			found = x
		}
	}
	if found == nil || !s.now().Before(found.expires) || found.user != u.Name || found.userHash != u.Hash {
		return nil
	}
	return found
}

func (s *sessions) remove(x *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = slices.DeleteFunc(s.list, func(y *session) bool { return y == x })
}

func (s *sessions) setFlash(x *session, f *flash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x.flash = f
}

func (s *sessions) takeFlash(x *session) *flash {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := x.flash
	x.flash = nil
	return f
}

// setSmartThings запоминает страницу входа SmartThings до until.
func (s *sessions) setSmartThings(x *session, url string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x.stLogin, x.stLoginUntil = url, until
}

// smartThingsLogin - страница входа SmartThings, если её срок не прошёл.
func (s *sessions) smartThingsLogin(x *session) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x.stLogin == "" || !s.now().Before(x.stLoginUntil) {
		return "", false
	}
	return x.stLogin, true
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
