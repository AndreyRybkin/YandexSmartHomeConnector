// Package atomicfile пишет файлы так, что сбой посреди записи не портит прежний файл.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write пишет файл с правами 0600 через временный файл в том же каталоге и переименование.
// Каталога нет - он создаётся с правами 0700.
func Write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
