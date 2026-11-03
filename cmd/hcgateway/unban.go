package main

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"time"

	"hcgateway/internal/auth"
	"hcgateway/internal/statedb"
)

// unban снимает баны в базе состояния. Перезапуск баны не снимает, поэтому команда - выход, когда
// бан закрыл вход всем. Работающий шлюз замечает это при следующей попытке входа.
func unban(dataDir string, args []string, stdout io.Writer, now time.Time) error {
	if len(args) != 1 {
		return errors.New("использование: hcgateway [-config путь] unban all|адрес")
	}
	var target netip.Prefix
	all := args[0] == "all"
	if !all {
		var err error
		if target, err = auth.BanTarget(args[0]); err != nil {
			return err
		}
	}
	n, err := statedb.Unban(filepath.Join(dataDir, statedb.FileName), target, all, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Снято банов: %d. Работающий шлюз подхватит это сам.\n", n)
	return nil
}
