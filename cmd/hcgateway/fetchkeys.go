package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"hcgateway/internal/hccloud"
)

func fetchKeys(ctx context.Context, dataDir string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("fetch-keys", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("region", hccloud.Global.Name, "регион аккаунта: eu - Европа, na - Северная Америка, cn - Китай, global - Европа или Северная Америка через общий адрес входа")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("лишние аргументы")
	}
	region, ok := hccloud.RegionByName(*name)
	if !ok {
		return fmt.Errorf("неизвестный регион %q: нужен eu, na, cn или global", *name)
	}
	// Токен нужен только на эту загрузку, поэтому и права - только на ключи.
	login, err := hccloud.NewLogin(region, hccloud.ScopeKeys)
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
	grant, err := login.Exchange(ctx, redirect)
	if err != nil {
		return err
	}
	account, paired, err := hccloud.Discover(ctx, region, grant.Access, grant.Subject)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Регион аккаунта: %s. Приборов в аккаунте: %d.\n", paired.Host, len(paired.Appliances))
	failed, err := hccloud.Download(ctx, dataDir, account, paired, func(r hccloud.Saved) {
		a := r.Appliance
		if r.Err != nil {
			fmt.Fprintf(stdout, "! %s %s %s: %v\n", a.Brand, a.HaType, a.HaID, r.Err)
			return
		}
		fmt.Fprintf(stdout, "  %s %s %s: связь %s, функций %d\n", a.Brand, a.HaType, a.HaID, r.Mode, r.Features)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Ключи и описания: %s.\n", filepath.Join(dataDir, "appliances"))
	if failed > 0 {
		return fmt.Errorf("не загружено приборов: %d", failed)
	}
	return nil
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
