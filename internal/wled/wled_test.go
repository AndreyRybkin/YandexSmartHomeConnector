package wled

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/alice"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStrip - поддельная лента: при подключении присылает состояние и сведения о себе,
// на команду JSON API отвечает новым состоянием, на p - pong.
type fakeStrip struct {
	srv *httptest.Server

	mu       sync.Mutex
	state    map[string]any
	received []string
	conns    []*websocket.Conn
}

func newFake(t *testing.T) *fakeStrip {
	t.Helper()
	f := &fakeStrip{state: map[string]any{"on": false, "bri": 128.0, "ps": -1.0, "mainseg": 0.0,
		"seg": []any{map[string]any{"id": 0.0, "col": []any{[]any{255.0, 160.0, 0.0}, []any{0.0, 0.0, 0.0}}}}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", f.ws)
	mux.HandleFunc("/presets.json", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"0":{},"1":{"n":"Свечи","on":true,"bri":80},"3":{"n":"Вечеринка"},"x":{}}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStrip) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeStrip) fullLocked() []byte {
	data, _ := json.Marshal(map[string]any{"state": f.state, "info": map[string]any{"name": "Кухня", "arch": "esp32"}})
	return data
}

func (f *fakeStrip) ws(w http.ResponseWriter, r *http.Request) {
	c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	f.mu.Lock()
	f.conns = append(f.conns, c)
	msg := f.fullLocked()
	f.mu.Unlock()
	c.WriteMessage(websocket.TextMessage, msg)
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.received = append(f.received, string(data))
		if string(data) == "p" {
			f.mu.Unlock()
			c.WriteMessage(websocket.TextMessage, []byte("pong"))
			continue
		}
		var cmd map[string]any
		json.Unmarshal(data, &cmd)
		for k, v := range cmd {
			f.state[k] = v
		}
		msg := f.fullLocked()
		f.mu.Unlock()
		c.WriteMessage(websocket.TextMessage, msg)
	}
}

// kick рвёт все соединения, как лента, у которой кончились места для клиентов.
func (f *fakeStrip) kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
	f.conns = nil
}

func (f *fakeStrip) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.received) - 1; i >= 0; i-- {
		if f.received[i] != "p" {
			return f.received[i]
		}
	}
	return ""
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

func newDevice(f *fakeStrip) *Device {
	return &Device{ID: "strip", Name: "Лента", Room: "Кухня", Host: f.host(),
		Presets: map[int]string{1: "candle"}, Log: discard()}
}

func state(d *Device) map[string]any {
	out := map[string]any{}
	for _, st := range d.States() {
		if st.ErrorCode != "" {
			out["error"] = st.ErrorCode
		}
		for _, c := range st.Capabilities {
			sv := c.State.(alice.StateValue)
			out[sv.Instance] = sv.Value
		}
	}
	return out
}

func TestStrip(t *testing.T) {
	f := newFake(t)
	d := newDevice(f)
	if st := state(d); st["error"] != alice.CodeUnreachable {
		t.Fatalf("до подключения: %v", st)
	}
	changes := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		<-done
	}()
	go func() {
		defer close(done)
		d.Run(ctx, func([]alice.DeviceState) {
			select {
			case changes <- struct{}{}:
			default:
			}
		})
	}()
	eventually(t, "подключение", func() bool { online, _, _ := d.Strip(); return online })
	if _, name, arch := d.Strip(); name != "Кухня" || arch != "esp32" {
		t.Fatalf("сведения: %s %s", name, arch)
	}
	if st := state(d); st["on"] != false || st["brightness"] != 50 || st["rgb"] != 0xFFA000 {
		t.Fatalf("состояние: %v", st)
	}
	<-changes

	act := func(typ, instance string, value any, relative bool) string {
		return d.Act(context.Background(), "strip", &alice.Action{Type: typ,
			State: alice.ActionState{Instance: instance, Value: value, Relative: relative}})
	}
	// sent ждёт, пока лента получит команду: Act возвращается, как только команда ушла в соединение.
	sent := func(want string) {
		t.Helper()
		eventually(t, "команда "+want, func() bool { return f.last() == want })
	}
	if code := act(alice.CapOnOff, "on", true, false); code != "" {
		t.Fatalf("включение: %q", code)
	}
	sent(`{"on":true}`)
	if code := act(alice.CapRange, "brightness", 100.0, false); code != "" {
		t.Fatalf("яркость: %q", code)
	}
	sent(`{"bri":255,"on":true}`)
	// «Сделай ярче» на максимуме не выходит за 100 %.
	if code := act(alice.CapRange, "brightness", 20.0, true); code != "" || state(d)["brightness"] != 100 {
		t.Fatalf("ярче: %q %v", code, state(d))
	}
	if code := act(alice.CapRange, "brightness", 0.0, false); code != alice.CodeInvalidValue {
		t.Fatalf("яркость 0: %q", code)
	}
	if code := act(alice.CapColor, "rgb", float64(0xFF0000), false); code != "" || state(d)["rgb"] != 0xFF0000 {
		t.Fatalf("цвет: %q %v", code, state(d))
	}
	sent(`{"on":true,"seg":[{"col":[[255,0,0]],"fx":0}]}`)
	if code := act(alice.CapColor, "scene", "candle", false); code != "" || state(d)["scene"] != "candle" {
		t.Fatalf("сцена: %q %v", code, state(d))
	}
	sent(`{"ps":1}`)
	if code := act(alice.CapColor, "scene", "party", false); code != alice.CodeInvalidValue {
		t.Fatalf("сцена без пресета: %q", code)
	}
	if code := act(alice.CapColor, "temperature_k", 4500.0, false); code != alice.CodeInvalidAction {
		t.Fatalf("температура: %q", code)
	}
	if code := d.Act(context.Background(), "other", &alice.Action{Type: alice.CapOnOff}); code != alice.CodeInvalidAction {
		t.Fatalf("чужое устройство: %q", code)
	}
	info := d.Info()[0]
	color := info.Capabilities[2].Parameters.(alice.ColorParams)
	if info.Type != alice.TypeLight || color.ColorModel != "rgb" || len(color.ColorScene.Scenes) != 1 || color.ColorScene.Scenes[0].ID != "candle" {
		t.Fatalf("описание: %+v", info)
	}

	// Лента порвала соединение: устройство недоступно, затем шлюз переподключается.
	f.kick()
	eventually(t, "обрыв", func() bool { online, _, _ := d.Strip(); return !online })
	if st := state(d); st["error"] != alice.CodeUnreachable {
		t.Fatalf("после обрыва: %v", st)
	}
	if code := act(alice.CapOnOff, "on", false, false); code != alice.CodeUnreachable {
		t.Fatalf("команда без соединения: %q", code)
	}
	eventually(t, "переподключение", func() bool { online, _, _ := d.Strip(); return online })
}

func TestHeartbeat(t *testing.T) {
	pingEvery, readTimeout = 20*time.Millisecond, 200*time.Millisecond
	f := newFake(t)
	d := newDevice(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Пределы возвращаются, когда лента уже остановлена: иначе её горутины читали бы их во время записи.
	defer func() {
		cancel()
		<-done
		pingEvery, readTimeout = 30*time.Second, 70*time.Second
	}()
	go func() {
		defer close(done)
		d.Run(ctx, func([]alice.DeviceState) {})
	}()
	// Ответы pong продлевают соединение дольше readTimeout.
	eventually(t, "проверки соединения", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, r := range f.received {
			if r == "p" {
				n++
			}
		}
		return n >= 15
	})
	if online, _, _ := d.Strip(); !online {
		t.Fatal("соединение порвалось, хотя лента отвечала pong")
	}
}

func TestLoadPresets(t *testing.T) {
	f := newFake(t)
	presets, err := LoadPresets(context.Background(), f.host())
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 2 || presets[1] != "Свечи" || presets[3] != "Вечеринка" {
		t.Fatalf("пресеты: %v", presets)
	}
}

func TestFrameAndColor(t *testing.T) {
	d := &Device{ID: "strip", Log: discard()}
	d.conn = &websocket.Conn{}
	d.st.Arch = "esp8266"
	if err := d.send(context.Background(), map[string]any{"n": strings.Repeat("a", 600)}); err == nil || !strings.Contains(err.Error(), "кадр") {
		t.Fatalf("большая команда: %v", err)
	}
	for raw, want := range map[string]int{`[255,170,0]`: 0xFFAA00, `[1,2,3,4]`: 0x010203, `"00FF7F"`: 0x00FF7F} {
		if c, ok := parseColor(json.RawMessage(raw)); !ok || c != want {
			t.Errorf("%s: %x", raw, c)
		}
	}
	if _, ok := parseColor(json.RawMessage(`"r"`)); ok {
		t.Error("случайный цвет")
	}
	if percent(255) != 100 || percent(1) != 1 || level(100) != 255 || level(1) != 3 {
		t.Error("пересчёт яркости")
	}
}

// Проверка адреса при поиске: лента отвечает на /json/info, MAC - заглавными через двоеточие.
func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/info" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"brand": "WLED", "name": "Кухня", "arch": "esp32", "mac": "aabbccddee01"})
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	in, err := Probe(context.Background(), http.DefaultClient, host)
	if err != nil || in.Name != "Кухня" || in.MAC != "AA:BB:CC:DD:EE:01" || in.Arch != "esp32" {
		t.Fatalf("лента: %+v %v", in, err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"brand":"Other"}`)
	}))
	defer other.Close()
	if _, err := Probe(context.Background(), http.DefaultClient, strings.TrimPrefix(other.URL, "http://")); err == nil {
		t.Fatal("чужой прибор принят за ленту")
	}
	if colonMAC("zz") != "" || colonMAC("gggggggggggg") != "" {
		t.Fatal("неверный MAC разобран")
	}
}

// Значения ленты: те же instance, что у Алисы, и только пока лента на связи.
func TestValues(t *testing.T) {
	d := &Device{ID: "strip", Presets: map[int]string{3: "party"}}
	if _, ok := d.Values(); ok {
		t.Fatal("лента без связи сообщает значения")
	}
	d.st = status{On: true, Bri: 255, PS: 3, Color: 0xFF0000}
	d.have = true
	v, ok := d.Values()
	if !ok || v["on"] != true || v["brightness"] != 100.0 || v["rgb"] != "#FF0000" || v["scene"] != "party" {
		t.Fatalf("значения: %v", v)
	}
}

// infoMessage - сведения ленты: биты lc и ток.
func infoMessage(lc, pwr, maxpwr int) []byte {
	data, _ := json.Marshal(map[string]any{"info": map[string]any{"leds": map[string]any{"lc": lc, "pwr": pwr, "maxpwr": maxpwr}}})
	return data
}

func colorParams(t *testing.T, d *Device) alice.ColorParams {
	t.Helper()
	for _, c := range d.Info()[0].Capabilities {
		if p, ok := c.Parameters.(alice.ColorParams); ok {
			return p
		}
	}
	t.Fatal("нет умения цвета")
	return alice.ColorParams{}
}

// Лента без CCT и с выключенным ABL описывается как раньше и не просит новый список устройств.
func TestPlainStripUnchanged(t *testing.T) {
	calls := 0
	d := &Device{ID: "strip", Dir: t.TempDir(), Log: discard(), ListChanged: func() { calls++ }}
	before, _ := json.Marshal(d.Info())
	d.apply(infoMessage(1, 0, 0))
	d.apply([]byte(`{"state":{"on":true,"bri":128,"mainseg":0,"seg":[{"id":0,"cct":127,"col":[[0,0,0]]}]}}`))
	after, _ := json.Marshal(d.Info())
	if string(before) != string(after) || calls != 0 {
		t.Fatalf("описание %s, вызовов %d", after, calls)
	}
	if st := d.States()[0]; len(st.Properties) != 0 || st.Capabilities[2].State.(alice.StateValue).Instance != "rgb" {
		t.Fatalf("состояние: %+v", st)
	}
	if _, _, code := d.command(&alice.Action{Type: alice.CapColor, State: alice.ActionState{Instance: "temperature_k", Value: 4000.0}}); code != alice.CodeInvalidAction {
		t.Fatalf("команда: %s", code)
	}
	if entries, _ := os.ReadDir(d.Dir); len(entries) != 0 {
		t.Fatalf("файлы: %v", entries)
	}
}

// Температура: пределы только при бите 2, новое описание - один раз, значение 0-255 переводится в
// кельвины, а заданная команда не округляется.
func TestTemperature(t *testing.T) {
	calls := 0
	dir := t.TempDir()
	d := &Device{ID: "strip", Dir: dir, Log: discard(), ListChanged: func() { calls++ }}
	d.apply(infoMessage(7, 0, 0))
	d.apply(infoMessage(7, 0, 0))
	if calls != 1 {
		t.Fatalf("новых списков: %d", calls)
	}
	if lim := colorParams(t, d).TemperatureK; lim == nil || lim.Min != 2000 || lim.Max != 9000 {
		t.Fatalf("пределы: %+v", lim)
	}
	// Цвет - не белый: лента показывает rgb.
	d.apply([]byte(`{"state":{"on":true,"bri":128,"mainseg":0,"seg":[{"id":0,"cct":127,"col":[[255,0,0,0]]}]}}`))
	if v := state(d); v["rgb"] != 0xFF0000 {
		t.Fatalf("цвет: %v", v)
	}
	// Только белый канал: лента показывает температуру 1900 + 127 * 32.
	d.apply([]byte(`{"state":{"seg":[{"id":0,"cct":127,"col":[[0,0,0,255]]}]}}`))
	if v := state(d); v["temperature_k"] != 5964 {
		t.Fatalf("температура: %v", v)
	}
	// Края шкалы ленты не выходят за пределы, объявленные Алисе.
	d.apply([]byte(`{"state":{"seg":[{"id":0,"cct":0,"col":[[0,0,0,255]]}]}}`))
	if v := state(d); v["temperature_k"] != 2000 {
		t.Fatalf("тёплый край: %v", v)
	}
	d.apply([]byte(`{"state":{"seg":[{"id":0,"cct":255,"col":[[0,0,0,255]]}]}}`))
	if v := state(d); v["temperature_k"] != 9000 {
		t.Fatalf("холодный край: %v", v)
	}
	a := &alice.Action{Type: alice.CapColor, State: alice.ActionState{Instance: "temperature_k", Value: 4000.0}}
	cmd, update, code := d.command(a)
	if code != "" {
		t.Fatal(code)
	}
	data, _ := json.Marshal(cmd)
	if string(data) != `{"on":true,"seg":[{"cct":4000,"col":[[0,0,0,255]],"fx":0}]}` {
		t.Fatalf("команда: %s", data)
	}
	update(&d.st)
	// Лента подтверждает округлённым числом (4000 - 1900) >> 5: Алиса видит заданные 4000 K.
	if d.apply([]byte(`{"state":{"seg":[{"id":0,"cct":65,"col":[[0,0,0,255]]}]}}`)) {
		t.Fatal("подтверждение команды - изменение")
	}
	if v := state(d); v["temperature_k"] != 4000 {
		t.Fatalf("после команды: %v", v)
	}
	for _, bad := range []any{1999.0, 9001.0, 4000.5, "4000"} {
		a.State.Value = bad
		if _, _, code := d.command(a); code != alice.CodeInvalidValue {
			t.Fatalf("%v: %s", bad, code)
		}
	}
	// После перезапуска описание то же: новый объект ленты читает его из файла и список не просит.
	again := &Device{ID: "strip", Dir: dir, Log: discard(), ListChanged: func() { calls++ }}
	if colorParams(t, again).TemperatureK == nil {
		t.Fatal("возможности не прочитаны из файла")
	}
	again.apply(infoMessage(7, 0, 0))
	if calls != 1 {
		t.Fatalf("новых списков после перезапуска: %d", calls)
	}
	// Лента перестала сообщать CCT - настоящая причина для нового списка.
	again.apply(infoMessage(1, 0, 0))
	if calls != 2 || colorParams(t, again).TemperatureK != nil {
		t.Fatalf("списков %d, пределы %+v", calls, colorParams(t, again).TemperatureK)
	}
}

// Лента без белого канала показывает температуру белым цветом.
func TestTemperatureNoWhiteChannel(t *testing.T) {
	d := &Device{ID: "strip", Log: discard()}
	d.apply(infoMessage(5, 0, 0))
	cmd, update, code := d.command(&alice.Action{Type: alice.CapColor, State: alice.ActionState{Instance: "temperature_k", Value: 3000.0}})
	if code != "" {
		t.Fatal(code)
	}
	data, _ := json.Marshal(cmd)
	if !strings.Contains(string(data), `"col":[[255,255,255]]`) {
		t.Fatalf("команда: %s", data)
	}
	update(&d.st)
	d.apply([]byte(`{"state":{"seg":[{"id":0,"cct":34,"col":[[255,255,255]]}]}}`))
	if v := state(d); v["temperature_k"] != 3000 {
		t.Fatalf("состояние: %v", v)
	}
}

// Ток: свойство только при включённом ABL, 0 при выключенном не передаётся.
func TestAmperage(t *testing.T) {
	calls := 0
	d := &Device{ID: "strip", Dir: t.TempDir(), Log: discard(), ListChanged: func() { calls++ }}
	amps := func() any {
		for _, st := range d.States() {
			for _, p := range st.Properties {
				if sv := p.State.(alice.StateValue); sv.Instance == "amperage" {
					return sv.Value
				}
			}
		}
		return nil
	}
	d.apply([]byte(`{"state":{"on":true,"bri":128}}`))
	d.apply(infoMessage(1, 0, 0))
	if len(d.Info()[0].Properties) != 0 || amps() != nil || calls != 0 {
		t.Fatalf("ABL выключен: %+v, %v, %d", d.Info()[0].Properties, amps(), calls)
	}
	if !d.apply(infoMessage(1, 450, 850)) || calls != 1 {
		t.Fatalf("новых списков: %d", calls)
	}
	p := d.Info()[0].Properties
	if len(p) != 1 || p[0].Type != alice.PropFloat || p[0].Parameters != (alice.FloatParams{Instance: "amperage", Unit: "unit.ampere"}) {
		t.Fatalf("свойство: %+v", p)
	}
	if amps() != 0.45 {
		t.Fatalf("ток: %v", amps())
	}
	v, _ := d.Values()
	if v["amperage"] != 0.45 {
		t.Fatalf("значения: %v", v)
	}
	// При включённом ABL 0 - настоящий ток; значение вне документации не принимается.
	d.apply(infoMessage(1, 0, 850))
	if amps() != 0.0 {
		t.Fatalf("ток 0: %v", amps())
	}
	d.apply(infoMessage(1, -1, 850))
	if amps() != 0.0 {
		t.Fatalf("отрицательный ток: %v", amps())
	}
	d.apply(infoMessage(1, 0, 0))
	if len(d.Info()[0].Properties) != 0 || amps() != nil || calls != 2 {
		t.Fatalf("ABL выключили: %+v, %v, %d", d.Info()[0].Properties, amps(), calls)
	}
}
