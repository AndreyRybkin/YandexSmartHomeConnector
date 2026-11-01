package discovery

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"hcgateway/internal/config"
)

func TestHosts(t *testing.T) {
	for p, want := range map[string]int{"192.168.1.0/24": 254, "192.168.1.0/30": 2, "192.168.1.0/31": 2, "192.168.1.7/32": 1} {
		if got := len(hosts(netip.MustParsePrefix(p))); got != want {
			t.Errorf("%s: %d адресов, нужно %d", p, got, want)
		}
	}
	if h := hosts(netip.MustParsePrefix("10.0.0.0/30")); h[0].String() != "10.0.0.1" || h[1].String() != "10.0.0.2" {
		t.Fatalf("адреса /30: %v", h)
	}
}

// Поиск проверяет все адреса сети, дополняет находки сведениями облака, хранит их 10 минут
// и не запускается чаще раза в минуту.
func TestSearch(t *testing.T) {
	var mu sync.Mutex
	checked := map[string]bool{}
	s := New(func(ctx context.Context, ip netip.Addr) []Found {
		mu.Lock()
		checked[ip.String()] = true
		mu.Unlock()
		switch ip.String() {
		case "192.168.1.5":
			return []Found{{Source: config.SourceDeLonghi, Host: "192.168.1.5"}}
		case "192.168.1.3":
			return []Found{{Source: config.SourceWLED, Host: "192.168.1.3", Name: "Лента"}}
		}
		return nil
	})
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	cloud := func(context.Context) map[netip.Addr]Found {
		return map[netip.Addr]Found{netip.MustParseAddr("192.168.1.5"): {Source: config.SourceDeLonghi, Serial: "000000ZZ00000000000",
			Model: "ECAM63070"}}
	}
	p := netip.MustParsePrefix("192.168.1.0/29")
	if err := s.Start(p, cloud); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(p, nil); !errors.Is(err, ErrBusy) && !errors.Is(err, ErrTooSoon) {
		t.Fatalf("второй поиск сразу: %v", err)
	}
	st := wait(t, s)
	if st.Total != 6 || st.Checked != 6 || len(checked) != 6 || st.Partial || len(st.Found) != 2 {
		t.Fatalf("поиск: %+v, проверены %v", st, checked)
	}
	if st.Found[0].Host != "192.168.1.3" || st.Found[1].Serial != "000000ZZ00000000000" || st.Found[1].Model != "ECAM63070" {
		t.Fatalf("находки: %+v", st.Found)
	}
	if err := s.Start(p, nil); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("поиск через секунду: %v", err)
	}
	now = now.Add(11 * time.Minute)
	if st := s.Status(); st.Found != nil {
		t.Fatal("находки старше 10 минут остались")
	}
	if err := s.Start(netip.MustParsePrefix("192.168.0.0/21"), nil); err == nil {
		t.Fatal("сеть больше /22 принята")
	}
	if err := s.Start(p, nil); err != nil {
		t.Fatalf("поиск через 11 минут: %v", err)
	}
	wait(t, s)
}

func wait(t *testing.T, s *Search) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.Status(); !st.Running {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("поиск не закончился за 5 с")
	return Status{}
}
