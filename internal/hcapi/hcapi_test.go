package hcapi

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// tokenServer - облако для токена приложения Home Connect: refused - отвергнуть продление.
func tokenServer(t *testing.T, refused *atomic.Bool, refreshes *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/security/oauth/token":
			r.ParseForm()
			if r.Form.Get("client_id") != "app" || r.Form.Has("client_secret") || r.Form.Get("refresh_token") == "" {
				t.Errorf("продление: %v", r.Form)
			}
			if refused.Load() {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			refreshes.Add(1)
			w.Write([]byte(`{"access_token":"new","refresh_token":"r2","expires_in":86400}`))
		case "/api/homeappliances":
			if r.Header.Get("Authorization") != "Bearer new" && r.Header.Get("Authorization") != "Bearer old" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.bsh.sdk.v1+json")
			w.Write([]byte(`{"data":{"homeappliances":[]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Токен приложения Home Connect ходит на свой адрес, продлевается без секрета, переживает
// перезапуск и удаляется, когда облако отвергло refresh token.
func TestAppTokens(t *testing.T) {
	var refused atomic.Bool
	var refreshes atomic.Int32
	srv := tokenServer(t, &refused, &refreshes)
	path := filepath.Join(t.TempDir(), "tokens.json")
	log := slog.New(slog.DiscardHandler)
	c, err := New(path, log)
	if err != nil {
		t.Fatal(err)
	}
	c.Configure(Config{ClientID: "portal", ClientSecret: "secret", BaseURL: "http://127.0.0.1:9"})
	if err := c.UseTokens(Tokens{Access: "old"}); err == nil {
		t.Fatal("неполный токен принят")
	}
	if err := c.UseTokens(Tokens{Access: "old", Refresh: "r1", ClientID: "app", BaseURL: srv.URL, Region: "cn",
		Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Appliances(context.Background()); err != nil {
		t.Fatalf("запрос на адрес токена: %v", err)
	}
	if err := c.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err = New(path, log)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := c.AppTokens()
	if !ok || info.Access != "" || info.Refresh != "" || info.Region != "cn" || info.Refreshed.IsZero() {
		t.Fatalf("после перезапуска: %+v %v", info, ok)
	}
	if token, err := c.AppAccess(context.Background()); err != nil || token != "new" {
		t.Fatalf("access: %q %v", token, err)
	}
	refused.Store(true)
	if err := c.RefreshNow(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("отказ в продлении: %v", err)
	}
	if c.LoggedIn() || !strings.Contains(c.LoginState().Error, "нужен новый вход") {
		t.Fatalf("после отказа: %+v", c.LoginState())
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("файл токенов остался: %v", err)
	}
}

// Фоновое продление: токен обновляется до конца срока без единого запроса к API.
func TestKeepFresh(t *testing.T) {
	var refused atomic.Bool
	var refreshes atomic.Int32
	srv := tokenServer(t, &refused, &refreshes)
	c, err := New(filepath.Join(t.TempDir(), "tokens.json"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c.keepCheck = time.Millisecond
	if err := c.UseTokens(Tokens{Access: "old", Refresh: "r1", ClientID: "app", BaseURL: srv.URL, Expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.KeepFresh(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for refreshes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("продлений: %d, новый токен действует сутки", n)
	}
}

// blockingToken - облако, которое отвечает на продление только после release.
func blockingToken(t *testing.T, status int, body string) (*httptest.Server, chan struct{}, chan struct{}) {
	t.Helper()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, entered, release
}

func appClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := New(filepath.Join(t.TempDir(), "tokens.json"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UseTokens(Tokens{Access: "old", Refresh: "r1", ClientID: "app", BaseURL: base, Region: "cn",
		Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return c
}

const rotated = `{"access_token":"new","refresh_token":"r2","expires_in":86400}`

// Выход, пока идёт продление: ответ облака не возвращает токены.
func TestRefreshAfterLogout(t *testing.T) {
	srv, entered, release := blockingToken(t, http.StatusOK, rotated)
	c := appClient(t, srv.URL)
	done := make(chan error, 1)
	go func() { done <- c.RefreshNow(context.Background()) }()
	<-entered
	if err := c.Logout(); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if c.LoggedIn() {
		t.Fatal("токены вернулись после выхода")
	}
	if _, err := os.Stat(c.path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("файл токенов вернулся: %v", err)
	}
}

// Новый токен, пока идёт продление старого: ни ответ, ни отказ облака его не трогают.
func TestRefreshAfterReplace(t *testing.T) {
	for name, status := range map[string]int{"ответ": http.StatusOK, "отказ": http.StatusBadRequest} {
		t.Run(name, func(t *testing.T) {
			srv, entered, release := blockingToken(t, status, rotated)
			c := appClient(t, srv.URL)
			done := make(chan error, 1)
			go func() { done <- c.RefreshNow(context.Background()) }()
			<-entered
			if err := c.UseTokens(Tokens{Access: "pasted", Refresh: "r9", ClientID: "app", BaseURL: srv.URL,
				Expires: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			close(release)
			<-done
			if token, err := c.AppAccess(context.Background()); err != nil || token != "pasted" {
				t.Fatalf("токен: %q %v", token, err)
			}
			if st := c.RefreshState(); st.Rejected || c.LoginState().Status == LoginFailed {
				t.Fatalf("отказ старому токену: %+v", st)
			}
			if _, err := os.Stat(c.path); err != nil {
				t.Fatalf("файл токенов: %v", err)
			}
		})
	}
}

// Вызывающий ушёл, пока идёт продление: новый refresh token всё равно сохраняется.
func TestRefreshOutlivesCaller(t *testing.T) {
	srv, entered, release := blockingToken(t, http.StatusOK, rotated)
	c := appClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.RefreshNow(ctx) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("вызывающий: %v", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if t2, _ := c.current(); t2 != nil && t2.Refresh == "r2" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("новый refresh token потерян")
}

// Запись в файл не удалась: новый refresh token остаётся в памяти, запросы идут дальше.
func TestRefreshSaveFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, rotated) }))
	defer srv.Close()
	c := appClient(t, srv.URL)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c.path = filepath.Join(blocker, "tokens.json")
	if err := c.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if t2, _ := c.current(); t2.Refresh != "r2" || t2.Access != "new" {
		t.Fatalf("токены: %+v", t2)
	}
}

// Сбой продления виден в состоянии до следующего успеха, отказ облака - до нового входа.
func TestRefreshState(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		if status.Load() == http.StatusOK {
			io.WriteString(w, rotated)
		}
	}))
	defer srv.Close()
	c := appClient(t, srv.URL)
	if err := c.RefreshNow(context.Background()); err == nil {
		t.Fatal("нет ошибки")
	}
	if st := c.RefreshState(); st.Failed.IsZero() || st.Rejected {
		t.Fatalf("после сбоя: %+v", st)
	}
	if !c.LoggedIn() {
		t.Fatal("сбой удалил токены")
	}
	status.Store(http.StatusOK)
	if err := c.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := c.RefreshState(); !st.Failed.IsZero() {
		t.Fatalf("после успеха: %+v", st)
	}
	status.Store(http.StatusBadRequest)
	if err := c.RefreshNow(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("отказ: %v", err)
	}
	if st := c.RefreshState(); !st.Rejected {
		t.Fatalf("после отказа: %+v", st)
	}
	if err := c.UseTokens(Tokens{Access: "a", Refresh: "b", ClientID: "app", BaseURL: srv.URL, Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if st := c.RefreshState(); st.Rejected || c.LoginState().Status != "" {
		t.Fatalf("после нового входа: %+v %+v", st, c.LoginState())
	}
	if exp, ok := c.Expires(); !ok || exp.IsZero() {
		t.Fatalf("срок: %v %v", exp, ok)
	}
}

// Смена входа и выход закрывают открытый поток, а продление токена - нет.
func TestEventsLoginChanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/security/oauth/token" {
			io.WriteString(w, rotated)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	open := func(c *Client) chan error {
		ended, up := make(chan error, 1), make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() { ended <- c.Events(ctx, func() { up <- struct{}{} }, func(Event) {}) }()
		<-up
		return ended
	}
	wait := func(ended chan error) {
		t.Helper()
		select {
		case err := <-ended:
			if !errors.Is(err, ErrLoginChanged) {
				t.Fatalf("поток: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("поток не закрылся")
		}
	}
	c := appClient(t, srv.URL)
	ended := open(c)
	if err := c.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		t.Fatalf("продление закрыло поток: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := c.UseTokens(Tokens{Access: "cn", Refresh: "r9", ClientID: "app", BaseURL: srv.URL, Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	wait(ended)
	ended = open(c)
	if err := c.Logout(); err != nil {
		t.Fatal(err)
	}
	wait(ended)
}
