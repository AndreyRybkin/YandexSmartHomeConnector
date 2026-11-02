package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestFetchKeysArgs(t *testing.T) {
	for _, args := range [][]string{{"-region", "mars"}, {"лишнее"}} {
		if err := fetchKeys(context.Background(), t.TempDir(), args, strings.NewReader(""), io.Discard, io.Discard); err == nil || strings.Contains(err.Error(), "адрес не введён") {
			t.Errorf("%v: %v", args, err)
		}
	}
	var out strings.Builder
	err := fetchKeys(context.Background(), t.TempDir(), []string{"-region", "cn"}, strings.NewReader(""), io.Discard, &out)
	if err == nil || !strings.Contains(out.String(), "https://api.home-connect.cn/security/oauth/authorize?") {
		t.Fatalf("адрес входа: %v %s", err, out.String())
	}
	// Токен нужен один раз: только права на ключи.
	if !strings.Contains(out.String(), "scope=ReadOrigApi&") || strings.Contains(out.String(), "Control") {
		t.Fatalf("область: %s", out.String())
	}
}
