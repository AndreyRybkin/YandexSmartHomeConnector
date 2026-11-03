package smartthings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/profile"
	"hcgateway/internal/smartthings/stfake"
)

const redirect = "https://gw.example/connections/smartthings/callback"

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

// signIn входит в поддельное облако так, как владелец со страницы «Подключения».
func signIn(t *testing.T, c *Cloud) {
	t.Helper()
	u, err := c.BeginLogin(redirect)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(u)
	if q.Query().Get("client_id") != stfake.ClientID || q.Query().Get("redirect_uri") != redirect || q.Query().Get("scope") != Scopes {
		t.Fatalf("страница входа: %s", u)
	}
	ctx := context.Background()
	if err := c.FinishLogin(ctx, "чужой", stfake.Code); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("чужой state: %v", err)
	}
	state := q.Query().Get("state")
	if err := c.FinishLogin(ctx, state, stfake.Code); err != nil {
		t.Fatal(err)
	}
	if err := c.FinishLogin(ctx, state, stfake.Code); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("state второй раз: %v", err)
	}
}

func newCloud(t *testing.T, fake *stfake.Cloud) (*Cloud, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := New(filepath.Join(dir, "smartthings-tokens.json"), filepath.Join(dir, "smartthings"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.Configure(stfake.ClientID, stfake.ClientSecret, fake.URL())
	return c, dir
}

// Вход, список устройств, состояние по потоку событий и команды телевизору, лампе и камере
// холодильника.
func TestCloud(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	c, dir := newCloud(t, fake)
	signIn(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.RefreshAccount(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.Account()) != 3 {
		t.Fatalf("устройства: %+v", c.Account())
	}
	if _, err := os.Stat(filepath.Join(dir, "smartthings", "capabilities", "audioVolume-1.json")); err != nil {
		t.Fatalf("описание capability не на диске: %v", err)
	}
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()

	tv := c.Appliance(stfake.TV, "")
	freezer := c.Appliance(stfake.Fridge, "freezer")
	lamp := c.Appliance(stfake.Lamp, "main")
	for _, a := range []*Appliance{tv, freezer, lamp} {
		go a.Run(ctx, func([]string) {})
	}
	waitFor(t, "состояние", func() bool {
		return tv.State().Online && freezer.State().Online && lamp.State().Online
	})
	desc := tv.Describe()
	if desc.Type != "SmartThings.Television" || !slices.Equal(desc.Options["samsungvd.mediaInputSource.inputSource"].Values, []string{"dtv", "HDMI1"}) ||
		!slices.Contains(desc.Commands, "audioVolume.volumeUp") || desc.Options["audioVolume.volume"].Max != 100 {
		t.Fatalf("описание телевизора: %+v", desc)
	}
	st := tv.State().Values
	if st["switch.switch"] != "on" || st["audioVolume.volume"] != 15.0 || st["tvChannel.tvChannel"] != 5.0 || st["audioMute.mute"] != "unmuted" {
		t.Fatalf("состояние телевизора: %v", st)
	}
	if v := freezer.State().Values; v["temperatureMeasurement.temperature"] != -18.0 || v["contactSensor.contact"] != "closed" {
		t.Fatalf("камера холодильника: %v", v)
	}

	// Прибор по встроенному профилю: телевизор в Алисе.
	p, err := profile.Resolve(desc.Type, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := &hcdevice.Device{ID: "tv", Name: "Телевизор", Profile: p, Appliance: tv}
	raw, _ := json.Marshal(dev.Info()[0].Capabilities)
	for _, want := range []string{`"instance":"input_source","modes":[{"value":"one"},{"value":"five"}]`,
		`"instance":"volume","unit":"unit.percent"`, `"instance":"channel","random_access":true,"range":{"min":1,"max":999`,
		`"instance":"mute"`, `"instance":"pause"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("нет %s в %s", want, raw)
		}
	}
	act := func(typ, instance string, value any, relative bool) {
		t.Helper()
		if code := dev.Act(ctx, "tv", &alice.Action{Type: typ, State: alice.ActionState{Instance: instance, Value: value, Relative: relative}}); code != "" {
			t.Fatalf("%s %s: %s", typ, instance, code)
		}
	}
	act(alice.CapRange, "volume", 30.0, false)
	act(alice.CapRange, "channel", 1.0, true)
	act(alice.CapToggle, "mute", true, false)
	act(alice.CapMode, "input_source", "one", false)
	act(alice.CapToggle, "pause", true, false)
	act(alice.CapOnOff, "on", false, false)
	if err := freezer.Set(ctx, "thermostatCoolingSetpoint.coolingSetpoint", -20.0); err != nil {
		t.Fatal(err)
	}
	if err := lamp.Set(ctx, "colorControl.setColor", map[string]any{"hue": 25.0, "saturation": 50.0}); err != nil {
		t.Fatal(err)
	}
	if err := tv.Step(ctx, "audioVolume.volume", -2); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`tv-1 main audioVolume setVolume [30]`,
		`tv-1 main tvChannel setTvChannel ["6"]`,
		`tv-1 main audioMute mute []`,
		`tv-1 main samsungvd.mediaInputSource setInputSource ["HDMI1"]`,
		`tv-1 main mediaPlayback pause []`,
		`tv-1 main switch off []`,
		`fridge-1 freezer thermostatCoolingSetpoint setCoolingSetpoint [-20]`,
		`lamp-1 main colorControl setColor [{"hue":25,"saturation":50}]`,
		`tv-1 main audioVolume volumeDown []`,
		`tv-1 main audioVolume volumeDown []`,
	}
	if got := fake.Commands(); !slices.Equal(got, want) {
		t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
	}
	waitFor(t, "события", func() bool {
		v := tv.State().Values
		return v["switch.switch"] == "off" && v["samsungvd.mediaInputSource.inputSource"] == "HDMI1" && lamp.State().Values["colorControl.hue"] == 25.0
	})
	if err := tv.Set(ctx, "audioVolume.volume", 150.0); err == nil {
		t.Fatal("громкость вне пределов ушла в облако")
	}

	// goodbye: новая подписка, прежняя удаляется.
	fake.Goodbye()
	waitFor(t, "новая подписка", func() bool { created, _ := fake.Streams(); return created == 2 })
	waitFor(t, "поток после goodbye", func() bool { return c.Connected() })
	if c.API.Usage().Calls == 0 {
		t.Fatal("запросы не посчитаны")
	}
}

// Истёкший access token шлюз обновляет сам, а отвергнутый refresh token удаляет: нужен вход.
func TestTokenRefresh(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	c, dir := newCloud(t, fake)
	signIn(t, c)
	c.API.mu.Lock()
	c.API.tokens.Expiry = time.Now()
	c.API.mu.Unlock()
	ctx := context.Background()
	if err := c.RefreshAccount(ctx); err != nil {
		t.Fatalf("после обновления токена: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "smartthings-tokens.json"))
	if !strings.Contains(string(data), "refresh-1") {
		t.Fatalf("новый refresh token не сохранён: %s", data)
	}
	c.API.mu.Lock()
	c.API.tokens.RefreshToken, c.API.tokens.Expiry = "чужой", time.Now()
	c.API.mu.Unlock()
	if err := c.RefreshAccount(ctx); !errors.Is(err, ErrLoginRequired) || c.API.LoggedIn() {
		t.Fatalf("отвергнутый refresh token: %v", err)
	}
}

// Кнопка: каждое нажатие - событие, даже если значение то же. Другие атрибуты событий не дают.
func TestButtonEvent(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	c, _ := newCloud(t, fake)
	signIn(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.RefreshAccount(ctx); err != nil {
		t.Fatal(err)
	}
	a := c.Appliance(stfake.Lamp, "main")
	got := make(chan []string, 4)
	go a.Run(ctx, func(events []string) { got <- events })
	waitFor(t, "подписка", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.notify != nil
	})
	a.load(Status{})
	<-got
	for range 2 {
		a.apply("main", "button", "button", AttrValue{Value: json.RawMessage(`"pushed"`)})
		if ev := <-got; !slices.Equal(ev, []string{"button.button=pushed"}) {
			t.Fatalf("нажатие: %v", ev)
		}
	}
	a.apply("main", "switch", "switch", AttrValue{Value: json.RawMessage(`"on"`)})
	if ev := <-got; len(ev) != 0 {
		t.Fatalf("другой атрибут: %v", ev)
	}
}
