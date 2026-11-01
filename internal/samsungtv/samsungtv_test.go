package samsungtv

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/appliance"
	"hcgateway/internal/tv"
)

// fakeTV - телевизор Samsung: REST с питанием, WebSocket пульта с токеном и UPnP громкости.
type fakeTV struct {
	rest, ws *httptest.Server

	mu      sync.Mutex
	power   string
	approve bool
	keys    []string
	volume  string
	mute    string
}

const fakeToken = "TOKEN-1"

func newFakeTV(t *testing.T) *fakeTV {
	f := &fakeTV{power: "on", approve: true, volume: "12", mute: "0"}
	desired := regexp.MustCompile(`<Desired(Volume|Mute)>([0-9]+)</`)
	f.rest = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v2/":
			json.NewEncoder(w).Encode(map[string]any{"device": map[string]string{"PowerState": f.power,
				"wifiMac": "aa:bb:cc:dd:ee:01", "modelName": "UE43TU7100", "TokenAuthSupport": "true"}})
		case r.URL.Path == "/dmr":
			io.WriteString(w, `<root><device><deviceList><device><serviceList><service>`+
				`<serviceType>`+renderingControl+`</serviceType><controlURL>/upnp/control/RenderingControl1</controlURL>`+
				`</service></serviceList></device></deviceList></device></root>`)
		case r.URL.Path == "/upnp/control/RenderingControl1":
			body, _ := io.ReadAll(r.Body)
			action := r.Header.Get("SOAPACTION")
			switch {
			case strings.HasSuffix(action, `#GetVolume"`):
				io.WriteString(w, "<Envelope><Body><GetVolumeResponse><CurrentVolume>"+f.volume+"</CurrentVolume></GetVolumeResponse></Body></Envelope>")
			case strings.HasSuffix(action, `#GetMute"`):
				io.WriteString(w, "<Envelope><Body><GetMuteResponse><CurrentMute>"+f.mute+"</CurrentMute></GetMuteResponse></Body></Envelope>")
			default:
				if m := desired.FindStringSubmatch(string(body)); m != nil && m[1] == "Volume" {
					f.volume = m[2]
				} else if m != nil {
					f.mute = m[2]
				}
				io.WriteString(w, "<Envelope/>")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	up := websocket.Upgrader{}
	f.ws = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		f.mu.Lock()
		approve := f.approve
		f.mu.Unlock()
		switch {
		case r.URL.Query().Get("token") == fakeToken:
			ws.WriteJSON(map[string]any{"event": "ms.channel.connect", "data": map[string]any{}})
		case approve:
			ws.WriteJSON(map[string]any{"event": "ms.channel.connect", "data": map[string]any{"token": fakeToken}})
		default:
			ws.WriteJSON(map[string]any{"event": "ms.channel.unauthorized"})
			return
		}
		for {
			var m struct {
				Params struct {
					Key string `json:"DataOfCmd"`
				} `json:"params"`
			}
			if ws.ReadJSON(&m) != nil {
				return
			}
			f.mu.Lock()
			f.keys = append(f.keys, m.Params.Key)
			if m.Params.Key == "KEY_POWER" {
				f.power = "standby"
			}
			f.mu.Unlock()
		}
	}))
	t.Cleanup(func() { f.rest.Close(); f.ws.Close() })
	return f
}

func (f *fakeTV) pressed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys)
}

func start(t *testing.T, f *fakeTV, dir string) (*TV, func()) {
	t.Helper()
	set, err := New(Config{ID: "tv", Host: "127.0.0.1", Dir: dir, Broadcast: "127.0.0.1"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	set.restURL = f.rest.URL + "/api/v2/"
	set.upnpURL = f.rest.URL + "/dmr"
	set.wsURLs = []string{"wss" + strings.TrimPrefix(f.ws.URL, "https") + "/api/v2/channels/samsung.remote.control"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		set.Run(ctx, func([]string) {})
	}()
	return set, func() { cancel(); <-done }
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

// Включённый телевизор спрашивает разрешение, шлюз сохраняет токен и MAC и видит громкость
// и звук; команды - кнопки пульта и UPnP.
func TestPairAndCommands(t *testing.T) {
	f := newFakeTV(t)
	dir := t.TempDir()
	set, stop := start(t, f, dir)
	defer stop()
	waitFor(t, "токен и громкость", func() bool {
		st := set.State()
		return st.Online && st.Values[tv.Volume] == 12.0 && st.Values[tv.Mute] == false
	})
	if s := set.Status(); !s.Paired || !s.UPnP || s.MAC != "AA:BB:CC:DD:EE:01" || s.Model != "UE43TU7100" {
		t.Fatalf("связь: %+v", s)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "tv.json"))
	var saved pairing
	if json.Unmarshal(data, &saved) != nil || saved.Token != fakeToken || !saved.Paired {
		t.Fatalf("файл телевизора: %s", data)
	}
	ctx := context.Background()
	for _, c := range []struct {
		key   string
		value any
	}{{tv.Volume, 30.0}, {tv.Mute, true}, {tv.Channel, 25.0}, {tv.Input, "HDMI2"}, {tv.Playback, tv.Paused}} {
		if err := set.Set(ctx, c.key, c.value); err != nil {
			t.Fatalf("%s=%v: %v", c.key, c.value, err)
		}
	}
	if err := set.Step(ctx, tv.Channel, -2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "кнопки", func() bool { return len(f.pressed()) == 7 })
	if want := []string{"KEY_2", "KEY_5", "KEY_ENTER", "KEY_HDMI2", "KEY_PAUSE", "KEY_CHDOWN", "KEY_CHDOWN"}; !slices.Equal(f.pressed(), want) {
		t.Fatalf("кнопки: %v", f.pressed())
	}
	if st := set.State(); st.Values[tv.Volume] != 30.0 || st.Values[tv.Mute] != true {
		t.Fatalf("громкость и звук: %v", st.Values)
	}
	if err := set.Set(ctx, tv.Input, "SCART"); err == nil || !strings.Contains(err.Error(), appliance.ErrInvalidValue.Error()) {
		t.Fatalf("неизвестный источник: %v", err)
	}
	if err := set.Set(ctx, tv.Power, tv.Off); err != nil {
		t.Fatal(err)
	}
	set.Retry()
	waitFor(t, "выключение", func() bool { st := set.State(); return st.Online && st.Values[tv.Power] == tv.Off })
	if err := set.Set(ctx, tv.Volume, 10.0); err == nil || !strings.Contains(err.Error(), appliance.ErrWrongMode.Error()) {
		t.Fatalf("команда выключенному: %v", err)
	}
	if err := set.Set(ctx, tv.Power, tv.On); err != nil {
		t.Fatalf("включение по сети: %v", err)
	}
}

// Отказ на экране виден на странице прибора, а без разрешения телевизор не на связи.
func TestPairRefused(t *testing.T) {
	f := newFakeTV(t)
	f.approve = false
	set, stop := start(t, f, t.TempDir())
	defer stop()
	waitFor(t, "отказ", func() bool { return set.Status().Err != "" })
	if set.State().Online {
		t.Fatal("без разрешения телевизор на связи")
	}
}

// Проверка адреса при поиске: модель и MAC из REST, без запроса разрешения.
func TestProbe(t *testing.T) {
	f := newFakeTV(t)
	in, err := probe(context.Background(), http.DefaultClient, f.rest.URL+"/api/v2/")
	if err != nil || in.Model != "UE43TU7100" || in.MAC != "AA:BB:CC:DD:EE:01" {
		t.Fatalf("телевизор: %+v %v", in, err)
	}
	if _, err := probe(context.Background(), http.DefaultClient, f.rest.URL+"/nothing/"); err == nil {
		t.Fatal("не телевизор принят за телевизор")
	}
}
