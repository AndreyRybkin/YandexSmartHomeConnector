package webos

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/appliance"
	"hcgateway/internal/tv"
)

// fakeTV - телевизор LG по протоколу SSAP: регистрация с подтверждением, подписки и команды.
type fakeTV struct {
	srv *httptest.Server

	mu      sync.Mutex
	key     string
	approve bool
	conn    *websocket.Conn
	subs    map[string]string
	got     []string
	volume  int
	mute    bool
	app     string
	channel string
	play    string
	// screenOff - экран погашен: состояние питания Screen Off.
	screenOff bool
}

var fakeInputs = []map[string]string{{"id": "HDMI_1", "label": "HDMI 1", "appId": "com.webos.app.hdmi1"},
	{"id": "HDMI_2", "label": "HDMI 2", "appId": "com.webos.app.hdmi2"}}

var fakeChannels = map[string]string{"ch5": "5", "ch7": "7-1"}

func newFakeTV(t *testing.T) *fakeTV {
	f := &fakeTV{approve: true, volume: 10, app: liveTVApp, channel: "ch5", play: "playing"}
	up := websocket.Upgrader{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn, f.subs = ws, map[string]string{}
		f.mu.Unlock()
		f.serve(ws)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTV) host() string {
	return strings.TrimPrefix(f.srv.URL, "https://")
}

func (f *fakeTV) write(ws *websocket.Conn, typ, id string, payload any) {
	ws.WriteJSON(map[string]any{"type": typ, "id": id, "payload": payload})
}

// state - ответ на подписку uri; f.mu захвачен.
func (f *fakeTV) state(uri string) any {
	ok := map[string]any{"returnValue": true}
	add := func(k string, v any) map[string]any { ok[k] = v; return ok }
	switch uri {
	case uriPowerState:
		if f.screenOff {
			return add("state", "Screen Off")
		}
		return add("state", "Active")
	case uriForeground:
		return add("appId", f.app)
	case uriVolume:
		return add("volumeStatus", map[string]any{"volume": f.volume})
	case uriAudio:
		return add("mute", f.mute)
	case uriInputs:
		return add("devices", fakeInputs)
	case uriChannel:
		return add("channelNumber", fakeChannels[f.channel])
	case uriChannels:
		var list []map[string]string
		for id, n := range fakeChannels {
			list = append(list, map[string]string{"channelId": id, "channelNumber": n})
		}
		return add("channelList", list)
	case uriMedia:
		return add("foregroundAppInfo", []map[string]string{{"appId": f.app, "playState": f.play}})
	}
	return ok
}

// push сообщает подписке uri новое состояние; f.mu захвачен.
func (f *fakeTV) push(uri string) {
	if id, ok := f.subs[uri]; ok {
		f.write(f.conn, "response", id, f.state(uri))
	}
}

func (f *fakeTV) serve(ws *websocket.Conn) {
	defer ws.Close()
	for {
		var m struct {
			Type    string         `json:"type"`
			ID      string         `json:"id"`
			URI     string         `json:"uri"`
			Payload map[string]any `json:"payload"`
		}
		if ws.ReadJSON(&m) != nil {
			return
		}
		f.mu.Lock()
		switch m.Type {
		case "hello":
			f.write(ws, "hello", m.ID, map[string]any{"deviceUUID": "uuid-1"})
		case "register":
			key, _ := m.Payload["client-key"].(string)
			switch {
			case key != "" && key == f.key:
				f.write(ws, "registered", m.ID, map[string]any{"client-key": f.key})
			case !f.approve:
				f.write(ws, "response", m.ID, map[string]any{"pairingType": "PROMPT", "returnValue": true})
				f.write(ws, "error", m.ID, nil)
			default:
				f.write(ws, "response", m.ID, map[string]any{"pairingType": "PROMPT", "returnValue": true})
				f.key = "KEY-1"
				f.write(ws, "registered", m.ID, map[string]any{"client-key": f.key})
			}
		case "subscribe":
			f.subs[m.URI] = m.ID
			f.write(ws, "response", m.ID, f.state(m.URI))
		case "request":
			f.got = append(f.got, m.URI)
			f.request(ws, m.ID, m.URI, m.Payload)
			if m.URI == uriTurnOff {
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
	}
}

// request выполняет команду и сообщает подпискам новое состояние; f.mu захвачен.
func (f *fakeTV) request(ws *websocket.Conn, id, uri string, p map[string]any) {
	reply := map[string]any{"returnValue": true}
	switch uri {
	case uriSystemInfo:
		reply["modelName"] = "OLED55C1"
	case uriConnection:
		reply["wiredInfo"] = map[string]string{"macAddress": "aa:bb:cc:dd:ee:ff"}
	case uriSetVolume:
		f.volume = int(p["volume"].(float64))
		f.push(uriVolume)
	case uriVolumeUp:
		f.volume++
		f.push(uriVolume)
	case uriSetMute:
		f.mute = p["mute"].(bool)
		f.push(uriAudio)
	case uriSwitchInput:
		for _, in := range fakeInputs {
			if in["id"] == p["inputId"] {
				f.app = in["appId"]
			}
		}
		f.push(uriForeground)
	case uriLaunch:
		f.app = p["id"].(string)
		f.push(uriForeground)
	case uriOpenChannel:
		f.channel = p["channelId"].(string)
		f.push(uriChannel)
	case uriPause:
		f.play = "paused"
		f.push(uriMedia)
	case uriScreenOff, uriScreenOn:
		f.screenOff = uri == uriScreenOff
		f.push(uriPowerState)
	case uriTurnOff:
		return
	default:
		reply = map[string]any{"returnValue": false, "errorText": "unknown"}
	}
	f.write(ws, "response", id, reply)
}

func (f *fakeTV) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождался: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func run(t *testing.T, cfg Config) (*TV, func()) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tvset, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tvset.Run(ctx, func([]string) {})
	}()
	return tvset, func() { cancel(); <-done }
}

// Телевизор спрашивает разрешение, шлюз сохраняет ключ, MAC и входы и видит состояние.
func TestPairAndState(t *testing.T) {
	f := newFakeTV(t)
	dir := t.TempDir()
	changed := make(chan struct{}, 4)
	set, stop := run(t, Config{ID: "tv", Host: f.host(), Dir: dir, Changed: func() { changed <- struct{}{} }})
	defer stop()
	waitFor(t, "состояние телевизора", func() bool {
		st := set.State()
		return st.Online && st.Values[tv.Volume] == 10.0 && st.Values[tv.Channel] == 5.0 && set.Status().MAC != ""
	})
	<-changed
	st := set.State()
	if st.Values[tv.Power] != tv.On || st.Values[tv.Mute] != false || st.Values[tv.Input] != tv.LiveTV || st.Values[tv.Playback] != tv.Playing {
		t.Fatalf("состояние: %v", st.Values)
	}
	if s := set.Status(); !s.Paired || !s.Connected || s.Model != "OLED55C1" || s.MAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("связь: %+v", s)
	}
	if got := set.Describe().Options[tv.Input].Values; !slices.Equal(got, []string{tv.LiveTV, "HDMI_1", "HDMI_2"}) {
		t.Fatalf("входы: %v", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tv.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved pairing
	if json.Unmarshal(data, &saved) != nil || saved.ClientKey != "KEY-1" || saved.MAC != "AA:BB:CC:DD:EE:FF" || len(saved.Inputs) != 2 {
		t.Fatalf("файл телевизора: %s", data)
	}
}

// Команды доходят до телевизора, а его ответы меняют состояние; выключенный телевизор
// остаётся на связи выключенным.
func TestCommands(t *testing.T) {
	f := newFakeTV(t)
	set, stop := run(t, Config{ID: "tv", Host: f.host(), Dir: t.TempDir(), Broadcast: "127.0.0.1"})
	defer stop()
	waitFor(t, "подключение", func() bool { return set.Status().Connected && set.State().Values[tv.Channel] == 5.0 })
	ctx := context.Background()
	for _, c := range []struct {
		key   string
		value any
	}{{tv.Volume, 30.0}, {tv.Mute, true}, {tv.Input, "HDMI_2"}, {tv.Channel, 7.0}, {tv.Playback, tv.Paused}} {
		if err := set.Set(ctx, c.key, c.value); err != nil {
			t.Fatalf("%s=%v: %v", c.key, c.value, err)
		}
	}
	waitFor(t, "новое состояние", func() bool {
		v := set.State().Values
		return v[tv.Volume] == 30.0 && v[tv.Mute] == true && v[tv.Input] == "HDMI_2" && v[tv.Channel] == 7.0 && v[tv.Playback] == tv.Paused
	})
	if err := set.Step(ctx, tv.Volume, 2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "громкость шагом", func() bool { return set.State().Values[tv.Volume] == 32.0 })
	for _, bad := range []struct {
		key   string
		value any
	}{{tv.Channel, 9.0}, {tv.Input, "HDMI_9"}, {tv.Volume, 101.0}} {
		if err := set.Set(ctx, bad.key, bad.value); err == nil || !strings.Contains(err.Error(), appliance.ErrInvalidValue.Error()) {
			t.Fatalf("%s=%v: %v", bad.key, bad.value, err)
		}
	}
	if err := set.Set(ctx, tv.Power, tv.Off); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "выключение", func() bool { st := set.State(); return st.Online && st.Values[tv.Power] == tv.Off })
	if err := set.Set(ctx, tv.Volume, 5.0); err == nil || !strings.Contains(err.Error(), appliance.ErrWrongMode.Error()) {
		t.Fatalf("команда выключенному: %v", err)
	}
	if !slices.Contains(f.requests(), uriTurnOff) {
		t.Fatalf("запросы: %v", f.requests())
	}
	// Включение - пакет Wake-on-LAN по MAC, который сообщил телевизор, и новое подключение.
	if err := set.Set(ctx, tv.Power, tv.On); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "подключение после включения", func() bool { return set.State().Values[tv.Power] == tv.On })
}

// Отказ на экране телевизора виден на странице прибора, а без ключа телевизор не на связи.
func TestPairRefused(t *testing.T) {
	f := newFakeTV(t)
	f.approve = false
	set, stop := run(t, Config{ID: "tv", Host: f.host(), Dir: t.TempDir()})
	defer stop()
	waitFor(t, "отказ", func() bool { return set.Status().Err != "" })
	if st := set.State(); st.Online || set.Status().Paired {
		t.Fatalf("без ключа телевизор на связи: %+v", st)
	}
	if err := set.Set(context.Background(), tv.Power, tv.On); err == nil {
		t.Fatal("включение без MAC")
	}
}

// Проверка адреса при поиске: телевизор отвечает на hello и сообщает модель, а
// регистрации, после которой он спросил бы разрешение, нет.
func TestProbe(t *testing.T) {
	f := newFakeTV(t)
	model, err := Probe(context.Background(), f.host())
	if err != nil || model != "OLED55C1" {
		t.Fatalf("телевизор: %q %v", model, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.key != "" {
		t.Fatal("проверка зарегистрировалась на телевизоре")
	}
}

// Номер канала проверяется до поиска в списке телевизора: дробный, вне пределов и не число не проходят.
func TestChannelCommand(t *testing.T) {
	tvSet := &TV{numbers: map[int]string{5: "channel-5"}}
	uri, payload, err := tvSet.command(tv.Channel, 5.0)
	if err != nil || uri != uriOpenChannel || payload["channelId"] != "channel-5" {
		t.Fatalf("канал 5: %q %v %v", uri, payload, err)
	}
	for _, bad := range []any{5.5, 0.0, -1.0, 6.0, float64(tv.MaxChannel + 1), 1e300, "5"} {
		if _, _, err := tvSet.command(tv.Channel, bad); err == nil {
			t.Errorf("канал %v принят", bad)
		}
	}
}

// Отпечаток сертификата: запоминается при первом подключении, чужой останавливает связь до решения
// владельца, а запасной ws после этого не используется: ключ никуда не уходит.
func TestCertPin(t *testing.T) {
	f := newFakeTV(t)
	dir := t.TempDir()
	want := tv.Fingerprint(f.srv.Certificate().Raw)
	set, stop := run(t, Config{ID: "tv", Host: f.host(), Dir: dir})
	waitFor(t, "отпечаток", func() bool { return set.State().Online && set.Status().Cert == want })
	if set.Status().Plain {
		t.Fatal("wss считается соединением без шифрования")
	}
	stop()

	// Тот же файл, другой сертификат: как будто в сети появился чужой телевизор.
	data, err := os.ReadFile(filepath.Join(dir, "tv.json"))
	if err != nil {
		t.Fatal(err)
	}
	const old = "00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF"
	if !strings.Contains(string(data), want) {
		t.Fatalf("файл без отпечатка: %s", data)
	}
	if err := os.WriteFile(filepath.Join(dir, "tv.json"), []byte(strings.ReplaceAll(string(data), want, old)), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(f.requests())
	set, stop = run(t, Config{ID: "tv", Host: f.host(), Dir: dir})
	defer stop()
	waitFor(t, "новый отпечаток", func() bool { return set.Status().NewCert == want })
	if st := set.Status(); st.Connected || st.Cert != old {
		t.Fatalf("связь с чужим сертификатом: %+v", st)
	}
	if len(f.requests()) != before {
		t.Fatal("телевизор с чужим сертификатом получил запрос")
	}
	if err := set.AcceptCert(old); err == nil {
		t.Fatal("принят не тот отпечаток, что показан")
	}
	if err := set.AcceptCert(want); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "связь после решения", func() bool { st := set.Status(); return st.Connected && st.Cert == want && st.NewCert == "" })
	if err := set.Forget(); err != nil {
		t.Fatal(err)
	}
	if st := set.Status(); st.Cert != "" && st.Connected {
		t.Fatalf("после «Забыть» отпечаток остался: %+v", st)
	}
}

// С запомненным отпечатком шлюз не идёт на ws, а модель только с ws работает без отпечатка.
func TestCertPlain(t *testing.T) {
	up := websocket.Upgrader{}
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ws.Close()
	}))
	defer plain.Close()
	host := strings.TrimPrefix(plain.URL, "http://")

	free := &TV{cfg: Config{ID: "tv", Host: host, Dir: t.TempDir()}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ws, err := free.dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws.Close()
	if st := free.Status(); !st.Plain || st.Cert != "" {
		t.Fatalf("модель только с ws: %+v", st)
	}

	hits.Store(0)
	pinned := &TV{cfg: Config{ID: "tv", Host: host, Dir: t.TempDir()}, log: free.log}
	pinned.pair.Cert = "AA"
	if ws, err := pinned.dial(context.Background()); err == nil {
		ws.Close()
		t.Fatal("с отпечатком шлюз подключился без шифрования")
	}
	if hits.Load() != 0 {
		t.Fatal("с отпечатком шлюз обратился к порту без шифрования")
	}
}

// Экран гаснет и зажигается у включённого телевизора; у выключенного значения экрана нет.
func TestScreen(t *testing.T) {
	f := newFakeTV(t)
	set, stop := run(t, Config{ID: "tv", Host: f.host(), Dir: t.TempDir()})
	defer stop()
	waitFor(t, "экран", func() bool { return set.State().Values[tv.Screen] == tv.On })
	ctx := context.Background()
	if err := set.Set(ctx, tv.Screen, tv.Off); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "экран погас", func() bool { v := set.State().Values; return v[tv.Screen] == tv.Off && v[tv.Power] == tv.On })
	if err := set.Set(ctx, tv.Screen, tv.On); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "экран зажёгся", func() bool { return set.State().Values[tv.Screen] == tv.On })
	if err := set.Set(ctx, tv.Screen, "maybe"); !errors.Is(err, appliance.ErrInvalidValue) {
		t.Fatalf("неверное значение: %v", err)
	}
	if d := set.Describe(); !slices.Contains(d.Reports, tv.Screen) || !d.Options[tv.Screen].Setting {
		t.Fatalf("описание: %+v", d)
	}
	set.onPower([]byte(`{"state":"Suspend"}`))
	if _, ok := set.State().Values[tv.Screen]; ok {
		t.Fatalf("выключенный телевизор: %v", set.State().Values)
	}
}

// Приложения: режим есть только со списком, команда запускает только приложения из списка.
func TestApps(t *testing.T) {
	f := newFakeTV(t)
	apps := []string{"example.app.one", "example.app.two"}
	set, stop := run(t, Config{ID: "tv", Host: f.host(), Dir: t.TempDir(), Apps: apps})
	defer stop()
	waitFor(t, "подключение", func() bool { return set.Status().Connected && set.State().Values[tv.App] == liveTVApp })
	if o := set.Describe().Options[tv.App]; !slices.Equal(o.Values, apps) || !o.Setting {
		t.Fatalf("настройка: %+v", o)
	}
	ctx := context.Background()
	if err := set.Set(ctx, tv.App, "example.app.two"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "приложение", func() bool { return set.State().Values[tv.App] == "example.app.two" })
	if err := set.Set(ctx, tv.App, "example.other"); !errors.Is(err, appliance.ErrInvalidValue) {
		t.Fatalf("чужое приложение: %v", err)
	}
	none, stopNone := run(t, Config{ID: "tv2", Host: "127.0.0.1:1", Dir: t.TempDir()})
	defer stopNone()
	if _, ok := none.Describe().Options[tv.App]; ok {
		t.Fatal("настройка без списка приложений")
	}
}
