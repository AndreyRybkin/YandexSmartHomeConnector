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
	"hcgateway/internal/appliance"
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

// profiled - прибор поддельного устройства с состоянием как в снимке и встроенным профилем.
func profiled(t *testing.T, c *Cloud, id, component, status string) (*Appliance, *hcdevice.Device) {
	t.Helper()
	a := c.Appliance(id, component)
	loadJSON(t, a, status)
	desc := a.Describe()
	p, err := profile.Resolve(desc.Type, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a, &hcdevice.Device{ID: id, Name: id, Profile: p, Appliance: a}
}

func loadJSON(t *testing.T, a *Appliance, status string) {
	t.Helper()
	var st Status
	if err := json.Unmarshal([]byte(status), &st); err != nil {
		t.Fatal(err)
	}
	a.load(st)
}

// account - облако с устройствами, которые добавил тест: со списком и описаниями capability.
func account(t *testing.T, fake *stfake.Cloud) *Cloud {
	t.Helper()
	c, _ := newCloud(t, fake)
	signIn(t, c)
	if err := c.RefreshAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	drain(c.changes)
	return c
}

// online - прибор на связи для команд Алисы.
func online(c *Cloud, a *Appliance) {
	c.setConnected(true, "")
	c.setHealth(a.DeviceID(), true)
}

func infoJSON(d *hcdevice.Device) string {
	info := d.Info()
	if len(info) == 0 {
		return ""
	}
	raw, _ := json.Marshal(info[0])
	return string(raw)
}

func statesJSON(d *hcdevice.Device) string {
	raw, _ := json.Marshal(d.States())
	return string(raw)
}

func act(t *testing.T, d *hcdevice.Device, typ, instance string, value any) string {
	t.Helper()
	return d.Act(context.Background(), d.ID, &alice.Action{Type: typ, State: alice.ActionState{Instance: instance, Value: value}})
}

// Компонент с категорией Other получает категорию main, а у main с Other тип не теряется.
func TestCategory(t *testing.T) {
	d := Device{Components: []Component{{ID: "main"}, {ID: "freezer"}, {ID: "none"}}}
	d.Components[0].Categories = append(d.Components[0].Categories, struct {
		Name string `json:"name"`
		Type string `json:"categoryType"`
	}{"Refrigerator", "manufacturer"})
	d.Components[1].Categories = append(d.Components[1].Categories, struct {
		Name string `json:"name"`
		Type string `json:"categoryType"`
	}{"Other", "manufacturer"})
	if got := d.Type("freezer"); got != "SmartThings.Refrigerator" {
		t.Fatalf("freezer: %s", got)
	}
	if got := d.Category("none"); got != "Refrigerator" {
		t.Fatalf("без категории: %s", got)
	}
	if got := d.Category("cooler"); got != "Refrigerator" {
		t.Fatalf("нет компонента: %s", got)
	}
	d.Components[0].Categories[0].Name = "Others"
	if got := d.Category("main"); got != "Others" {
		t.Fatalf("main с Others: %s", got)
	}
	if got := (&Device{}).Category("main"); got != "Others" {
		t.Fatalf("без компонентов: %s", got)
	}
}

// Холодильник по образцу снимка: на main отключены уставка и температура, а камера с категорией Other
// - прибор со своими пределами в °F. Отключённый компонент не предлагается и ждёт, а lamp остаётся.
func TestDisabledFridge(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	comps := []map[string]any{
		stfake.Component("main", "Refrigerator", "contactSensor", "temperatureMeasurement", "thermostatCoolingSetpoint",
			"custom.disabledCapabilities", "custom.disabledComponents"),
		stfake.Component("freezer", "Other", "temperatureMeasurement", "thermostatCoolingSetpoint", "custom.disabledCapabilities"),
		stfake.Component("onedoor", "Other", "temperatureMeasurement"),
		stfake.Component("burner-06", "Other", "temperatureMeasurement"),
		stfake.Component("lamp", "Other", "switch"),
	}
	state := map[string]map[string]map[string]any{"main": {"custom.disabledComponents": {"disabledComponents": []string{"onedoor", "burner-6", "lamp"}}}}
	fake.AddDevice("ref-1", "Холодильник", comps, state)
	c := account(t, fake)
	var parts []string
	for _, d := range c.Account() {
		if d.ID == "ref-1" {
			parts = d.Parts()
		}
	}
	if !slices.Equal(parts, []string{"main", "freezer", "lamp"}) {
		t.Fatalf("компоненты: %v", parts)
	}
	const status = `{
		"main": {"custom.disabledCapabilities": {"disabledCapabilities": {"value": ["temperatureMeasurement", "thermostatCoolingSetpoint"]}},
			"custom.disabledComponents": {"disabledComponents": {"value": ["onedoor", "burner-6", "lamp"]}},
			"contactSensor": {"contact": {"value": "closed"}},
			"temperatureMeasurement": {"temperature": {"value": 37, "unit": "F"}},
			"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 37, "unit": "F"}, "coolingSetpointRange": {"value": null}}},
		"freezer": {"custom.disabledCapabilities": {"disabledCapabilities": {"value": []}},
			"temperatureMeasurement": {"temperature": {"value": 0, "unit": "F"}},
			"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 0, "unit": "F"},
				"coolingSetpointRange": {"value": {"minimum": -8, "maximum": 5, "step": 1}, "unit": "F"}}}}`
	main, mainDev := profiled(t, c, "ref-1", "main", status)
	if v := main.State().Values; v["thermostatCoolingSetpoint.coolingSetpoint"] != nil || v["temperatureMeasurement.temperature"] != nil || v["contactSensor.contact"] != "closed" {
		t.Fatalf("значения main: %v", v)
	}
	if info := infoJSON(mainDev); strings.Contains(info, `"temperature"`) || !strings.Contains(info, `"open"`) {
		t.Fatalf("main в Алисе: %s", info)
	}
	if err := main.Set(context.Background(), "thermostatCoolingSetpoint.coolingSetpoint", 3.0); !errors.Is(err, appliance.ErrNotSupported) {
		t.Fatalf("команда отключённой capability: %v", err)
	}
	if len(fake.Commands()) != 0 {
		t.Fatalf("команды: %v", fake.Commands())
	}
	freezer, freezerDev := profiled(t, c, "ref-1", "freezer", status)
	if freezer.Describe().Type != "SmartThings.Refrigerator" {
		t.Fatalf("тип камеры: %s", freezer.Describe().Type)
	}
	opt := freezer.Describe().Options["thermostatCoolingSetpoint.coolingSetpoint"]
	if opt.Min != -22.2 || opt.Max != -15 || opt.Step != 1 {
		t.Fatalf("пределы камеры: %+v", opt)
	}
	if info := infoJSON(freezerDev); !strings.Contains(info, `"instance":"temperature","unit":"unit.temperature.celsius","random_access":true,"range":{"min":-22.2,"max":-15,"precision":1}`) {
		t.Fatalf("камера в Алисе: %s", info)
	}
	if off := c.Appliance("ref-1", "onedoor").Describe(); off.Type != "" {
		t.Fatalf("отключённый компонент: %+v", off)
	}
	if off := c.Appliance("ref-1", "burner-06").Describe(); off.Type != "" {
		t.Fatalf("конфорка с другим написанием номера: %+v", off)
	}
	if lamp := c.Appliance("ref-1", "lamp").Describe(); lamp.Type == "" {
		t.Fatal("компонент lamp пропал")
	}

	// Событие меняет список: шлюз собирает приборы заново, повтор того же списка - нет.
	drain(c.changes)
	event := `{"eventType":"DEVICE_EVENT","deviceEvent":{"deviceId":"ref-1","componentId":"main","capability":"custom.disabledComponents",
		"attribute":"disabledComponents","value":["burner-6"]}}`
	c.handle("DEVICE_EVENT", event)
	if !gotSignal(c) {
		t.Fatal("новый список компонентов не пересобрал приборы")
	}
	c.handle("DEVICE_EVENT", event)
	if gotSignal(c) {
		t.Fatal("тот же список компонентов пересобрал приборы")
	}
	if c.Appliance("ref-1", "onedoor").Describe().Type == "" {
		t.Fatal("включённый компонент не вернулся")
	}
}

func gotSignal(c *Cloud) bool {
	select {
	case <-c.changes:
		return true
	default:
		return false
	}
}

// Кондиционер по образцу снимка: отключённые датчики пыли не видны, пределы устройства важнее
// запасных, режим fan вместо wind, а качание - режим по списку устройства.
func TestAirConditioner(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	comps := []map[string]any{stfake.Component("main", "AirConditioner", "switch", "airConditionerMode", "airConditionerFanMode",
		"thermostatCoolingSetpoint", "fanOscillationMode", "temperatureMeasurement", "dustSensor", "custom.thermostatSetpointControl", "custom.disabledCapabilities")}
	fake.AddDevice("ac-1", "Кондиционер", comps, map[string]map[string]map[string]any{})
	c := account(t, fake)
	const status = `{"main": {"switch": {"switch": {"value": "on"}},
		"custom.disabledCapabilities": {"disabledCapabilities": {"value": ["dustSensor"]}},
		"dustSensor": {"dustLevel": {"value": 3}, "fineDustLevel": {"value": null}},
		"airConditionerMode": {"airConditionerMode": {"value": "fan"}, "supportedAcModes": {"value": ["auto", "cool", "dry", "fan", "heat", "dryClean"]}},
		"airConditionerFanMode": {"fanMode": {"value": "high"}, "supportedAcFanModes": {"value": ["auto", "low", "high"]}},
		"fanOscillationMode": {"fanOscillationMode": {"value": "vertical"}, "supportedFanOscillationModes": {"value": ["fixed", "vertical", "horizontal", "all"]}},
		"temperatureMeasurement": {"temperature": {"value": 27, "unit": "C"}},
		"custom.thermostatSetpointControl": {"minimumSetpoint": {"value": -1000, "unit": "C"}, "maximumSetpoint": {"value": -1000, "unit": "C"}},
		"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 23, "unit": "C"}, "coolingSetpointRange": {"value": {"minimum": 18, "maximum": 28, "step": 1}, "unit": "C"}}}}`
	a, dev := profiled(t, c, "ac-1", "main", status)
	online(c, a)
	info := infoJSON(dev)
	for _, want := range []string{`"range":{"min":18,"max":28,"precision":1}`, `{"value":"fan_only"}`, `"instance":"swing"`,
		`{"value":"auto"},{"value":"stationary"},{"value":"horizontal"},{"value":"vertical"}`} {
		if !strings.Contains(info, want) {
			t.Fatalf("нет %s в %s", want, info)
		}
	}
	if strings.Contains(info, `"pm`) || strings.Contains(info, "oscillation") {
		t.Fatalf("лишнее: %s", info)
	}
	st := statesJSON(dev)
	for _, want := range []string{`"instance":"thermostat","value":"fan_only"`, `"instance":"swing","value":"vertical"`} {
		if !strings.Contains(st, want) {
			t.Fatalf("нет %s в %s", want, st)
		}
	}
	if code := act(t, dev, alice.CapMode, "thermostat", "fan_only"); code != "" {
		t.Fatal(code)
	}
	if code := act(t, dev, alice.CapMode, "swing", "auto"); code != "" {
		t.Fatal(code)
	}
	if code := act(t, dev, alice.CapRange, "temperature", 35.0); code != alice.CodeInvalidValue {
		t.Fatalf("вне пределов устройства: %q", code)
	}
	want := []string{`ac-1 main airConditionerMode setAirConditionerMode ["fan"]`, `ac-1 main fanOscillationMode setFanOscillationMode ["all"]`}
	if got := fake.Commands(); !slices.Equal(got, want) {
		t.Fatalf("команды: %v", got)
	}

	// Без диапазона - пределы custom.thermostatSetpointControl.
	loadJSON(t, a, `{"main": {"custom.thermostatSetpointControl": {"minimumSetpoint": {"value": 17, "unit": "C"}, "maximumSetpoint": {"value": 29, "unit": "C"}},
		"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 23, "unit": "C"}, "coolingSetpointRange": {"value": null}}}}`)
	if opt := a.Describe().Options["thermostatCoolingSetpoint.coolingSetpoint"]; opt.Min != 17 || opt.Max != 29 {
		t.Fatalf("пределы custom: %+v", opt)
	}
	// Предел -1000 - неизвестен: остаются запасные, и качания нет без списка устройства.
	loadJSON(t, a, `{"main": {"custom.thermostatSetpointControl": {"minimumSetpoint": {"value": -1000, "unit": "C"}, "maximumSetpoint": {"value": 30, "unit": "C"}},
		"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 23, "unit": "C"}},
		"fanOscillationMode": {"fanOscillationMode": {"value": "fixed"}, "supportedFanOscillationModes": {"value": null}}}}`)
	if opt := a.Describe().Options["thermostatCoolingSetpoint.coolingSetpoint"]; opt.Min != 16 || opt.Max != 30 {
		t.Fatalf("запасные пределы: %+v", opt)
	}
	if strings.Contains(infoJSON(dev), `"instance":"swing"`) {
		t.Fatal("качание без списка устройства")
	}
}

// Исключения из отключённых: стиральная машина со списком состояний не теряет on_off, а без него -
// теряет. Кнопка окончания работы есть только у машины, которая сообщает значение job.
func TestDisabledWasher(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	fake.AddDevice("wm-1", "Машина", []map[string]any{stfake.Component("main", "Washer", "washerOperatingState", "custom.disabledCapabilities")}, nil)
	fake.AddDevice("wm-2", "Другая", []map[string]any{stfake.Component("main", "Washer", "switch")}, nil)
	c := account(t, fake)
	const disabled = `"custom.disabledCapabilities": {"disabledCapabilities": {"value": ["washerOperatingState"]}}`
	_, dev := profiled(t, c, "wm-1", "main", `{"main": {`+disabled+`, "washerOperatingState": {"machineState": {"value": "stop"},
		"washerJobState": {"value": "none"}, "supportedMachineStates": {"value": ["stop", "run", "pause"]}}}}`)
	info := infoJSON(dev)
	for _, want := range []string{"devices.capabilities.on_off", `"instance":"button"`} {
		if !strings.Contains(info, want) {
			t.Fatalf("нет %s в %s", want, info)
		}
	}
	a := c.Appliance("wm-1", "main")
	loadJSON(t, a, `{"main": {`+disabled+`, "washerOperatingState": {"machineState": {"value": "stop"}, "supportedMachineStates": {"value": null}}}}`)
	if strings.Contains(infoJSON(dev), "on_off") {
		t.Fatalf("отключённая capability без списка: %s", infoJSON(dev))
	}
	_, other := profiled(t, c, "wm-2", "main", `{"main": {"switch": {"switch": {"value": "on"}}}}`)
	if info := infoJSON(other); strings.Contains(info, `"instance":"button"`) {
		t.Fatalf("кнопка без значения job: %s", info)
	}
}

// Новые списки и пределы шлюз передаёт только при настоящем изменении: иначе каждая загрузка
// состояния пересобирала бы приборы и уведомляла Яндекс.
func TestReshape(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	c := account(t, fake)
	tv := c.Appliance(stfake.TV, "")
	const status = `{"main": {"samsungvd.mediaInputSource": {"inputSource": {"value": "dtv"}, "supportedInputSourcesMap": {"value": [{"id": "dtv", "name": "TV"}]}}}}`
	loadJSON(t, tv, status)
	if !gotSignal(c) {
		t.Fatal("первый список не пересобрал приборы")
	}
	loadJSON(t, tv, status)
	if gotSignal(c) {
		t.Fatal("та же загрузка пересобрала приборы")
	}
	tv.apply("main", "samsungvd.mediaInputSource", "inputSource", AttrValue{Value: json.RawMessage(`"HDMI1"`)})
	if gotSignal(c) {
		t.Fatal("новое значение пересобрало приборы")
	}
	tv.apply("main", "samsungvd.mediaInputSource", "supportedInputSourcesMap", AttrValue{Value: json.RawMessage(`[{"id": "HDMI1"}]`)})
	if !gotSignal(c) {
		t.Fatal("новый список из события не пересобрал приборы")
	}
	tv.apply("main", "samsungvd.mediaInputSource", "supportedInputSourcesMap", AttrValue{Value: json.RawMessage(`[{"id": "HDMI1"}]`)})
	if gotSignal(c) {
		t.Fatal("тот же список из события пересобрал приборы")
	}
}

// Термостат по образцу снимка: уставка по режиму, «включи» - режим из списка устройства. Без пределов
// устройства температуры нет.
func TestThermostat(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	fake.AddDevice("th-1", "Термостат", []map[string]any{stfake.Component("main", "Thermostat", "thermostatMode", "thermostatHeatingSetpoint",
		"thermostatCoolingSetpoint", "temperatureMeasurement")}, nil)
	c := account(t, fake)
	const modes = `"thermostatMode": {"thermostatMode": {"value": "%s"}, "supportedThermostatModes": {"value": ["off", "cool", "auxheatonly", "auto"]}}`
	const rest = `"temperatureMeasurement": {"temperature": {"value": 71, "unit": "F"}},
		"thermostatHeatingSetpoint": {"heatingSetpoint": {"value": 68, "unit": "F"}, "heatingSetpointRange": {"value": {"minimum": 50, "maximum": 79, "step": 1}, "unit": "F"}},
		"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 77, "unit": "F"}, "coolingSetpointRange": {"value": {"minimum": 65, "maximum": 92, "step": 1}, "unit": "F"}}`
	body := func(mode string) string {
		return `{"main": {` + strings.Replace(modes, "%s", mode, 1) + `, ` + rest + `}}`
	}
	a, dev := profiled(t, c, "th-1", "main", body("cool"))
	online(c, a)
	if !strings.Contains(infoJSON(dev), `"range":{"min":10,"max":33.3,"precision":1}`) {
		t.Fatalf("пределы: %s", infoJSON(dev))
	}
	if !strings.Contains(statesJSON(dev), `"instance":"temperature","value":25`) {
		t.Fatalf("уставка охлаждения: %s", statesJSON(dev))
	}
	if code := act(t, dev, alice.CapRange, "temperature", 24.0); code != "" {
		t.Fatal(code)
	}
	// Состояние heat не из списка устройства в Алису не уходит, а уставка - нагрева.
	loadJSON(t, a, body("heat"))
	st := statesJSON(dev)
	if !strings.Contains(st, `"instance":"temperature","value":20`) || strings.Contains(st, `"instance":"thermostat"`) {
		t.Fatalf("режим heat: %s", st)
	}
	if code := act(t, dev, alice.CapRange, "temperature", 21.0); code != "" {
		t.Fatal(code)
	}
	// Вне пределов уставки нагрева, но в общих пределах.
	if code := act(t, dev, alice.CapRange, "temperature", 30.0); code != alice.CodeInvalidValue {
		t.Fatalf("вне пределов уставки: %q", code)
	}
	loadJSON(t, a, body("off"))
	if code := act(t, dev, alice.CapOnOff, "on", true); code != "" {
		t.Fatal(code)
	}
	want := []string{`th-1 main thermostatCoolingSetpoint setCoolingSetpoint [75]`, `th-1 main thermostatHeatingSetpoint setHeatingSetpoint [70]`,
		`th-1 main thermostatMode setThermostatMode ["auto"]`}
	if got := fake.Commands(); !slices.Equal(got, want) {
		t.Fatalf("команды: %v", got)
	}
	// Без пределов устройства действуют запасные.
	loadJSON(t, a, `{"main": {"thermostatMode": {"thermostatMode": {"value": "cool"}},
		"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 25, "unit": "C"}}}}`)
	if info := infoJSON(dev); !strings.Contains(info, `"range":{"min":5,"max":35,"precision":1}`) {
		t.Fatalf("уставка без пределов: %s", info)
	}
	if code := act(t, dev, alice.CapRange, "temperature", 40.0); code != alice.CodeInvalidValue {
		t.Fatalf("вне запасных пределов: %q", code)
	}
	// Режим heat есть у устройства: «включи» его и отправляет.
	loadJSON(t, a, `{"main": {"thermostatMode": {"thermostatMode": {"value": "off"}, "supportedThermostatModes": {"value": ["off", "heat"]}}}}`)
	if _, value, err := a.resolve(keyThermostat, "heat"); err != nil || value != "heat" {
		t.Fatalf("режим heat: %v %v", value, err)
	}
	loadJSON(t, a, `{"main": {"thermostatMode": {"thermostatMode": {"value": "off"}, "supportedThermostatModes": {"value": ["off"]}}}}`)
	if _, _, err := a.resolve(keyThermostat, "heat"); !errors.Is(err, appliance.ErrNotSupported) {
		t.Fatalf("только off: %v", err)
	}
}

// Духовка по образцу снимка: уставка без единицы берёт единицу температуры в камере, а значения
// простоя не передаются. Пределы - только от устройства.
func TestOven(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	fake.AddDevice("oven-1", "Плита", []map[string]any{stfake.Component("main", "Range", "ovenSetpoint", "temperatureMeasurement"),
		stfake.Component("cavity-01", "Other", "ovenSetpoint", "temperatureMeasurement")}, nil)
	c := account(t, fake)
	a, dev := profiled(t, c, "oven-1", "main", `{"main": {
		"ovenSetpoint": {"ovenSetpoint": {"value": 425}, "ovenSetpointRange": {"value": {"minimum": 150, "maximum": 550, "step": 5}, "unit": "F"}},
		"temperatureMeasurement": {"temperature": {"value": 425, "unit": "F"}}}}`)
	online(c, a)
	if v := a.State().Values[keyOven]; v != 218.3 {
		t.Fatalf("уставка: %v", v)
	}
	opt := a.Describe().Options[keyOven]
	if opt.Min != 65.6 || opt.Max != 287.8 || opt.Step != 3 {
		t.Fatalf("пределы: %+v", opt)
	}
	if code := act(t, dev, alice.CapRange, "temperature", 200.0); code != "" {
		t.Fatal(code)
	}
	if got := fake.Commands(); !slices.Equal(got, []string{`oven-1 main ovenSetpoint setOvenSetpoint [392]`}) {
		t.Fatalf("команды: %v", got)
	}
	for _, idle := range []string{"0", "1", "-17"} {
		loadJSON(t, a, `{"main": {"ovenSetpoint": {"ovenSetpoint": {"value": `+idle+`}}, "temperatureMeasurement": {"temperature": {"value": 30, "unit": "C"}}}}`)
		if _, ok := a.State().Values[keyOven]; ok {
			t.Fatalf("значение простоя %s передано", idle)
		}
	}
	// Единица из события: духовка в °F.
	loadJSON(t, a, `{"main": {"temperatureMeasurement": {"temperature": {"value": 100, "unit": "F"}}}}`)
	a.apply("main", "ovenSetpoint", "ovenSetpoint", AttrValue{Value: json.RawMessage(`212`)})
	if v := a.State().Values[keyOven]; v != 100.0 {
		t.Fatalf("уставка из события: %v", v)
	}
	// Камера: категория Other, тип Range, уставки нет в простое, а температура своя.
	cavity := c.Appliance("oven-1", "cavity-01")
	loadJSON(t, cavity, `{"cavity-01": {"ovenSetpoint": {"ovenSetpoint": {"value": 0}}, "temperatureMeasurement": {"temperature": {"value": 175, "unit": "F"}}}}`)
	if cavity.Describe().Type != "SmartThings.Range" || cavity.State().Values[keyTemperature] != 79.4 {
		t.Fatalf("камера: %s %v", cavity.Describe().Type, cavity.State().Values)
	}
	if info := infoJSON(&hcdevice.Device{ID: "c", Name: "c", Profile: dev.Profile, Appliance: cavity}); !strings.Contains(info, `"range":{"min":30,"max":300,"precision":5}`) {
		t.Fatalf("запасные пределы камеры: %s", info)
	}
}

// Энергия: Wh - в кВт·ч, kWh как есть, mWh и kVAh не передаются.
func TestEnergy(t *testing.T) {
	cases := []struct {
		unit  string
		value string
		want  any
	}{{"Wh", "12000", 12.0}, {"kWh", "1930.362", 1930.362}, {"", "5", 5.0}, {"mWh", "5", nil}, {"kVAh", "5", nil}}
	for _, tc := range cases {
		got, ok := scalar(AttrValue{Value: json.RawMessage(tc.value), Unit: tc.unit}, nil)
		if ok != (tc.want != nil) || got != tc.want {
			t.Fatalf("%s %s: %v %v", tc.value, tc.unit, got, ok)
		}
	}
}

// Кнопка по образцу снимков: pushed_2x и down_hold понятны, событие со stateChange false и значение из
// загрузки состояния нажатием не считаются.
func TestButtonPresses(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	fake.AddDevice("btn-1", "Кнопка", []map[string]any{stfake.Component("main", "Button", "button")}, nil)
	c := account(t, fake)
	a := c.Appliance("btn-1", "main")
	p, err := profile.Resolve("SmartThings.Button", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := &hcdevice.Device{ID: "btn-1", Name: "Кнопка", Profile: p, Appliance: a}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan []alice.DeviceState, 8)
	done := make(chan struct{})
	go func() { defer close(done); dev.Run(ctx, func(s []alice.DeviceState) { got <- s }) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "подписка", func() bool { return len(c.running("btn-1")) == 1 })
	c.setHealth("btn-1", true)
	<-got
	c.setConnected(true, "")
	<-got
	press := func() string {
		t.Helper()
		select {
		case s := <-got:
			raw, _ := json.Marshal(s)
			return string(raw)
		case <-time.After(5 * time.Second):
			t.Fatal("нет уведомления")
		}
		return ""
	}
	loadJSON(t, a, `{"main": {"button": {"button": {"value": "pushed"}, "supportedButtonValues": {"value": ["pushed", "held", "pushed_2x"]}}}}`)
	if s := press(); strings.Contains(s, "click") {
		t.Fatalf("первая загрузка: %s", s)
	}
	// Значение сменилось за время обрыва: нажатием это не считается.
	loadJSON(t, a, `{"main": {"button": {"button": {"value": "held"}}}}`)
	if s := press(); strings.Contains(s, "long_press") {
		t.Fatalf("значение из загрузки: %s", s)
	}
	handle := func(value string, stateChange bool) {
		data, _ := json.Marshal(map[string]any{"eventType": "DEVICE_EVENT", "deviceEvent": map[string]any{"deviceId": "btn-1", "componentId": "main",
			"capability": "button", "attribute": "button", "value": value, "stateChange": stateChange}})
		c.handle("DEVICE_EVENT", string(data))
	}
	handle("pushed", false)
	if s := press(); strings.Contains(s, "click") {
		t.Fatalf("stateChange false: %s", s)
	}
	handle("pushed_2x", true)
	if s := press(); !strings.Contains(s, `"double_click"`) {
		t.Fatalf("pushed_2x: %s", s)
	}
	handle("down_hold", true)
	if s := press(); !strings.Contains(s, `"long_press"`) {
		t.Fatalf("down_hold: %s", s)
	}
	if _, ok := a.State().Values[buttonKey]; ok {
		t.Fatal("значение кнопки в состоянии")
	}
}

// Очиститель по образцу снимка сообщает скорость в airConditionerFanMode.
func TestPurifier(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	fake.AddDevice("ap-1", "Очиститель", []map[string]any{stfake.Component("main", "AirPurifier", "switch", "airConditionerFanMode")}, nil)
	c := account(t, fake)
	a, dev := profiled(t, c, "ap-1", "main", `{"main": {"switch": {"switch": {"value": "on"}},
		"airConditionerFanMode": {"fanMode": {"value": "sleep"}, "supportedAcFanModes": {"value": ["auto", "low", "medium", "high", "sleep"]}}}}`)
	online(c, a)
	if !strings.Contains(infoJSON(dev), `"instance":"fan_speed"`) || !strings.Contains(statesJSON(dev), `"instance":"fan_speed","value":"quiet"`) {
		t.Fatalf("скорость: %s %s", infoJSON(dev), statesJSON(dev))
	}
	if code := act(t, dev, alice.CapMode, "fan_speed", "low"); code != "" {
		t.Fatal(code)
	}
	if got := fake.Commands(); !slices.Equal(got, []string{`ap-1 main airConditionerFanMode setFanMode ["low"]`}) {
		t.Fatalf("команды: %v", got)
	}
}

// Духовка без своих пределов получает запасные, а холодильник остаётся без умения температуры.
func TestOvenFallbackLimits(t *testing.T) {
	fake := stfake.New()
	defer fake.Close()
	fake.AddDevice("oven-2", "Духовка", []map[string]any{stfake.Component("main", "Oven", "ovenSetpoint", "temperatureMeasurement")}, nil)
	fake.AddDevice("fridge-2", "Холодильник", []map[string]any{stfake.Component("main", "Refrigerator", "thermostatCoolingSetpoint")}, nil)
	c := account(t, fake)
	a, dev := profiled(t, c, "oven-2", "main", `{"main": {"ovenSetpoint": {"ovenSetpoint": {"value": 180}},
		"temperatureMeasurement": {"temperature": {"value": 180, "unit": "C"}}}}`)
	online(c, a)
	if info := infoJSON(dev); !strings.Contains(info, `"range":{"min":30,"max":300,"precision":5}`) {
		t.Fatalf("духовка: %s", info)
	}
	_, fridge := profiled(t, c, "fridge-2", "main", `{"main": {"thermostatCoolingSetpoint": {"coolingSetpoint": {"value": 3, "unit": "C"}}}}`)
	if info := infoJSON(fridge); strings.Contains(info, "devices.capabilities.range") {
		t.Fatalf("холодильник: %s", info)
	}
}
