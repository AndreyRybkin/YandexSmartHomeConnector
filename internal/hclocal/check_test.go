package hclocal

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"hcgateway/internal/hcdesc"
)

// Описание помечает настройку только для чтения и перечисляет события прибора.
func TestDescribeReadOnly(t *testing.T) {
	d := describe("Oven", map[string]*hcdesc.Feature{
		"a.Setting.Read":  {Kind: "setting", Attrs: map[string]string{"access": "read"}},
		"a.Setting.Write": {Kind: "setting", Attrs: map[string]string{"access": "readWrite"}},
		"a.Event.Done":    {Kind: "event"},
	})
	if !d.Options["a.Setting.Read"].ReadOnly || d.Options["a.Setting.Write"].ReadOnly {
		t.Fatalf("описание: %+v", d.Options)
	}
	if len(d.Events) != 1 || d.Events[0] != "a.Event.Done" {
		t.Fatalf("события: %v", d.Events)
	}
}

func TestCheck(t *testing.T) {
	for _, mode := range []string{ModeTLS, ModeAES} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := Check(ctx, writeFiles(t, mode, testPSK), testHaID, f.addr); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Чужой ключ отличается от отказа связи: совет владельцу разный.
func TestCheckFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := newFake(t, ModeTLS)
	wrong := writeFiles(t, ModeTLS, bytes.Repeat([]byte{0x01}, 32))
	if err := Check(ctx, wrong, testHaID, f.addr); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("чужой ключ: %v", err)
	}
	if err := Check(ctx, writeFiles(t, ModeTLS, testPSK), testHaID, "127.0.0.1:1"); err == nil || errors.Is(err, ErrWrongKey) {
		t.Fatalf("нет связи: %v", err)
	}
	if err := Check(ctx, filepath.Join(t.TempDir(), testHaID), testHaID, f.addr); !errors.Is(err, errNoKeys) {
		t.Fatalf("нет ключей: %v", err)
	}
	if err := Check(ctx, writeFiles(t, ModeTLS, testPSK), "другой", f.addr); err == nil {
		t.Fatal("чужой haId принят")
	}
}
