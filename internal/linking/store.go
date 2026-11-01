// Package linking - привязка аккаунта Яндекса к шлюзу по OAuth 2.0 с кодом авторизации.
// Шлюз сам выдаёт токены Яндексу и хранит на диске только их хэши.
package linking

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"hcgateway/internal/atomicfile"
)

const (
	// AccessTTL - срок жизни access token.
	AccessTTL = 24 * time.Hour
	// maxRefresh - сколько refresh token хранить; при переполнении удаляется самый старый.
	maxRefresh = 10
	// maxStoreSize - предел размера файла токенов.
	maxStoreSize = 1 << 20
)

type tokenRecord struct {
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires,omitempty"`
}

type storeFile struct {
	Access  []tokenRecord `json:"access"`
	Refresh []tokenRecord `json:"refresh"`
}

// Store хранит хэши выданных токенов в файле.
type Store struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	data storeFile
}

// Tokens - пара токенов для ответа Яндексу.
type Tokens struct {
	Access  string
	Refresh string
}

// OpenStore читает файл токенов; если файла нет, начинает с пустого списка.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxStoreSize {
		return nil, fmt.Errorf("%s больше %d байт", path, maxStoreSize)
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Issue выдаёт новую пару токенов.
func (s *Store) Issue() (Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueLocked()
}

func (s *Store) issueLocked() (Tokens, error) {
	access, err := randomToken()
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := randomToken()
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	next := s.data
	next.Access = append(s.liveAccess(now), tokenRecord{Hash: hash(access), Created: now, Expires: now.Add(AccessTTL)})
	next.Refresh = append(append([]tokenRecord(nil), s.data.Refresh...), tokenRecord{Hash: hash(refresh), Created: now})
	if len(next.Refresh) > maxRefresh {
		next.Refresh = next.Refresh[len(next.Refresh)-maxRefresh:]
	}
	if err := s.saveLocked(next); err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, Refresh: refresh}, nil
}

// Refresh меняет refresh token на новую пару. Старый refresh token после этого недействителен.
func (s *Store) Refresh(refresh string) (Tokens, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := find(s.data.Refresh, refresh)
	if i < 0 {
		return Tokens{}, false, nil
	}
	prev := s.data
	s.data.Refresh = append(append([]tokenRecord(nil), prev.Refresh[:i]...), prev.Refresh[i+1:]...)
	t, err := s.issueLocked()
	if err != nil {
		s.data = prev
		return Tokens{}, false, err
	}
	return t, true, nil
}

// Valid сообщает, действует ли access token.
func (s *Store) Valid(access string) bool {
	if access == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := find(s.data.Access, access)
	return i >= 0 && s.now().Before(s.data.Access[i].Expires)
}

// RevokeAll отзывает все токены: Яндекс сообщил, что аккаунт отвязан.
func (s *Store) RevokeAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(storeFile{})
}

// Linked сообщает, выдан ли хотя бы один refresh token.
func (s *Store) Linked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data.Refresh) > 0
}

func (s *Store) liveAccess(now time.Time) []tokenRecord {
	var out []tokenRecord
	for _, r := range s.data.Access {
		if now.Before(r.Expires) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) saveLocked(next storeFile) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.path, data); err != nil {
		return err
	}
	s.data = next
	return nil
}

// find ищет токен по хэшу. Сравнение за постоянное время, перебор всего списка.
func find(list []tokenRecord, token string) int {
	h := []byte(hash(token))
	found := -1
	for i, r := range list {
		if subtle.ConstantTimeCompare([]byte(r.Hash), h) == 1 {
			found = i
		}
	}
	return found
}

func hash(token string) string {
	sum := sha256Sum(token)
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

func sha256Sum(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
