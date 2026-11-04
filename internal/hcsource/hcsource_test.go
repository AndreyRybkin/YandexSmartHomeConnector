package hcsource

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/hcapi"
	"hcgateway/internal/hcapi/hcfake"
)

const (
	haID   = "BOSCH-OVEN-1"
	hotAir = "Cooking.Oven.Program.HeatingMode.HotAir"
	pizza  = "Cooking.Oven.Program.HeatingMode.PizzaSetting"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ovenFake() *hcfake.Appliance {
	temp := []map[string]any{{"key": appliance.OptionSetpointTemperature, "type": "Int", "unit": "°C",
		"constraints": map[string]any{"min": 30, "max": 250, "stepsize": 5}}}
	return &hcfake.Appliance{HaID: haID, Name: "Духовка", Type: "Oven", Brand: "Bosch", Connected: true,
		Status: map[string]any{
			appliance.StatusOperationState:     appliance.OperationReady,
			appliance.StatusDoorState:          appliance.DoorClosed,
			appliance.StatusRemoteStartAllowed: true,
			statusLocalControl:                 false,
		},
		Settings: map[string]any{appliance.SettingPowerState: appliance.PowerOn},
		Programs: []hcfake.Program{{Key: hotAir, Options: temp}, {Key: pizza, Options: temp}},
		Commands: []string{appliance.CommandPause, appliance.CommandResume}}
}

// newCloud - облако с выполненным входом в поддельный аккаунт.
func newCloud(t *testing.T, fake *hcfake.Cloud, dir string) *Cloud {
	t.Helper()
	path := filepath.Join(dir, "homeconnect-tokens.json")
	if _, err := os.Stat(path); err != nil {
		fake.TokenFile(t, path)
	}
	api, err := hcapi.New(path, discard())
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(hcapi.Config{ClientID: hcfake.ClientID, ClientSecret: hcfake.ClientSecret, BaseURL: fake.URL})
	return New(api, filepath.Join(dir, "homeconnect"), discard())
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloudAppliance(t *testing.T) {
	programPause = time.Millisecond
	fake := hcfake.New(t, ovenFake())
	dir := t.TempDir()
	c := newCloud(t, fake, dir)
	a := c.Appliance(haID)
	if a.Describe().Type != "" || a.State().Online {
		t.Fatal("описание или связь до потока событий")
	}
	events := make(chan []string, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, func(ev []string) {
		if len(ev) > 0 {
			events <- ev
		}
	})
	go c.Run(ctx)

	// Поток подключён: облако сообщает тип, состояние, команды и программы с параметрами.
	eventually(t, "описание и состояние", func() bool {
		d := a.Describe()
		return d.Type == "Oven" && len(d.Programs) == 2 && len(d.Commands) == 2 && a.State().Online
	})
	select {
	case <-c.Changes():
	default:
		t.Fatal("нет сигнала об изменении описания")
	}
	if o := a.Describe().Options[appliance.OptionSetpointTemperature]; o.Min != 30 || o.Max != 250 || o.Step != 5 {
		t.Fatalf("параметр: %+v", o)
	}
	if len(c.Account()) != 1 || !c.Connected() {
		t.Fatal("список аккаунта или связь потока")
	}

	cmd, done := context.WithTimeout(ctx, 3*time.Second)
	defer done()
	if err := a.Start(cmd, hotAir, map[string]any{appliance.OptionSetpointTemperature: 180.0}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Run", func() bool {
		st := a.State()
		return st.Values[appliance.StatusOperationState] == appliance.OperationRun && st.ActiveProgram == hotAir
	})
	// Прибор не в Ready и программу не останавливали: запуск не отправляется.
	if err := a.Start(cmd, pizza, nil); !errors.Is(err, appliance.ErrWrongMode) {
		t.Fatalf("запуск во время работы: %v", err)
	}
	// Смена программы: остановка, а новая программа запускается в фоне, когда прибор вернётся в Ready.
	if err := a.Stop(cmd); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Aborting", func() bool {
		return a.State().Values[appliance.StatusOperationState] != appliance.OperationRun
	})
	if err := a.Start(cmd, pizza, nil); err != nil {
		t.Fatalf("запуск после остановки: %v", err)
	}
	eventually(t, "новая программа", func() bool {
		st := a.State()
		return st.ActiveProgram == pizza && st.Values[appliance.StatusOperationState] == appliance.OperationRun
	})

	// Событие прибора уходит в уведомление.
	fake.Send(hcapi.EventEvent, haID, hcfake.Item(appliance.EventProgramFinished, appliance.EventPresent))
	select {
	case ev := <-events:
		if len(ev) != 1 || ev[0] != appliance.EventProgramFinished {
			t.Fatalf("события: %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("нет события")
	}
	// Местное управление: команда не отправляется.
	fake.Send(hcapi.EventStatus, haID, hcfake.Item(statusLocalControl, true))
	eventually(t, "местное управление", func() bool { return a.State().Values[statusLocalControl] == true })
	if err := a.Start(cmd, hotAir, nil); !errors.Is(err, appliance.ErrBusy) {
		t.Fatalf("местное управление: %v", err)
	}
	fake.Send(hcapi.EventDisconnected, haID)
	eventually(t, "нет связи", func() bool { return !a.State().Online })
	cancel()

	// После перезапуска описание берётся из файла, без запросов к облаку.
	again := New(c.API, filepath.Join(dir, "homeconnect"), discard()).Appliance(haID)
	if d := again.Describe(); d.Type != "Oven" || len(d.Programs) != 2 || len(d.Commands) != 2 {
		t.Fatalf("описание из файла: %+v", d)
	}
}

func TestMapErr(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{hcapi.ErrBusy, appliance.ErrBusy},
		{hcapi.ErrPaused, appliance.ErrBusy},
		{hcapi.ErrNotLoggedIn, appliance.ErrUnreachable},
		{&hcapi.APIError{Status: 400}, appliance.ErrInvalidValue},
		{&hcapi.APIError{Status: 403}, appliance.ErrNotSupported},
		{&hcapi.APIError{Status: 409}, appliance.ErrWrongMode},
		{&hcapi.APIError{Status: 429}, appliance.ErrBusy},
		{&hcapi.APIError{Status: 503}, appliance.ErrUnreachable},
		{errors.New("dial tcp: connection refused"), appliance.ErrUnreachable},
	}
	for _, c := range cases {
		if got := mapErr(c.err); !errors.Is(got, c.want) {
			t.Errorf("%v: %v", c.err, got)
		}
	}
	if mapErr(nil) != nil {
		t.Error("nil")
	}
}

// Смена токена переключает поток событий на облако нового токена сразу, без паузы.
func TestRunSwitchesLogin(t *testing.T) {
	var second atomic.Bool
	sse := func(onOpen func()) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/homeappliances/events" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if onOpen != nil {
				onOpen()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
	}
	first := sse(nil)
	defer first.Close()
	next := sse(func() { second.Store(true) })
	defer next.Close()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	api, err := hcapi.New(filepath.Join(dir, "tokens.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	tokens := func(base string) hcapi.Tokens {
		return hcapi.Tokens{Access: "a", Refresh: "r", ClientID: "app", BaseURL: base, Expires: time.Now().Add(time.Hour)}
	}
	if err := api.UseTokens(tokens(first.URL)); err != nil {
		t.Fatal(err)
	}
	c := New(api, filepath.Join(dir, "homeconnect"), log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !c.Connected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !c.Connected() {
		t.Fatal("поток не открылся")
	}
	if err := api.UseTokens(tokens(next.URL)); err != nil {
		t.Fatal(err)
	}
	for !second.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !second.Load() {
		t.Fatal("поток не перешёл на новый токен")
	}
}

// Forget забывает объект прибора: следующий вызов создаёт новый.
func TestForget(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	api, err := hcapi.New(filepath.Join(dir, "tokens.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	c := New(api, filepath.Join(dir, "homeconnect"), log)
	a := c.Appliance(haID)
	if c.Appliance(haID) != a {
		t.Fatal("новый объект без Forget")
	}
	c.Forget(haID)
	if c.Appliance(haID) == a {
		t.Fatal("объект не забыт")
	}
}

// Снимок облака не содержит событий: уровень соли после переподключения потока не сбрасывается.
func TestSnapshotKeepsEvents(t *testing.T) {
	programPause = time.Millisecond
	const salt = "Dishcare.Dishwasher.Event.SaltLack"
	c := newCloud(t, hcfake.New(t, ovenFake()), t.TempDir())
	a := c.Appliance(haID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.loadState(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.state.Values[salt] = appliance.EventPresent
	a.state.Values[appliance.StatusOperationState] = "старое"
	a.mu.Unlock()
	if err := a.loadState(ctx); err != nil {
		t.Fatal(err)
	}
	st := a.State()
	if st.Values[salt] != appliance.EventPresent {
		t.Fatalf("уровень соли: %v", st.Values[salt])
	}
	if st.Values[appliance.StatusOperationState] != appliance.OperationReady {
		t.Fatalf("состояние из снимка: %v", st.Values[appliance.StatusOperationState])
	}
}
