package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"hcgateway/internal/atomicfile"
)

var (
	// ErrStale - настройки сохранили после того, как страницу открыли: правка сделана по старой версии.
	ErrStale = errors.New("настройки изменились после открытия страницы")
	// ErrChanged - файл изменили вручную после загрузки.
	ErrChanged = errors.New("файл настроек изменён после загрузки")
)

// File - файл настроек: загруженная версия и сохранение, которое не затирает чужую правку.
// Версия - SHA-256 содержимого файла, из которого загружены настройки.
type File struct {
	path string

	mu      sync.Mutex
	cfg     *Config
	version string
}

// OpenFile читает и проверяет файл настроек.
func OpenFile(path string) (*File, error) {
	f := &File{path: path}
	cfg, version, err := f.Read()
	if err != nil {
		return nil, err
	}
	f.cfg, f.version = cfg, version
	return f, nil
}

// Path - путь к файлу.
func (f *File) Path() string {
	return f.path
}

// Current - загруженные настройки и их версия. Настройки не меняют: для правки - Clone.
func (f *File) Current() (*Config, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, f.version
}

// Read читает и проверяет файл, но не делает его текущей версией: сначала его проверяет шлюз.
func (f *File) Read() (*Config, string, error) {
	data, err := readFile(f.path)
	if err != nil {
		return nil, "", err
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, "", err
	}
	return cfg, sum(data), nil
}

// Use делает прочитанные настройки текущими.
func (f *File) Use(cfg *Config, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg, f.version = cfg, version
}

// Save пишет настройки в файл и возвращает их в том виде, в каком их прочитает следующий запуск.
// version - версия, по которой сделана правка. Прежнее содержимое файла остаётся в .bak.
func (f *File) Save(next *Config, version string) (*Config, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if version != f.version {
		return nil, "", ErrStale
	}
	old, err := readFile(f.path)
	if err != nil {
		return nil, "", err
	}
	if sum(old) != f.version {
		return nil, "", ErrChanged
	}
	data, err := next.marshal()
	if err != nil {
		return nil, "", err
	}
	// Файл, который не прочитается при запуске, не сохраняется.
	cfg, err := parse(data)
	if err != nil {
		return nil, "", fmt.Errorf("проверка сохраняемых настроек: %w", err)
	}
	if err := atomicfile.Write(f.path+".bak", old); err != nil {
		return nil, "", err
	}
	if err := atomicfile.Write(f.path, data); err != nil {
		return nil, "", err
	}
	f.cfg, f.version = cfg, sum(data)
	return cfg, f.version, nil
}

func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}
