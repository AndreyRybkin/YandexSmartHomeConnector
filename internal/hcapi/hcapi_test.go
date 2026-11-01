package hcapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hcgateway/internal/hcapi/hcfake"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func oven() *hcfake.Appliance {
	return &hcfake.Appliance{HaID: "BOSCH-OVEN-1", Name: "Духовка", Type: "Oven", Brand: "Bosch", Connected: true,
		Status:   map[string]any{"BSH.Common.Status.OperationState": "BSH.Common.EnumType.OperationState.Ready"},
		Settings: map[string]any{}}
}

// newClient - клиент поддельного облака. Паузы в тестах - 1 мс.
func newClient(t *testing.T, cloud *hcfake.Cloud) *Client {
	t.Helper()
	c, err := New(filepath.Join(t.TempDir(), "homeconnect-tokens.json"), discard())
	if err != nil {
		t.Fatal(err)
	}
	c.Configure(Config{ClientID: hcfake.ClientID, ClientSecret: hcfake.ClientSecret, BaseURL: cloud.URL})
	c.lim.sleep = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, time.Millisecond) }
	return c
}

// login входит в поддельное облако по Device Flow.
func login(t *testing.T, c *Client, cloud *hcfake.Cloud) {
	t.Helper()
	done := make(chan struct{})
	l, err := c.StartLogin(context.Background(), func() { close(done) })
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != LoginPending || l.UserCode != hcfake.UserCode || l.VerificationURIComplete == "" {
		t.Fatalf("вход: %+v", l)
	}
	cloud.Approve()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("вход не завершился: %+v", c.LoginState())
	}
}

func TestLogin(t *testing.T) {
	cloud := hcfake.New(t, oven())
	c := newClient(t, cloud)
	if c.LoggedIn() {
		t.Fatal("вход без токенов")
	}
	if _, err := c.Appliances(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("запрос без входа: %v", err)
	}
	// slow_down не прерывает опрос.
	cloud.SlowDown()
	login(t, c, cloud)
	if !c.LoggedIn() || c.LoginState().Status != LoginDone {
		t.Fatalf("после входа: %+v", c.LoginState())
	}
	if info, err := os.Stat(c.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("файл токенов: %v", err)
	}
	list, err := c.Appliances(context.Background())
	if err != nil || len(list) != 1 || list[0].Type != "Oven" || !list[0].Connected {
		t.Fatalf("приборы: %+v %v", list, err)
	}
	// Токены переживают перезапуск.
	again, err := New(c.path, discard())
	if err != nil || !again.LoggedIn() {
		t.Fatalf("после перезапуска: %v", err)
	}
	if err := again.Logout(); err != nil || again.LoggedIn() {
		t.Fatal("выход")
	}
	if _, err := os.Stat(c.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("токены не удалены")
	}
}

func TestRefresh(t *testing.T) {
	cloud := hcfake.New(t, oven())
	c := newClient(t, cloud)
	login(t, c, cloud)
	// Срок access token подходит к концу: перед запросом токен обновляется с client_secret.
	c.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if _, err := c.Appliances(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cloud.Refreshes() != 1 {
		t.Fatalf("обновлений %d", cloud.Refreshes())
	}
	c.now = time.Now
	// Облако отозвало токен раньше срока: 401, обновление и повтор запроса.
	cloud.Revoke()
	if _, err := c.Appliances(context.Background()); err != nil {
		t.Fatalf("после отзыва: %v", err)
	}
	if cloud.Refreshes() != 2 {
		t.Fatalf("обновлений %d", cloud.Refreshes())
	}
	// Неверный client_secret: облако отвергает обновление, нужен новый вход.
	c.Configure(Config{ClientID: hcfake.ClientID, ClientSecret: "wrong", BaseURL: cloud.URL})
	cloud.Revoke()
	if _, err := c.Appliances(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("неверный secret: %v", err)
	}
	if c.LoggedIn() || c.LoginState().Error == "" {
		t.Fatal("токены остались или нет ошибки входа")
	}
}

func TestErrors(t *testing.T) {
	cloud := hcfake.New(t, oven())
	c := newClient(t, cloud)
	login(t, c, cloud)
	ctx := context.Background()
	var ae *APIError
	cloud.Fail = http.StatusConflict
	if err := c.StartProgram(ctx, "BOSCH-OVEN-1", "P", nil); !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Key != "SDK.Error.Fake" {
		t.Fatalf("ошибка облака: %v", err)
	}
	if p, err := c.ActiveProgram(ctx, "BOSCH-OVEN-1"); p != nil || err != nil {
		t.Fatalf("нет активной программы: %+v %v", p, err)
	}

	// Чтение после 5xx повторяется.
	cloud.Fail = http.StatusBadGateway
	if _, err := c.Status(ctx, "BOSCH-OVEN-1"); err != nil {
		t.Fatalf("повтор после 502: %v", err)
	}
	// После errorStreak ошибок подряд - пауза без запросов к облаку.
	for range errorStreak {
		cloud.Fail = http.StatusConflict
		c.SelectProgram(ctx, "BOSCH-OVEN-1", "P")
	}
	before := len(cloud.Requests)
	if err := c.SelectProgram(ctx, "BOSCH-OVEN-1", "P"); !errors.Is(err, ErrPaused) || len(cloud.Requests) != before {
		t.Fatalf("пауза: %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	cloud := hcfake.New(t, oven())
	c := newClient(t, cloud)
	login(t, c, cloud)
	cloud.Fail = http.StatusTooManyRequests
	c.Status(context.Background(), "BOSCH-OVEN-1")
	// Команда Алисы ждёт 3 с, а облако просит паузу 52 с: команда сразу получает ErrBusy.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.lim.sleep = sleepCtx
	before := len(cloud.Requests)
	if err := c.SelectProgram(ctx, "BOSCH-OVEN-1", "P"); !errors.Is(err, ErrBusy) || len(cloud.Requests) != before {
		t.Fatalf("после 429: %v", err)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	l := newLimiter()
	l.now = func() time.Time { return now }
	var slept time.Duration
	l.sleep = func(ctx context.Context, d time.Duration) error {
		slept += d
		now = now.Add(d)
		return nil
	}
	ctx := context.Background()
	// Всплеск до 20 запросов без ожидания, дальше - 10 в секунду.
	for range burst {
		l.wait(ctx)
	}
	if slept != 0 {
		t.Fatalf("всплеск ждал %v", slept)
	}
	l.wait(ctx)
	if slept != 100*time.Millisecond {
		t.Fatalf("21-й запрос ждал %v", slept)
	}
	// Не больше 50 запросов в минуту.
	for range perMinute - burst - 1 {
		l.wait(ctx)
	}
	slept = 0
	l.wait(ctx)
	if slept < 50*time.Second {
		t.Fatalf("51-й запрос в минуту ждал %v", slept)
	}
	// Ожидание дольше дедлайна - сразу ErrBusy.
	short, cancel := context.WithDeadline(ctx, now.Add(time.Second))
	defer cancel()
	l.block(time.Minute)
	if err := l.wait(short); !errors.Is(err, ErrBusy) {
		t.Fatalf("дедлайн: %v", err)
	}
	// Пять запусков в минуту, шестой не отправляется.
	for i := range actionsPerMinute {
		if !l.action("start") {
			t.Fatalf("запуск %d", i+1)
		}
	}
	if l.action("start") || !l.action("stop") {
		t.Fatal("лимит запусков")
	}
	now = now.Add(time.Minute)
	if !l.action("start") {
		t.Fatal("лимит запусков через минуту")
	}
}

func TestEvents(t *testing.T) {
	cloud := hcfake.New(t, oven())
	c := newClient(t, cloud)
	login(t, c, cloud)
	got := make(chan Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- c.Events(ctx, func() { close(connected) }, func(e Event) { got <- e }) }()
	<-connected
	if e := <-got; e.Type != EventKeepAlive || len(e.Items) != 0 {
		t.Fatalf("первое событие: %+v", e)
	}
	cloud.Send(EventStatus, "BOSCH-OVEN-1", hcfake.Item("BSH.Common.Status.DoorState", "BSH.Common.EnumType.DoorState.Open"),
		hcfake.Item("BSH.Common.Status.LocalControlActive", false))
	e := <-got
	if e.Type != EventStatus || e.HaID != "BOSCH-OVEN-1" || len(e.Items) != 2 || e.Items[1].Value != false {
		t.Fatalf("событие: %+v", e)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("после отмены: %v", err)
	}

	// Поток, из которого ничего не приходит, считается оборванным.
	streamIdle = 100 * time.Millisecond
	defer func() { streamIdle = 3 * time.Minute }()
	if err := c.Events(context.Background(), func() {}, func(Event) {}); !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("молчащий поток: %v", err)
	}
}

func TestParseItems(t *testing.T) {
	if items := parseItems(`{"key":"BSH.Common.Root.ActiveProgram","value":"P"}`); len(items) != 1 || items[0].Value != "P" {
		t.Fatalf("одно изменение: %+v", items)
	}
	if items := parseItems(""); items != nil {
		t.Fatal("пустые данные")
	}
	if items := parseItems("не json"); items != nil {
		t.Fatal("не JSON")
	}
}
