package tlspsk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const testIdentity = "HCCOM_Local_App"

var testPSK = bytes.Repeat([]byte{0x5a, 0x17}, 16)

// serve принимает одно соединение, выполняет рукопожатие сервера и отдаёт обратно всё прочитанное.
// Ошибка рукопожатия приходит в канал.
func serve(t *testing.T, cfg ServerConfig) (string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	errc := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer raw.Close()
		c := Server(raw, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = c.Handshake(ctx)
		errc <- err
		if err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	return ln.Addr().String(), errc
}

func dial(t *testing.T, addr, identity string, psk []byte) (*Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Dial(ctx, "tcp", addr, identity, psk)
}

// echo пишет данные на несколько записей и ждёт их обратно.
func echo(t *testing.T, c *Conn) {
	t.Helper()
	data := make([]byte, 3*maxPlaintext+100)
	rand.Read(data)
	go c.Write(data)
	got := make([]byte, len(data))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("данные вернулись другими")
	}
}

func TestRoundTrip(t *testing.T) {
	for _, g := range groups {
		for _, noEMS := range []bool{false, true} {
			t.Run(fmt.Sprintf("%#04x ems=%v", g.id, !noEMS), func(t *testing.T) {
				addr, errc := serve(t, ServerConfig{Identity: testIdentity, PSK: testPSK, Hint: "hint", Group: g.id, NoEMS: noEMS})
				c, err := dial(t, addr, testIdentity, testPSK)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if err := <-errc; err != nil {
					t.Fatalf("сервер: %v", err)
				}
				echo(t, c)
			})
		}
	}
}

// Таймаут чтения не портит соединение: так HTTP-сервер прерывает фоновое чтение перед Hijack.
func TestReadTimeout(t *testing.T) {
	addr, errc := serve(t, ServerConfig{Identity: testIdentity, PSK: testPSK})
	c, err := dial(t, addr, testIdentity, testPSK)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := <-errc; err != nil {
		t.Fatalf("сервер: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(-time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("чтение без таймаута")
	}
	c.SetReadDeadline(time.Time{})
	echo(t, c)
}

func TestWrongKey(t *testing.T) {
	addr, errc := serve(t, ServerConfig{Identity: testIdentity, PSK: []byte("другой ключ")})
	_, err := dial(t, addr, testIdentity, testPSK)
	var ae *AlertError
	if !errors.As(err, &ae) || alert(ae.Code) != alertBadRecordMAC || !strings.Contains(err.Error(), "неверный ключ") || !WrongKey(err) {
		t.Fatalf("клиент: %v", err)
	}
	if WrongKey(errors.New("connection refused")) || WrongKey(&AlertError{Code: uint8(alertHandshakeFailure)}) {
		t.Fatal("не TLS-PSK принят за неверный ключ")
	}
	var le *localError
	if err := <-errc; !errors.As(err, &le) || le.alert != alertBadRecordMAC {
		t.Fatalf("сервер: %v", err)
	}
}

func TestWrongIdentity(t *testing.T) {
	addr, errc := serve(t, ServerConfig{Identity: "другой", PSK: testPSK})
	_, err := dial(t, addr, testIdentity, testPSK)
	var ae *AlertError
	if !errors.As(err, &ae) || alert(ae.Code) != alertUnknownPSKIdentity {
		t.Fatalf("клиент: %v", err)
	}
	<-errc
}

func TestNotTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
			c.Close()
		}
	}()
	if _, err := dial(t, ln.Addr().String(), testIdentity, testPSK); err == nil || !strings.Contains(err.Error(), "не похож на TLS") {
		t.Fatalf("ошибка: %v", err)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		// Сервер принимает соединение и молчит.
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Dial(ctx, "tcp", ln.Addr().String(), testIdentity, testPSK)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("ошибка: %v за %v", err, time.Since(start))
	}
}

// opensslPath - OpenSSL с наборами PSK: из переменной OPENSSL, из PATH или из Homebrew.
// Системный openssl macOS - это LibreSSL, PSK он не знает.
func opensslPath(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("OPENSSL")}
	if p, err := exec.LookPath("openssl"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/homebrew/bin/openssl", "/usr/local/opt/openssl/bin/openssl")
	for _, p := range candidates {
		if p != "" && exec.Command(p, "ciphers", "-psk", "ECDHE-PSK-CHACHA20-POLY1305").Run() == nil {
			return p
		}
	}
	t.Skip("нет OpenSSL с наборами PSK: задайте OPENSSL")
	return ""
}

// openssl запускает s_server на одно соединение и ждёт, пока он начнёт принимать соединения.
func openssl(t *testing.T, path string, args ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, path, append([]string{"s_server", "-accept", addr, "-nocert", "-tls1_2",
		"-cipher", "ECDHE-PSK-CHACHA20-POLY1305", "-psk_identity", testIdentity, "-psk", hex.EncodeToString(testPSK),
		"-psk_hint", "hint", "-naccept", "1", "-www"}, args...)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "ACCEPT") {
				close(ready)
				io.Copy(io.Discard, out)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("s_server не запустился")
	}
	return addr
}

func TestOpenSSL(t *testing.T) {
	path := opensslPath(t)
	for name, args := range map[string][]string{
		"x25519":      {"-groups", "X25519"},
		"P-256":       {"-groups", "P-256"},
		"P-384":       {"-groups", "P-384"},
		"без EMS":     {"-no_ems"},
		"другой ключ": nil,
	} {
		t.Run(name, func(t *testing.T) {
			addr := openssl(t, path, args...)
			psk := testPSK
			if args == nil {
				psk = []byte("другой ключ")
			}
			c, err := dial(t, addr, testIdentity, psk)
			if args == nil {
				var ae *AlertError
				if !errors.As(err, &ae) {
					t.Fatalf("с чужим ключом: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			page, err := io.ReadAll(c)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(page, []byte("HTTP/1.0 200 ok")) || !bytes.Contains(page, []byte("ECDHE-PSK-CHACHA20-POLY1305")) {
				t.Fatalf("страница s_server:\n%s", page)
			}
		})
	}
}
