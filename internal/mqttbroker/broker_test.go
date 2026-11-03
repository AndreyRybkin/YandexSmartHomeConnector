package mqttbroker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"hcgateway/internal/statedb"
)

const (
	testUser     = "hcgateway"
	testPassword = "0123456789abcdef"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// freeAddr - свободный порт на этом компьютере.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// start запускает брокер и ждёт, пока он откроет порт.
func start(t *testing.T, state *statedb.DB, addr string) (*Broker, func()) {
	t.Helper()
	b := New(Config{Listen: addr, Credentials: func() (string, string) { return testUser, testPassword }}, state, discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	waitFor(t, "брокер", func() bool { return b.Status("").Open })
	var once bool
	stop := func() {
		if !once {
			once = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return b, stop
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("не дождались: %s", what)
		}
	}
}

func client(addr, id, user, password string, version uint) paho.Client {
	o := paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID(id).SetUsername(user).SetPassword(password).
		SetConnectRetry(false).SetAutoReconnect(false).SetProtocolVersion(version).SetConnectTimeout(3 * time.Second)
	return paho.NewClient(o)
}

func connect(t *testing.T, addr, id string) paho.Client {
	t.Helper()
	c := client(addr, id, testUser, testPassword, 4)
	if tok := c.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	t.Cleanup(func() { c.Disconnect(10) })
	return c
}

func publish(t *testing.T, c paho.Client, topic, payload string, retain bool) {
	t.Helper()
	if tok := c.Publish(topic, 1, retain, payload); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
}

// Вход: неверный пароль и логин отклоняются и видны на странице, верные пропускают клиента. Брокер
// понимает и MQTT 3.1.1, и старый 3.1.
func TestAuth(t *testing.T) {
	addr := freeAddr(t)
	b, _ := start(t, nil, addr)
	for _, cred := range [][2]string{{testUser, "wrong-password-00"}, {"other", testPassword}, {"", ""}} {
		c := client(addr, "bad", cred[0], cred[1], 4)
		if tok := c.Connect(); tok.Wait() && tok.Error() == nil {
			c.Disconnect(10)
			t.Fatalf("вход с %v прошёл", cred)
		}
	}
	if f := b.Status("").Failures; len(f) != 3 || f[0].User != "" || f[2].User != testUser {
		t.Fatalf("неудачные входы: %+v", f)
	}
	for _, version := range []uint{3, 4} {
		id := fmt.Sprint("kettle-", version)
		c := connect(t, addr, id)
		_ = c
	}
	waitFor(t, "клиенты", func() bool { return len(b.Status("").Clients) == 2 })
	st := b.Status("").Clients
	if st[0].ID != "kettle-3" || st[0].User != testUser || st[1].Version != 4 || st[0].Remote == "" || st[0].Since.IsZero() {
		t.Fatalf("клиенты: %+v", st)
	}
}

// Сообщения: брокер передаёт между клиентами любые топики, журнал и значения видят их, а Publish
// шлюза отличается направлением.
func TestMessages(t *testing.T) {
	addr := freeAddr(t)
	b, _ := start(t, nil, addr)
	sub := connect(t, addr, "listener")
	got := make(chan string, 8)
	if tok := sub.Subscribe("#", 1, func(_ paho.Client, m paho.Message) { got <- m.Topic() + "=" + string(m.Payload()) }); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	kettle := connect(t, addr, "kettle")
	publish(t, kettle, "polaris/1/state/temperature", "80", false)
	publish(t, kettle, "wled/strip/g", "128", false)
	for _, want := range []string{"polaris/1/state/temperature=80", "wled/strip/g=128"} {
		select {
		case m := <-got:
			if m != want {
				t.Fatalf("получено %s, ждали %s", m, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("не дошло %s", want)
		}
	}
	if err := b.Publish("polaris/1/control/mode", []byte("1"), false, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != "polaris/1/control/mode=1" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("команда шлюза не дошла")
	}
	waitFor(t, "журнал", func() bool { return len(b.Status("").Journal) == 3 })
	st := b.Status("polaris/")
	if len(st.Journal) != 2 || st.Journal[0].Dir != Out || st.Journal[0].Client != "шлюз" || st.Journal[1].Dir != In || st.Journal[1].Client != "kettle" {
		t.Fatalf("журнал: %+v", st.Journal)
	}
	if len(st.Values) != 2 || st.Values[1].Topic != "polaris/1/state/temperature" || st.Values[1].Payload != "80" {
		t.Fatalf("значения: %+v", st.Values)
	}
}

// Журнал хранит последние сообщения, а длинное и не UTF-8 значение безопасно для показа.
func TestJournalLimits(t *testing.T) {
	addr := freeAddr(t)
	b, _ := start(t, nil, addr)
	c := connect(t, addr, "noisy")
	for i := range journalSize + 3 {
		c.Publish(fmt.Sprint("t/", i), 0, false, "v")
	}
	publish(t, c, "long", strings.Repeat("я", maxShown)+"\xff", false)
	waitFor(t, "длинное значение", func() bool { s := b.Status("long"); return len(s.Journal) == 1 })
	st := b.Status("")
	if len(st.Journal) != journalSize {
		t.Fatalf("журнал: %d", len(st.Journal))
	}
	e := b.Status("long").Journal[0]
	if len(e.Payload) > maxShown+3 || !e.PayloadCut || !strings.HasPrefix(e.Payload, "я") {
		t.Fatalf("значение: %d %v", len(e.Payload), e.PayloadCut)
	}
}

// Сохранённое сообщение переживает перезапуск шлюза, новый подписчик его получает, значение помечено как
// старое, а пустое сообщение стирает топик. Предел: лишнее не хранится, и страница об этом знает.
func TestRetainedRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), statedb.FileName)
	state := statedb.Open(path, discard())
	addr := freeAddr(t)
	b, stop := start(t, state, addr)
	c := connect(t, addr, "kettle")
	publish(t, c, "polaris/1/state/mode", "3", true)
	publish(t, c, "gone/topic", "x", true)
	publish(t, c, "live/only", "y", false)
	publish(t, c, "gone/topic", "", true)
	publish(t, c, "too/big", strings.Repeat("x", statedb.MaxRetainedBytes+1), true)
	waitFor(t, "сохранение", func() bool { return state.RetainedCount() == 1 && b.Status("").Unsaved == 1 })
	c.Disconnect(10)
	stop()
	state.Close()

	state = statedb.Open(path, discard())
	defer state.Close()
	b, _ = start(t, state, addr)
	st := b.Status("")
	if st.Saved != 1 || len(st.Values) != 1 || !st.Values[0].Saved || st.Values[0].Payload != "3" || st.Values[0].Client != "" {
		t.Fatalf("после перезапуска: %+v", st)
	}
	sub := connect(t, addr, "listener")
	got := make(chan string, 4)
	if tok := sub.Subscribe("#", 1, func(_ paho.Client, m paho.Message) { got <- m.Topic() + "=" + string(m.Payload()) }); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	select {
	case m := <-got:
		if m != "polaris/1/state/mode=3" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("сохранённое сообщение не дошло после перезапуска")
	}
	select {
	case m := <-got:
		t.Fatalf("лишнее сообщение: %s", m)
	case <-time.After(100 * time.Millisecond):
	}
	publish(t, sub, "polaris/1/state/mode", "1", false)
	waitFor(t, "новое значение", func() bool { v := b.Status("polaris").Values; return len(v) == 1 && !v[0].Saved && v[0].Payload == "1" })
}

// Соединение, которое молчит и не входит, закрывается и освобождает место.
func TestSilentConnection(t *testing.T) {
	old := connectWait
	connectWait = 100 * time.Millisecond
	defer func() { connectWait = old }()
	addr := freeAddr(t)
	start(t, nil, addr)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("брокер что-то прислал молчащему соединению")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("брокер не закрыл молчащее соединение")
	}
}

// Вошедший клиент не отключается из-за срока до входа: его ведёт keep-alive.
func TestLoggedInStaysConnected(t *testing.T) {
	old := connectWait
	connectWait = 100 * time.Millisecond
	defer func() { connectWait = old }()
	addr := freeAddr(t)
	b, _ := start(t, nil, addr)
	c := connect(t, addr, "kettle")
	time.Sleep(400 * time.Millisecond)
	if !c.IsConnected() || len(b.Status("").Clients) != 1 {
		t.Fatalf("клиент отключён: %v %+v", c.IsConnected(), b.Status("").Clients)
	}
}

// Занятый порт не мешает шлюзу: брокер сообщает причину.
func TestPortBusy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	b := New(Config{Listen: l.Addr().String(), Credentials: func() (string, string) { return testUser, testPassword }}, nil, discard())
	b.Run(context.Background())
	if st := b.Status(""); st.Open || st.Err == "" {
		t.Fatalf("состояние: %+v", st)
	}
	if err := b.Publish("a", []byte("b"), false, 0); err != ErrClosed {
		t.Fatalf("публикация: %v", err)
	}
}
