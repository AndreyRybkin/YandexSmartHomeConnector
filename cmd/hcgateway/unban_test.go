package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/statedb"
)

// Команда снимает бан адреса и все баны, берёт каталог данных из настроек и отвергает мусор.
func TestUnbanCommand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := statedb.Open(filepath.Join(dir, statedb.FileName), slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer db.Close()
	now := time.Now()
	for _, a := range []string{"203.0.113.7/32", "203.0.113.8/32", "2001:db8:1:2::/64"} {
		db.BanStarted(netip.MustParsePrefix(a), now, now.Add(time.Hour), "привязка")
	}
	db.Flush()
	for _, tc := range []struct{ arg, want string }{{"203.0.113.7", "Снято банов: 1."}, {"2001:db8:1:2::10", "Снято банов: 1."},
		{"203.0.113.7", "Снято банов: 0."}, {"all", "Снято банов: 1."}} {
		var out bytes.Buffer
		if err := run([]string{"-config", cfgPath, "unban", tc.arg}, nil, &out, io.Discard); err != nil {
			t.Fatalf("%s: %v", tc.arg, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%s: %q", tc.arg, out.String())
		}
	}
	for _, args := range [][]string{{"unban"}, {"unban", "не адрес"}, {"unban", "all", "лишнее"}} {
		if err := run(append([]string{"-config", cfgPath}, args...), nil, io.Discard, io.Discard); err == nil {
			t.Fatalf("%v: ошибки нет", args)
		}
	}
}
