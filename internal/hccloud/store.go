package hccloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hcgateway/internal/atomicfile"
	"hcgateway/internal/hcdesc"
)

const (
	// tmpPrefix - начало имени временного каталога прибора: такого haId не бывает.
	tmpPrefix = ".tmp-"
	// leftoverAge - с какого возраста временный каталог считается брошенным.
	leftoverAge = time.Hour
)

// ErrNoAppliances - облако не вернуло ни одного прибора.
var ErrNoAppliances = errors.New("облако не вернуло ни одного прибора")

// applianceFile - ключи прибора и сведения о нём, data_dir/appliances/<haId>/appliance.json.
type applianceFile struct {
	HaID   string `json:"haId"`
	Brand  string `json:"brand"`
	HaType string `json:"haType"`
	Encryption
}

// Saved - итог загрузки одного прибора.
type Saved struct {
	Appliance PairedAppliance
	// Mode и Features - режим связи и число функций прибора, если он загружен.
	Mode     string
	Features int
	// Err - почему прибор не загружен. Текст только для журнала и терминала.
	Err error
}

// Download загружает ключи и описания приборов в data_dir/appliances и сообщает итог каждого
// прибора через report. Возвращает, сколько приборов не загружено. Пустой список сохраняет как
// есть: по нему видно, пуст ли аккаунт или облако поменяло формат.
func Download(ctx context.Context, dataDir string, account *Account, paired *Paired, report func(Saved)) (int, error) {
	if len(paired.Appliances) == 0 {
		path := filepath.Join(dataDir, "paired-appliances.json")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return 0, err
		}
		if err := atomicfile.Write(path, paired.Raw); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w, ответ сохранён в %s", ErrNoAppliances, path)
	}
	removeLeftovers(filepath.Join(dataDir, "appliances"))
	failed := 0
	for _, a := range paired.Appliances {
		s := Saved{Appliance: a}
		if !ValidHaID(a.HaID) {
			s.Err = errors.New("недопустимый haId, пропущен")
		} else {
			s.Mode, s.Features, s.Err = saveAppliance(ctx, account, filepath.Join(dataDir, "appliances", a.HaID), a)
		}
		if ctx.Err() != nil {
			return failed, ctx.Err()
		}
		if s.Err != nil {
			failed++
		}
		report(s)
	}
	return failed, nil
}

// saveAppliance загружает ключи и описание прибора и сохраняет их в dir.
// Сначала загружается всё, потом файлы пишутся во временный каталог, и он заменяет dir целиком:
// сбой посреди загрузки или записи не оставит неполный или смешанный каталог.
func saveAppliance(ctx context.Context, account *Account, dir string, a PairedAppliance) (string, int, error) {
	enc, err := account.Encryption(ctx, a.HaID)
	if err != nil {
		return "", 0, fmt.Errorf("ключи: %w", err)
	}
	raw, err := account.Description(ctx, a.HaID)
	if err != nil {
		return "", 0, fmt.Errorf("описание: %w", err)
	}
	desc, err := hcdesc.Parse(raw.FeatureMapping, raw.DeviceDescription)
	if err != nil {
		return "", 0, fmt.Errorf("описание: %w", err)
	}
	descJSON, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return "", 0, err
	}
	keyJSON, err := json.MarshalIndent(applianceFile{HaID: a.HaID, Brand: a.Brand, HaType: a.HaType, Encryption: *enc}, "", "  ")
	if err != nil {
		return "", 0, err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.MkdirTemp(parent, tmpPrefix+"*")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(tmp)
	files := []struct {
		name string
		data []byte
	}{
		{"appliance.json", keyJSON},
		{"description.json", descJSON},
		{"FeatureMapping.xml", raw.FeatureMapping},
		{"DeviceDescription.xml", raw.DeviceDescription},
	}
	for _, f := range files {
		if err := atomicfile.Write(filepath.Join(tmp, f.name), f.data); err != nil {
			return "", 0, err
		}
	}
	if err := replaceDir(tmp, dir); err != nil {
		return "", 0, err
	}
	return enc.Mode, len(desc.Features), nil
}

// replaceDir заменяет каталог dir каталогом tmp целиком: новый ключ не окажется рядом со старым
// описанием. Если новый каталог не встал на место, старый возвращается.
func replaceDir(tmp, dir string) error {
	old := tmp + ".old"
	switch err := os.Rename(dir, old); {
	case err == nil:
		defer os.RemoveAll(old)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if rerr := os.Rename(old, dir); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

// removeLeftovers убирает временные каталоги, которые оставила оборванная загрузка. Свежие не
// трогает: их может писать другая загрузка, например команда fetch-keys.
func removeLeftovers(appliances string) {
	entries, err := os.ReadDir(appliances)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tmpPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > leftoverAge {
			os.RemoveAll(filepath.Join(appliances, e.Name()))
		}
	}
}
