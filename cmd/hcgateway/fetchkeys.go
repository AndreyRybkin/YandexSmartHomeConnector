package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"hcgateway/internal/atomicfile"
	"hcgateway/internal/hccloud"
	"hcgateway/internal/hcdesc"
)

// applianceFile - ключи прибора и сведения о нём, data_dir/appliances/<haId>/appliance.json.
type applianceFile struct {
	HaID   string `json:"haId"`
	Brand  string `json:"brand"`
	HaType string `json:"haType"`
	hccloud.Encryption
}

func fetchKeys(ctx context.Context, dataDir string, stdin io.Reader, stdout, stderr io.Writer) error {
	login, err := hccloud.NewLogin()
	if err != nil {
		return err
	}
	link, err := login.URL()
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, "1. Откройте в браузере адрес ниже и войдите в аккаунт Home Connect.")
	fmt.Fprintln(stderr, "   Перед входом откройте инструменты разработчика (F12), вкладку Network.")
	fmt.Fprintln(stderr, "2. После входа браузер не сможет открыть адрес hcauth://auth/prod?... -")
	fmt.Fprintln(stderr, "   найдите его на вкладке Network или в консоли и скопируйте целиком.")
	fmt.Fprintln(stderr, "3. Вставьте адрес сюда и нажмите Enter.")
	fmt.Fprintln(stderr)
	fmt.Fprintln(stderr, link)
	fmt.Fprintln(stderr)
	fmt.Fprint(stderr, "Адрес hcauth://: ")

	redirect, err := readLine(stdin)
	if err != nil {
		return err
	}
	token, subject, err := login.Exchange(ctx, redirect)
	if err != nil {
		return err
	}
	account, paired, err := hccloud.Discover(ctx, token, subject)
	if err != nil {
		return err
	}
	appliances := paired.Appliances
	fmt.Fprintf(stdout, "Регион аккаунта: %s. Приборов в аккаунте: %d.\n", paired.Host, len(appliances))
	if len(appliances) == 0 {
		// Ответ сохраняется как есть: по нему видно, пуст ли список или облако поменяло формат.
		path := filepath.Join(dataDir, "paired-appliances.json")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return err
		}
		if err := atomicfile.Write(path, paired.Raw); err != nil {
			return err
		}
		return fmt.Errorf("облако не вернуло ни одного прибора, ответ сохранён в %s", path)
	}

	var failed int
	for _, a := range appliances {
		if !hccloud.ValidHaID(a.HaID) {
			fmt.Fprintf(stdout, "! %s %s: недопустимый haId, пропущен\n", a.Brand, a.HaType)
			failed++
			continue
		}
		features, err := saveAppliance(ctx, account, filepath.Join(dataDir, "appliances", a.HaID), a)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(stdout, "! %s %s %s: %v\n", a.Brand, a.HaType, a.HaID, err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "  %s %s %s: связь %s, функций %d\n", a.Brand, a.HaType, a.HaID, features.mode, features.count)
	}
	fmt.Fprintf(stdout, "Ключи и описания: %s.\n", filepath.Join(dataDir, "appliances"))
	if failed > 0 {
		return fmt.Errorf("не загружено приборов: %d", failed)
	}
	return nil
}

type savedInfo struct {
	mode  string
	count int
}

// saveAppliance загружает ключи и описание прибора и сохраняет их в dir.
// Сначала загружается всё, потом пишутся файлы: сбой посреди загрузки не оставит неполный каталог.
func saveAppliance(ctx context.Context, account *hccloud.Account, dir string, a hccloud.PairedAppliance) (savedInfo, error) {
	enc, err := account.Encryption(ctx, a.HaID)
	if err != nil {
		return savedInfo{}, fmt.Errorf("ключи: %w", err)
	}
	raw, err := account.Description(ctx, a.HaID)
	if err != nil {
		return savedInfo{}, fmt.Errorf("описание: %w", err)
	}
	desc, err := hcdesc.Parse(raw.FeatureMapping, raw.DeviceDescription)
	if err != nil {
		return savedInfo{}, fmt.Errorf("описание: %w", err)
	}
	descJSON, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return savedInfo{}, err
	}
	keyJSON, err := json.MarshalIndent(applianceFile{HaID: a.HaID, Brand: a.Brand, HaType: a.HaType, Encryption: *enc}, "", "  ")
	if err != nil {
		return savedInfo{}, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return savedInfo{}, err
	}
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
		if err := atomicfile.Write(filepath.Join(dir, f.name), f.data); err != nil {
			return savedInfo{}, err
		}
	}
	return savedInfo{mode: enc.Mode, count: len(desc.Features)}, nil
}

// readLine читает одну строку не длиннее 8 КБ.
func readLine(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 8<<10)
	if sc.Scan() {
		return sc.Text(), nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("адрес не введён")
}
