// Package auth - вход владельца: пользователь веб-интерфейса, проверка пароля и бан адресов
// после неудачных попыток. Вход общий для веб-интерфейса и страницы привязки Алисы.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"hcgateway/internal/atomicfile"
)

const (
	// UserFile - файл пользователя в data_dir.
	UserFile = "web-user.json"
	// DefaultName - имя пользователя по умолчанию.
	DefaultName = "admin"
	// MinPasswordLen и MaxPasswordLen - пределы длины своего пароля в символах.
	MinPasswordLen = 12
	MaxPasswordLen = 128
	// generatedLen - длина пароля, который создаёт команда web-user.
	generatedLen = 16
	// passwordAlphabet - символы созданного пароля: латинские буквы и цифры.
	passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// maxUserFile - предел размера файла пользователя.
	maxUserFile = 4 << 10
	saltLen     = 16
	keyLen      = 32
)

// nameRe - допустимое имя пользователя.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

type argonParams struct {
	// memory - память в КиБ.
	memory  uint32
	time    uint32
	threads uint8
}

// params - параметры argon2id по рекомендации OWASP: 19 МиБ памяти, 2 прохода, 1 поток.
// Свой пароль может быть словарным, а argon2id делает дорогой каждую попытку подбора по хэшу.
var params = argonParams{memory: 19 * 1024, time: 2, threads: 1}

// User - пользователь веб-интерфейса.
type User struct {
	Name string `json:"name"`
	// Hash - хэш пароля $argon2id$v=19$m=19456,t=2,p=1$<соль>$<хэш>, соль и хэш в base64.
	Hash string `json:"hash"`
}

// ValidName сообщает, подходит ли имя пользователя: от 1 до 32 латинских букв, цифр, _ и -.
func ValidName(name string) bool {
	return nameRe.MatchString(name)
}

// CheckPassword проверяет длину своего пароля.
func CheckPassword(password string) error {
	if n := utf8.RuneCountInString(password); n < MinPasswordLen || n > MaxPasswordLen {
		return fmt.Errorf("пароль: нужно от %d до %d символов", MinPasswordLen, MaxPasswordLen)
	}
	return nil
}

// GeneratePassword - пароль из 16 случайных латинских букв и цифр.
func GeneratePassword() (string, error) {
	b := make([]byte, generatedLen)
	n := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		k, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b[i] = passwordAlphabet[k.Int64()]
	}
	return string(b), nil
}

// NewUser создаёт пользователя с хэшем пароля.
func NewUser(name, password string) (User, error) {
	if !ValidName(name) {
		return User{}, fmt.Errorf("имя %q: нужно от 1 до 32 латинских букв, цифр, _ и -", name)
	}
	if err := CheckPassword(password); err != nil {
		return User{}, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return User{}, err
	}
	key := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, keyLen)
	enc := base64.RawStdEncoding
	return User{Name: name, Hash: fmt.Sprintf("$argon2id$v=%d$%s$%s$%s", argon2.Version, params,
		enc.EncodeToString(salt), enc.EncodeToString(key))}, nil
}

func (p argonParams) String() string {
	return fmt.Sprintf("m=%d,t=%d,p=%d", p.memory, p.time, p.threads)
}

// SaveUser пишет файл пользователя.
func SaveUser(path string, u User) error {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(data, '\n'))
}

// hash - разобранный хэш пароля.
type hash struct {
	salt, key []byte
}

// parseHash разбирает хэш. Параметры должны совпадать с params: от них зависит,
// сколько памяти занимает проверка пароля.
func parseHash(s string) (*hash, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" ||
		parts[2] != fmt.Sprintf("v=%d", argon2.Version) || parts[3] != params.String() {
		return nil, fmt.Errorf("хэш пароля - не argon2id с параметрами %s, создайте пользователя заново", params)
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil || len(salt) != saltLen {
		return nil, errors.New("неверная соль в хэше пароля")
	}
	key, err := enc.DecodeString(parts[5])
	if err != nil || len(key) != keyLen {
		return nil, errors.New("неверный хэш пароля")
	}
	return &hash{salt: salt, key: key}, nil
}

func (h *hash) matches(password string) bool {
	key := argon2.IDKey([]byte(password), h.salt, params.time, params.memory, params.threads, keyLen)
	return subtle.ConstantTimeCompare(key, h.key) == 1
}

// Users - пользователь из файла. Файл перечитывается, когда его заменили:
// повторный запуск web-user действует без перезапуска шлюза.
type Users struct {
	path string
	log  *slog.Logger

	mu sync.Mutex
	// info - файл, из которого прочитан пользователь; nil - файла нет.
	info os.FileInfo
	user *User
	hash *hash
	// lastErr - последняя ошибка в журнале: одна и та же ошибка пишется один раз.
	lastErr string
}

// NewUsers следит за файлом пользователя path.
func NewUsers(path string, log *slog.Logger) *Users {
	return &Users{path: path, log: log}
}

// Current - пользователь или false, если файла нет или он неверный.
func (u *Users) Current() (User, bool) {
	user, _ := u.snapshot()
	if user == nil {
		return User{}, false
	}
	return *user, true
}

func (u *Users) snapshot() (*User, *hash) {
	u.mu.Lock()
	defer u.mu.Unlock()
	info, err := os.Stat(u.path)
	if err == nil && u.info != nil && sameFile(u.info, info) {
		return u.user, u.hash
	}
	u.info, u.user, u.hash = nil, nil, nil
	if err == nil {
		var (
			user *User
			h    *hash
		)
		user, h, err = readUser(u.path)
		// Неверный файл не перечитывается на каждый запрос, только после замены.
		u.info = info
		if err == nil {
			u.user, u.hash, u.lastErr = user, h, ""
			u.log.Info("пользователь веб-интерфейса загружен", "user", user.Name)
			return u.user, u.hash
		}
	}
	if msg := err.Error(); msg != u.lastErr {
		u.lastErr = msg
		if errors.Is(err, fs.ErrNotExist) {
			u.log.Warn("пользователь веб-интерфейса не создан, нужна команда web-user")
		} else {
			u.log.Error("файл пользователя веб-интерфейса", "err", err)
		}
	}
	return nil, nil
}

// sameFile - тот же файл без изменений. Новый файл web-user пишет через переименование,
// поэтому у него другой inode.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

func readUser(path string) (*User, *hash, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUserFile+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxUserFile {
		return nil, nil, fmt.Errorf("%s больше %d байт", path, maxUserFile)
	}
	var u User
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if !ValidName(u.Name) {
		return nil, nil, fmt.Errorf("%s: недопустимое имя пользователя", path)
	}
	h, err := parseHash(u.Hash)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return &u, h, nil
}

// check сравнивает логин и пароль. Пароль проверяется и при неверном имени:
// иначе по времени ответа было бы видно, что неверно именно имя.
func (u *Users) check(name, password string) (User, bool) {
	user, h := u.snapshot()
	if user == nil || len(name) > maxNameBytes || len(password) > maxPasswordBytes {
		return User{}, false
	}
	nameOK := equal(name, user.Name)
	passwordOK := h.matches(password)
	return *user, nameOK && passwordOK
}

// equal сравнивает строки за постоянное время.
func equal(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
