package tv

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestPacket(t *testing.T) {
	mac, err := ParseMAC("aa-bb-cc-dd-ee-ff")
	if err != nil {
		t.Fatal(err)
	}
	p := Packet(mac)
	if len(p) != 102 || !bytes.Equal(p[:6], bytes.Repeat([]byte{0xFF}, 6)) || !bytes.Equal(p[96:], mac) {
		t.Fatalf("пакет % x", p)
	}
	for _, bad := range []string{"", "aa:bb:cc", "00:00:5e:00:53:00:00:01", "zz:bb:cc:dd:ee:ff"} {
		if _, err := ParseMAC(bad); err == nil {
			t.Errorf("принят MAC %q", bad)
		}
	}
}

// Пакет доходит до адреса телевизора; без MAC шлюз ничего не отправляет.
func TestWake(t *testing.T) {
	if err := Wake(nil, "", "127.0.0.1"); err != ErrNoMAC {
		t.Fatalf("без MAC: %v", err)
	}
	conn, err := net.ListenPacket("udp4", "127.0.0.1:"+wolPort)
	if err != nil {
		t.Skipf("порт %s занят или закрыт: %v", wolPort, err)
	}
	defer conn.Close()
	mac, _ := ParseMAC("AA:BB:CC:DD:EE:FF")
	if err := Wake(mac, "127.0.0.1", "127.0.0.1:3001"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 200)
	n, _, err := conn.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], Packet(mac)) {
		t.Fatalf("пакет не дошёл: %v", err)
	}
}
