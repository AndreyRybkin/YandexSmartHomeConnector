package yandex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/metrics"
	"hcgateway/internal/profile"
)

const (
	hotAir = "Cooking.Oven.Program.HeatingMode.HotAir"
	pizza  = "Cooking.Oven.Program.HeatingMode.PizzaSetting"
)

type fakeTokens struct{ revoked bool }

func (f *fakeTokens) Valid(access string) bool { return access == "good" }
func (f *fakeTokens) RevokeAll() error         { f.revoked = true; return nil }
func (f *fakeTokens) Linked() bool             { return true }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newDevice(t *testing.T, id, kind string) *hcdevice.Device {
	t.Helper()
	a, err := appliance.NewDemo(kind, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.Resolve(kind, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &hcdevice.Device{ID: id, Name: id, Room: "Кухня", Profile: p, Appliance: a}
}

type fixture struct {
	oven, dish *hcdevice.Device
	tokens     *fakeTokens
	url        string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{oven: newDevice(t, "oven", "Oven"), dish: newDevice(t, "dish", "Dishwasher"), tokens: &fakeTokens{}}
	svc := NewService([]alice.Device{f.oven, f.dish}, f.tokens, nil, discard())
	mux := http.NewServeMux()
	svc.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	f.url = hs.URL
	return f
}

func (f *fixture) call(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	status, out, _ := f.callRaw(t, method, path, token, body)
	return status, out
}

// callRaw возвращает и разобранный ответ, и его байты как есть: порядок полей в них не меняется.
func (f *fixture) callRaw(t *testing.T, method, path, token, body string) (int, map[string]any, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Request-Id", "req-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

// act отправляет одну команду и возвращает её результат.
func (f *fixture) act(t *testing.T, id, capType, instance string, value any) map[string]any {
	t.Helper()
	cmd := map[string]any{"type": capType, "state": map[string]any{"instance": instance, "value": value}}
	body, _ := json.Marshal(map[string]any{"payload": map[string]any{"devices": []any{
		map[string]any{"id": id, "capabilities": []any{cmd}}}}})
	status, out := f.call(t, http.MethodPost, "/v1.0/user/devices/action", "good", string(body))
	if status != http.StatusOK {
		t.Fatalf("action: %d", status)
	}
	dev := out["payload"].(map[string]any)["devices"].([]any)[0].(map[string]any)
	c := dev["capabilities"].([]any)[0].(map[string]any)
	return c["state"].(map[string]any)["action_result"].(map[string]any)
}

func mustDone(t *testing.T, res map[string]any) {
	t.Helper()
	if res["status"] != "DONE" {
		t.Fatalf("результат %v", res)
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t)
	for _, token := range []string{"", "bad"} {
		if status, out := f.call(t, http.MethodGet, "/v1.0/user/devices", token, ""); status != http.StatusUnauthorized || out != nil {
			t.Fatalf("токен %q: %d %v", token, status, out)
		}
	}
	if status, _ := f.call(t, http.MethodHead, "/v1.0", "", ""); status != http.StatusOK {
		t.Fatalf("HEAD: %d", status)
	}
	if status, out := f.call(t, http.MethodPost, "/v1.0/user/unlink", "good", ""); status != http.StatusOK || out["request_id"] != "req-1" || !f.tokens.revoked {
		t.Fatalf("unlink: %d %v", status, out)
	}
}

func TestDevicesList(t *testing.T) {
	f := newFixture(t)
	status, out, raw := f.callRaw(t, http.MethodGet, "/v1.0/user/devices", "good", "")
	if status != http.StatusOK || out["request_id"] != "req-1" {
		t.Fatalf("%d %v", status, out)
	}
	payload := out["payload"].(map[string]any)
	devices := payload["devices"].([]any)
	if payload["user_id"] != UserID || len(devices) != 2 {
		t.Fatalf("устройств %d: %v", len(devices), payload)
	}
	for _, want := range []string{
		`"id":"oven","name":"oven","room":"Кухня","type":"devices.types.cooking"`,
		`"instance":"program","modes":[{"value":"one"},{"value":"frying"},{"value":"baking"},{"value":"eco"},{"value":"pizza"},{"value":"preheat"},{"value":"slow_cook"},{"value":"normal"}]`,
		`"instance":"temperature","unit":"unit.temperature.celsius","random_access":true,"range":{"min":30,"max":275,"precision":5}`,
		`"instance":"button","events":[{"value":"click"},{"value":"double_click"}]`,
		`"instance":"dishwashing"`,
		`"instance":"food_level","events":[{"value":"empty"},{"value":"low"},{"value":"normal"}]`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("нет %s", want)
		}
	}
	if bytes.Contains(raw, []byte(`.events`)) || bytes.Contains(raw, []byte(`sensor.button`)) {
		t.Errorf("отдельное устройство кнопки: %s", raw)
	}
}

func TestQuery(t *testing.T) {
	f := newFixture(t)
	status, _, raw := f.callRaw(t, http.MethodPost, "/v1.0/user/devices/query", "good",
		`{"devices":[{"id":"oven"},{"id":"dish"},{"id":"dish.events"},{"id":"nope"}]}`)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	for _, want := range []string{
		`{"type":"devices.capabilities.on_off","state":{"instance":"on","value":false}}`,
		`{"type":"devices.capabilities.range","state":{"instance":"temperature","value":180}}`,
		`{"type":"devices.properties.event","state":{"instance":"open","value":"closed"}}`,
		`{"type":"devices.properties.float","state":{"instance":"temperature","value":22}}`,
		`{"type":"devices.properties.event","state":{"instance":"food_level","value":"normal"}},{"type":"devices.properties.event","state":{"instance":"water_level","value":"normal"}}]}`,
		`{"id":"dish.events","error_code":"DEVICE_NOT_FOUND"}`,
		`{"id":"nope","error_code":"DEVICE_NOT_FOUND"}`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("нет %s в %s", want, raw)
		}
	}
}

func TestActions(t *testing.T) {
	f := newFixture(t)
	oven := f.oven.Appliance

	// Температура без программы запоминается для запуска, режим выбирается без запуска.
	mustDone(t, f.act(t, "oven", alice.CapRange, "temperature", 200.0))
	mustDone(t, f.act(t, "oven", alice.CapMode, "program", "pizza"))
	if st := oven.State(); st.Running() || st.SelectedProgram != pizza {
		t.Fatalf("выбор режима: %+v", st)
	}
	mustDone(t, f.act(t, "oven", alice.CapOnOff, "on", true))
	st := oven.State()
	if st.ActiveProgram != pizza || st.Values[appliance.OptionSetpointTemperature] != 200.0 {
		t.Fatalf("запуск: %+v", st)
	}

	// Смена режима во время работы - остановка и запуск новой программы.
	mustDone(t, f.act(t, "oven", alice.CapMode, "program", "baking"))
	if st := oven.State(); st.ActiveProgram != hotAir || !st.Running() {
		t.Fatalf("смена режима: %+v", st)
	}
	mustDone(t, f.act(t, "oven", alice.CapRange, "temperature", 150.0))
	if oven.State().Values[appliance.OptionSetpointTemperature] != 150.0 {
		t.Fatal("температура работающей программы")
	}

	mustDone(t, f.act(t, "oven", alice.CapToggle, "pause", true))
	if oven.State().Values[appliance.StatusOperationState] != appliance.OperationPause {
		t.Fatal("пауза")
	}
	mustDone(t, f.act(t, "oven", alice.CapToggle, "pause", false))
	mustDone(t, f.act(t, "oven", alice.CapOnOff, "on", false))
	if oven.State().Running() {
		t.Fatal("остановка")
	}

	// Ошибки с кодами Яндекса.
	cases := []struct {
		capType, instance string
		value             any
		code              string
	}{
		{alice.CapRange, "temperature", 500.0, alice.CodeInvalidValue},
		{alice.CapRange, "temperature", "hot", alice.CodeInvalidValue},
		{alice.CapMode, "program", "steam", alice.CodeInvalidValue},
		{alice.CapToggle, "pause", true, alice.CodeWrongMode},
		{alice.CapToggle, "backlight", true, alice.CodeInvalidAction},
		{"devices.capabilities.color_setting", "rgb", 1.0, alice.CodeInvalidAction},
	}
	for _, c := range cases {
		if res := f.act(t, "oven", c.capType, c.instance, c.value); res["error_code"] != c.code {
			t.Errorf("%s %s %v: %v, нужно %s", c.capType, c.instance, c.value, res, c.code)
		}
	}
	if res := f.act(t, "nope", alice.CapOnOff, "on", true); res["error_code"] != alice.CodeNotFound {
		t.Errorf("нет устройства: %v", res)
	}
}

func TestActionsDefaultProgram(t *testing.T) {
	f := newFixture(t)
	mustDone(t, f.act(t, "dish", alice.CapOnOff, "on", true))
	if st := f.dish.Appliance.State(); st.ActiveProgram != "Dishcare.Dishwasher.Program.Auto2" {
		t.Fatalf("программа по умолчанию: %+v", st)
	}
	mustDone(t, f.act(t, "oven", alice.CapOnOff, "on", true))
	st := f.oven.Appliance.State()
	if st.ActiveProgram != hotAir || st.Values[appliance.OptionSetpointTemperature] != 180.0 {
		t.Fatalf("духовка по умолчанию: %+v", st)
	}
}

// remoteOff - демо-прибор с выключенным удалённым стартом.
type remoteOff struct{ *appliance.Demo }

func (r remoteOff) State() appliance.State {
	st := r.Demo.State()
	st.Values[appliance.StatusRemoteStartAllowed] = false
	return st
}

func TestActionRemoteDisabled(t *testing.T) {
	f := newFixture(t)
	f.oven.Appliance = remoteOff{f.oven.Appliance.(*appliance.Demo)}
	if res := f.act(t, "oven", alice.CapOnOff, "on", true); res["error_code"] != alice.CodeRemoteDisabled {
		t.Fatalf("%v", res)
	}
	if f.oven.Appliance.State().Running() {
		t.Fatal("прибор не должен получить команду")
	}
}

func TestLimits(t *testing.T) {
	f := newFixture(t)
	var ids []string
	for i := 0; i <= maxRequestDevices; i++ {
		ids = append(ids, `{"id":"oven"}`)
	}
	if status, _ := f.call(t, http.MethodPost, "/v1.0/user/devices/query", "good", `{"devices":[`+strings.Join(ids, ",")+`]}`); status != http.StatusBadRequest {
		t.Fatalf("много устройств: %d", status)
	}
	big := `{"devices":[{"id":"` + strings.Repeat("a", maxBody) + `"}]}`
	if status, _ := f.call(t, http.MethodPost, "/v1.0/user/devices/query", "good", big); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("большое тело: %d", status)
	}
	if status, _ := f.call(t, http.MethodPost, "/v1.0/user/devices/action", "good", "not json"); status != http.StatusBadRequest {
		t.Fatalf("не JSON: %d", status)
	}
}

type captured struct {
	path, auth string
	body       []byte
}

func TestNotifications(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []captured
		fail = 1
	)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail > 0 {
			fail--
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(r.Body)
		got = append(got, captured{r.URL.Path, r.Header.Get("Authorization"), body})
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hs.Close()

	n := NewNotifier(hs.URL, "skill-1", "owner-token", UserID, discard())
	n.sleep = func(context.Context, time.Duration) error { return nil }
	oven := newDevice(t, "oven", "Oven")
	m := manual{Demo: oven.Appliance.(*appliance.Demo), notify: make(chan appliance.Notify, 1)}
	oven.Appliance = m
	svc := NewService([]alice.Device{oven}, &fakeTokens{}, n, discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()

	// Окончание программы: нажатие кнопки сразу и состояние после паузы debounce.
	notify := <-m.notify
	m.Start(context.Background(), hotAir, nil)
	notify([]string{appliance.EventProgramFinished})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("уведомлений %d: %+v", len(got), got)
	}
	if metrics.Value("hcgateway_yandex_notifications_total", "kind", "state", "result", metrics.NotifyOK) < 2 ||
		metrics.Value("hcgateway_yandex_notifications_total", "kind", "discovery", "result", metrics.NotifyOK) < 1 {
		t.Fatal("уведомления не посчитаны")
	}
	if got[0].path != "/api/v1/skills/skill-1/callback/discovery" || got[0].auth != "OAuth owner-token" {
		t.Fatalf("discovery: %+v", got[0])
	}
	raw := got[1].body
	if got[1].path != "/api/v1/skills/skill-1/callback/state" ||
		!bytes.Contains(raw, []byte(`{"id":"oven","properties":[{"type":"devices.properties.event","state":{"instance":"button","value":"click"}}]}`)) {
		t.Fatalf("событие: %s", raw)
	}
	raw = got[2].body
	if !bytes.Contains(raw, []byte(`"user_id":"owner"`)) || !bytes.Contains(raw, []byte(`"id":"oven","status":"online","capabilities"`)) {
		t.Fatalf("состояние: %s", raw)
	}
}

// manual - демо-прибор, о событиях которого тест сообщает сам.
type manual struct {
	*appliance.Demo
	notify chan appliance.Notify
}

func (m manual) Run(ctx context.Context, notify appliance.Notify) {
	m.notify <- notify
	<-ctx.Done()
}

// tracked - прибор, у которого видно, сколько раз его запускали и когда остановили.
type tracked struct {
	*hcdevice.Device
	runs    atomic.Int32
	stopped chan struct{}
}

func (d *tracked) Run(ctx context.Context, changed alice.Changed) {
	d.runs.Add(1)
	<-ctx.Done()
	close(d.stopped)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSetDevices(t *testing.T) {
	oven := &tracked{Device: newDevice(t, "oven", "Oven"), stopped: make(chan struct{})}
	dish := &tracked{Device: newDevice(t, "dish", "Dishwasher"), stopped: make(chan struct{})}
	svc := NewService([]alice.Device{oven}, &fakeTokens{}, nil, discard())
	mux := http.NewServeMux()
	svc.Register(mux)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	f := &fixture{url: hs.URL}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	waitFor(t, "запуск духовки", func() bool { return oven.runs.Load() == 1 })

	// Прежний прибор останавливается, новый запускается, тот же объект не перезапускается.
	svc.SetDevices([]alice.Device{dish})
	<-oven.stopped
	waitFor(t, "запуск посудомойки", func() bool { return dish.runs.Load() == 1 })
	svc.SetDevices([]alice.Device{dish})
	if n := dish.runs.Load(); n != 1 {
		t.Fatalf("посудомойку запускали %d раз", n)
	}
	_, out := f.call(t, http.MethodGet, "/v1.0/user/devices", "good", "")
	devices := out["payload"].(map[string]any)["devices"].([]any)
	if len(devices) != 1 || devices[0].(map[string]any)["id"] != "dish" {
		t.Fatalf("список после замены: %v", devices)
	}

	on := &alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: true}}
	if code := svc.Command(context.Background(), "oven", on); code != alice.CodeNotFound {
		t.Fatalf("команда убранному прибору: %q", code)
	}
	if code := svc.Command(context.Background(), "dish", on); code != "" {
		t.Fatalf("команда из интерфейса: %q", code)
	}
	cancel()
	<-done
	<-dish.stopped
}

func TestNotifierConfigure(t *testing.T) {
	var (
		mu  sync.Mutex
		got []captured
	)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, captured{r.URL.Path, r.Header.Get("Authorization"), nil})
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hs.Close()

	// Без навыка уведомления не ставятся в очередь; навык, заданный на ходу, действует сразу.
	n := NewNotifier(hs.URL, "", "", UserID, discard())
	svc := NewService(nil, &fakeTokens{}, n, discard())
	svc.Discovery()
	if len(n.queue) != 0 {
		t.Fatal("уведомление без навыка")
	}
	n.Configure("skill-2", "token-2")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	waitFor(t, "уведомление", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].path != "/api/v1/skills/skill-2/callback/discovery" || got[0].auth != "OAuth token-2" {
		t.Fatalf("уведомления: %+v", got)
	}
}

func TestMetrics(t *testing.T) {
	f := newFixture(t)
	value := func(name string, labels ...string) float64 { return metrics.Value(name, labels...) }
	done := value("hcgateway_yandex_actions_total", "device", "oven", "capability", "on_off", "result", "DONE")
	unknown := value("hcgateway_yandex_actions_total", "device", "unknown", "capability", "other", "result", alice.CodeNotFound)
	requests := value("hcgateway_yandex_requests_total", "handler", "action", "code", "200")
	denied := value("hcgateway_yandex_requests_total", "handler", "devices", "code", "401")

	f.call(t, http.MethodPost, "/v1.0/user/devices/action", "good", `{"payload":{"devices":[
		{"id":"oven","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}}]},
		{"id":"nope","capabilities":[{"type":"devices.capabilities.color_setting","state":{"instance":"rgb","value":1}}]}]}}`)
	f.call(t, http.MethodGet, "/v1.0/user/devices", "bad", "")

	// Чужие идентификаторы и умения не плодят новых меток.
	if value("hcgateway_yandex_actions_total", "device", "oven", "capability", "on_off", "result", "DONE") != done+1 ||
		value("hcgateway_yandex_actions_total", "device", "unknown", "capability", "other", "result", alice.CodeNotFound) != unknown+1 {
		t.Fatal("команды Алисы не посчитаны")
	}
	if value("hcgateway_yandex_requests_total", "handler", "action", "code", "200") != requests+1 ||
		value("hcgateway_yandex_requests_total", "handler", "devices", "code", "401") != denied+1 {
		t.Fatal("запросы Яндекса не посчитаны")
	}
	if value("hcgateway_yandex_actions_total", "device", "nope") != 0 {
		t.Fatal("в метках чужой идентификатор")
	}
}

// syncBuffer - журнал теста: в него пишет отправитель уведомлений, а читает тест.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Отказ Яндекса попадает в журнал с кодом, текстом ошибки и request_id: по ним видно, что
// именно Яндекс не принял.
func TestNotifierRejected(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"request_id":"req-1","status":"error","error_code":"BAD_REQUEST","error_message":"`+
			strings.Repeat("x", maxErrorMessage+10)+`"}`)
	}))
	defer hs.Close()
	var log syncBuffer
	n := NewNotifier(hs.URL, "skill-1", "token-1", UserID, slog.New(slog.NewTextHandler(&log, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	n.State([]alice.DeviceState{{ID: "oven"}})
	waitFor(t, "запись об отказе", func() bool { return strings.Contains(log.String(), "BAD_REQUEST") })
	line := log.String()
	if !strings.Contains(line, "request_id=req-1") || !strings.Contains(line, "error_message="+strings.Repeat("x", maxErrorMessage)+"…") {
		t.Fatalf("журнал: %s", line)
	}
}

// Состояние каждого изменившегося прибора уходит своим уведомлением: отказ Яндекса по одному
// прибору не отбрасывает изменения другого.
func TestNotificationsPerDevice(t *testing.T) {
	var (
		mu     sync.Mutex
		states [][]byte
	)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/callback/state") {
			body, _ := io.ReadAll(r.Body)
			states = append(states, body)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hs.Close()
	n := NewNotifier(hs.URL, "skill-1", "owner-token", UserID, discard())
	var (
		devices []alice.Device
		ms      []manual
	)
	for _, id := range []string{"oven", "dish"} {
		d := newDevice(t, id, "Oven")
		m := manual{Demo: d.Appliance.(*appliance.Demo), notify: make(chan appliance.Notify, 1)}
		d.Appliance = m
		devices, ms = append(devices, d), append(ms, m)
	}
	svc := NewService(devices, &fakeTokens{}, n, discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	for _, m := range ms {
		notify := <-m.notify
		notify(nil)
	}
	waitFor(t, "два уведомления о состоянии", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(states) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	for _, body := range states {
		if bytes.Contains(body, []byte(`"id":"oven"`)) && bytes.Contains(body, []byte(`"id":"dish"`)) {
			t.Fatalf("два прибора в одном уведомлении: %s", body)
		}
	}
}

// notifierServer - сервер уведомлений: записывает тела принятых состояний и отвечает кодом status.
func notifierServer(t *testing.T, status *atomic.Int32) (*Notifier, func() []string) {
	t.Helper()
	var (
		mu  sync.Mutex
		got []string
	)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v struct {
			Payload struct {
				Devices json.RawMessage `json:"devices"`
			} `json:"payload"`
		}
		json.Unmarshal(body, &v)
		mu.Lock()
		got = append(got, string(v.Payload.Devices))
		mu.Unlock()
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(hs.Close)
	n := NewNotifier(hs.URL, "skill-1", "token", UserID, discard())
	return n, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
}

// Уведомление о состоянии содержит только то, что изменилось; нажатие кнопки уходит только своим
// уведомлением и каждый раз; полное состояние - после запуска, возврата на связь и нового списка,
// после команды - все умения.
func TestNotifyChanges(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusAccepted)
	n, got := notifierServer(t, &status)
	ctx := context.Background()
	float := func(instance string, v float64) alice.CapState {
		return alice.CapState{Type: alice.PropFloat, State: alice.StateValue{Instance: instance, Value: v}}
	}
	button := alice.CapState{Type: alice.PropEvent, State: alice.StateValue{Instance: alice.InstanceButton, Value: "click"}}
	on := alice.CapState{Type: alice.CapOnOff, State: alice.StateValue{Instance: "on", Value: true}}
	state := func(temp, hum float64) notifyJob {
		return notifyJob{devices: []alice.DeviceState{{ID: "k", Capabilities: []alice.CapState{on},
			Properties: []alice.CapState{float("temperature", temp), float("humidity", hum), button}}}}
	}
	const (
		tempOnly = `[{"id":"k","properties":[{"type":"devices.properties.float","state":{"instance":"temperature","value":30}}]}]`
		full     = `[{"id":"k","status":"online","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}}],` +
			`"properties":[{"type":"devices.properties.float","state":{"instance":"temperature","value":30}},{"type":"devices.properties.float","state":{"instance":"humidity","value":40}}]}]`
	)
	expect := func(what string, want ...string) {
		t.Helper()
		have := got()
		if !slices.Equal(have, want) {
			t.Fatalf("%s: %q, нужно %q", what, have, want)
		}
	}

	first := `[{"id":"k","status":"online","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}}],` +
		`"properties":[{"type":"devices.properties.float","state":{"instance":"temperature","value":20}},{"type":"devices.properties.float","state":{"instance":"humidity","value":40}}]}]`
	n.send(ctx, state(20, 40))
	expect("после запуска - полное состояние без кнопки", first)
	n.send(ctx, state(20, 40))
	expect("без изменений уведомления нет")
	n.send(ctx, state(30, 40))
	expect("только изменившееся", tempOnly)

	press := notifyJob{press: true, devices: []alice.DeviceState{{ID: "k", Properties: []alice.CapState{button}}}}
	const pressed = `[{"id":"k","properties":[{"type":"devices.properties.event","state":{"instance":"button","value":"click"}}]}]`
	n.send(ctx, press)
	n.send(ctx, press)
	expect("каждое нажатие - отдельно", pressed, pressed)
	n.send(ctx, state(30, 40))
	expect("нажатие не меняет память о принятом")

	// Отказ: значение не считается принятым и уходит снова.
	status.Store(http.StatusBadRequest)
	n.send(ctx, state(20, 40))
	status.Store(http.StatusAccepted)
	got()
	n.send(ctx, state(30, 40))
	expect("старое значение Яндекс ещё помнит")
	n.send(ctx, state(20, 40))
	got()

	n.Forget("k")
	n.send(ctx, state(20, 40))
	expect("после команды - все умения, неизменные свойства не повторяются",
		`[{"id":"k","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}}]}]`)

	n.Reset()
	n.send(ctx, state(30, 40))
	expect("после нового списка - полное состояние", full)

	// Связь: offline один раз, при возврате - online и всё состояние.
	offline := notifyJob{devices: []alice.DeviceState{{ID: "k", ErrorCode: alice.CodeUnreachable}}}
	n.send(ctx, offline)
	n.send(ctx, offline)
	expect("без связи - один раз", `[{"id":"k","status":"offline"}]`)
	n.send(ctx, state(30, 40))
	expect("возврат на связь - полное состояние", full)

	// Новые настройки навыка: Яндекс мог ничего не знать.
	n.Configure("skill-2", "token")
	n.send(ctx, state(30, 40))
	expect("после смены навыка - полное состояние", full)
}

// Цвет: значение сравнивается с тем, что ушло последним, даже если instance был другим.
func TestNotifyColor(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusAccepted)
	n, got := notifierServer(t, &status)
	ctx := context.Background()
	color := func(instance string, v any) notifyJob {
		return notifyJob{devices: []alice.DeviceState{{ID: "l", Capabilities: []alice.CapState{{Type: alice.CapColor,
			State: alice.StateValue{Instance: instance, Value: v}}}}}}
	}
	n.send(ctx, color("temperature_k", 4000))
	n.send(ctx, color("rgb", 255))
	n.send(ctx, color("temperature_k", 4000))
	if sent := got(); len(sent) != 3 {
		t.Fatalf("уведомления о цвете: %q", sent)
	}
}

// Запрос состояния не отвечает нажатием кнопки: у неё нет состояния.
func TestQueryWithoutButton(t *testing.T) {
	d := newDevice(t, "oven", "Oven")
	m := manual{Demo: d.Appliance.(*appliance.Demo), notify: make(chan appliance.Notify, 1)}
	d.Appliance = m
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, func([]alice.DeviceState) {})
	notify := <-m.notify
	notify([]string{appliance.EventProgramFinished})
	svc := NewService([]alice.Device{d}, &fakeTokens{}, nil, discard())
	st := svc.stateOf("oven")
	if st.ErrorCode != "" || len(st.Properties) == 0 {
		t.Fatalf("состояние: %+v", st)
	}
	for _, c := range st.Properties {
		if c.State.(alice.StateValue).Instance == alice.InstanceButton {
			t.Fatalf("нажатие в ответе на запрос: %+v", st)
		}
	}
}

// Команда во время отправки: память о принятом совпадает с тем, что знает Яндекс, и возврат на связь
// не теряется. Повторный offline после команды не уходит.
func TestForgetWhileSending(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
		gate   chan struct{}
	)
	entered := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Payload struct {
				Devices json.RawMessage `json:"devices"`
			} `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		bodies = append(bodies, string(req.Payload.Devices))
		g := gate
		mu.Unlock()
		if g != nil {
			entered <- struct{}{}
			<-g
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	take := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := bodies
		bodies = nil
		return out
	}
	on := alice.CapState{Type: alice.CapOnOff, State: alice.StateValue{Instance: "on", Value: true}}
	online := func(level string) notifyJob {
		return notifyJob{devices: []alice.DeviceState{{ID: "k", Capabilities: []alice.CapState{on},
			Properties: []alice.CapState{{Type: alice.PropEvent, State: alice.StateValue{Instance: "water_level", Value: level}}}}}}
	}
	offline := notifyJob{devices: []alice.DeviceState{{ID: "k", ErrorCode: alice.CodeUnreachable}}}
	ctx := context.Background()
	start := func() *Notifier {
		n := NewNotifier(srv.URL, "s", "t", "u", discard())
		n.send(ctx, online("normal"))
		take()
		return n
	}
	during := func(n *Notifier, job notifyJob) {
		mu.Lock()
		g := make(chan struct{})
		gate = g
		mu.Unlock()
		done := make(chan struct{})
		go func() {
			n.send(ctx, job)
			close(done)
		}()
		<-entered
		n.Forget("k")
		mu.Lock()
		gate = nil
		mu.Unlock()
		close(g)
		<-done
		take()
	}
	const onlyCaps = `[{"id":"k","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}}]}]`
	const backOnline = `[{"id":"k","status":"online","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}}],` +
		`"properties":[{"type":"devices.properties.event","state":{"instance":"water_level","value":"normal"}}]}]`
	const levelNormal = `[{"id":"k","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}}],` +
		`"properties":[{"type":"devices.properties.event","state":{"instance":"water_level","value":"normal"}}]}]`

	n := start()
	during(n, offline)
	n.send(ctx, online("normal"))
	if got := take(); len(got) != 1 || got[0] != backOnline {
		t.Fatalf("возврат на связь после команды во время offline: %q", got)
	}

	n = start()
	during(n, online("low"))
	n.send(ctx, online("low"))
	if got := take(); len(got) != 1 || got[0] != onlyCaps {
		t.Fatalf("принятый уровень повторён: %q", got)
	}

	n = start()
	during(n, online("low"))
	n.send(ctx, online("normal"))
	if got := take(); len(got) != 1 || got[0] != levelNormal {
		t.Fatalf("уровень не вернулся к normal: %q", got)
	}

	n = start()
	n.send(ctx, offline)
	take()
	n.Forget("k")
	n.send(ctx, offline)
	if got := take(); len(got) != 0 {
		t.Fatalf("offline второй раз: %q", got)
	}
}
