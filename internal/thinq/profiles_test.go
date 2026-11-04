package thinq

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hcgateway/internal/alice"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/profile"
)

// Раскладки ниже повторяют настоящие профили LG по фикстурам Home Assistant и библиотеке LG.
// Значения и коды - заглушки.

// lgDevice - прибор LG типа typ со встроенным профилем, заданным профилем ThinQ и состоянием.
type lgDevice struct {
	*hcdevice.Device
	t   *testing.T
	api *fakeAPI
	a   *Appliance
}

func newLG(t *testing.T, typ, prof, state, location string) *lgDevice {
	t.Helper()
	ca := newCA(t)
	api := newAPI(t, ca)
	dir := t.TempDir()
	c, err := New(filepath.Join(dir, "thinq-client.json"), filepath.Join(dir, "thinq"), discard())
	if err != nil {
		t.Fatal(err)
	}
	c.API.Configure("test-pat", "NL", api.URL)
	m, err := parseProfile(typ, []byte(prof), location)
	if err != nil {
		t.Fatal(err)
	}
	a := &Appliance{cloud: c, deviceID: "dev", location: location}
	a.setModel(m)
	a.apply(json.RawMessage(state), true)
	c.setConnected(true)
	t.Cleanup(func() { c.setConnected(false) })
	p, err := profile.Resolve(typ, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &lgDevice{Device: &hcdevice.Device{ID: "x", Name: "Прибор", Profile: p, Appliance: a, Manufacturer: "LG"}, t: t, api: api, a: a}
}

// shortType - тип без префикса: devices.capabilities.on_off -> on_off.
func shortType(t string) string {
	return t[strings.LastIndex(t, ".")+1:]
}

// items - умения и свойства устройства Алисы вида «тип/instance».
func (d *lgDevice) items() []string {
	d.t.Helper()
	info := d.Info()
	if len(info) == 0 {
		return nil
	}
	data, err := json.Marshal(info[0])
	if err != nil {
		d.t.Fatal(err)
	}
	var dev struct {
		Capabilities, Properties []struct {
			Type       string `json:"type"`
			Parameters struct {
				Instance string `json:"instance"`
			} `json:"parameters"`
		}
	}
	if err := json.Unmarshal(data, &dev); err != nil {
		d.t.Fatal(err)
	}
	var out []string
	for _, c := range append(dev.Capabilities, dev.Properties...) {
		out = append(out, strings.TrimSuffix(shortType(c.Type)+"/"+c.Parameters.Instance, "/"))
	}
	slices.Sort(out)
	return out
}

// params - параметры умения или свойства «тип/instance» в JSON.
func (d *lgDevice) params(item string) string {
	d.t.Helper()
	info := d.Info()[0]
	for _, c := range append(slices.Clone(info.Capabilities), info.Properties...) {
		data, _ := json.Marshal(c.Parameters)
		var p struct {
			Instance string `json:"instance"`
		}
		json.Unmarshal(data, &p)
		if shortType(c.Type)+"/"+p.Instance == item {
			return string(data)
		}
	}
	d.t.Fatalf("нет %s", item)
	return ""
}

// state - значения устройства Алисы вида «тип/instance».
func (d *lgDevice) state() map[string]any {
	out := map[string]any{}
	st := d.States()[0]
	for _, c := range append(slices.Clone(st.Capabilities), st.Properties...) {
		sv := c.State.(alice.StateValue)
		out[shortType(c.Type)+"/"+sv.Instance] = sv.Value
	}
	return out
}

// act выполняет команду Алисы и возвращает её код и тело ушедшей команды.
func (d *lgDevice) act(typ, instance string, v any) (string, string) {
	d.t.Helper()
	d.api.mu.Lock()
	before := len(d.api.controls)
	d.api.mu.Unlock()
	code := d.Act(context.Background(), "x", &alice.Action{Type: typ, State: alice.ActionState{Instance: instance, Value: v}})
	d.api.mu.Lock()
	defer d.api.mu.Unlock()
	if len(d.api.controls) == before {
		return code, ""
	}
	return code, strings.TrimPrefix(d.api.controls[len(d.api.controls)-1], "dev ")
}

const (
	powerProp  = `{"type":"enum","mode":["r","w"],"value":{"r":["POWER_ON","POWER_OFF"],"w":["POWER_ON","POWER_OFF"]}}`
	numberRead = `{"type":"number","mode":["r"]}`

	// Кондиционер: записи C и F, запись по режиму, ресурс temperature и датчики.
	acFullProfile = `{"notification":{"push":["WATER_IS_FULL"]},"property":{` +
		`"airConJobMode":{"currentJobMode":{"type":"enum","mode":["r","w"],"value":{"r":["COOL","HEAT","AIR_DRY"],"w":["COOL","HEAT","AIR_DRY"]}}},` +
		`"operation":{"airConOperationMode":` + powerProp + `},` +
		`"temperature":{"currentTemperature":` + numberRead + `,` +
		`"targetTemperature":{"type":"range","mode":["r","w"],"value":{"r":{"min":18,"max":30,"step":1},"w":{"min":18,"max":30,"step":1}}},` +
		`"unit":{"type":"enum","mode":["r"],"value":{"r":["C","F"]}}},` +
		`"temperatureInUnits":[` +
		`{"unit":"C","currentTemperature":` + numberRead + `,"targetTemperature":` + numberRead + `,` +
		`"coolTargetTemperature":{"type":"range","mode":["w"],"value":{"w":{"min":18,"max":30,"step":1}}},` +
		`"heatTargetTemperature":{"type":"range","mode":["w"],"value":{"w":{"min":16,"max":30,"step":1}}}},` +
		`{"unit":"F","currentTemperature":` + numberRead + `,"targetTemperature":` + numberRead + `,` +
		`"coolTargetTemperature":{"type":"range","mode":["w"],"value":{"w":{"min":64,"max":86,"step":2}}},` +
		`"heatTargetTemperature":{"type":"range","mode":["w"],"value":{"w":{"min":60,"max":86,"step":2}}}}],` +
		`"airQualitySensor":{"humidity":` + numberRead + `},` +
		`"windDirection":{"rotateUpDown":{"type":"boolean","mode":["r","w"],"value":{"r":[true,false],"w":[true,false]}}}}}`
	acFullState = `{"airConJobMode":{"currentJobMode":"COOL"},"operation":{"airConOperationMode":"POWER_ON"},` +
		`"temperature":{"currentTemperature":25,"targetTemperature":19,"unit":"C"},` +
		`"temperatureInUnits":[{"unit":"C","currentTemperature":25,"targetTemperature":19},{"unit":"F","currentTemperature":77,"targetTemperature":66}],` +
		`"airQualitySensor":{"humidity":40},"windDirection":{"rotateUpDown":false}}`
)

func TestACTemperature(t *testing.T) {
	d := newLG(t, "DEVICE_AIR_CONDITIONER", acFullProfile, acFullState, "")
	st := d.state()
	if st["range/temperature"] != 19.0 || st["float/temperature"] != 25.0 {
		t.Fatalf("значения в градусах Цельсия: %v", st)
	}
	if got := d.params("range/temperature"); !strings.Contains(got, `"min":16`) || !strings.Contains(got, `"max":30`) {
		t.Fatalf("пределы из записи C: %s", got)
	}
	if code, body := d.act(alice.CapRange, "temperature", 22.0); code != "" ||
		body != `{"temperatureInUnits":{"coolTargetTemperature":22,"unit":"C"}}` {
		t.Fatalf("охлаждение: %q %s", code, body)
	}
	// 17 есть только у нагрева: в охлаждении команда не уходит.
	if code, body := d.act(alice.CapRange, "temperature", 17.0); code != alice.CodeInvalidValue || body != "" {
		t.Fatalf("вне пределов режима: %q %s", code, body)
	}
	d.a.apply(json.RawMessage(`{"airConJobMode":{"currentJobMode":"HEAT"}}`), false)
	if code, body := d.act(alice.CapRange, "temperature", 17.0); code != "" ||
		body != `{"temperatureInUnits":{"heatTargetTemperature":17,"unit":"C"}}` {
		t.Fatalf("нагрев: %q %s", code, body)
	}
	// Режим без своей записи: запасная запись ресурса temperature с единицей.
	d.a.apply(json.RawMessage(`{"airConJobMode":{"currentJobMode":"AIR_DRY"}}`), false)
	if code, body := d.act(alice.CapRange, "temperature", 20.0); code != "" ||
		body != `{"temperature":{"targetTemperature":20,"unit":"C"}}` {
		t.Fatalf("запасная запись: %q %s", code, body)
	}
}

// Отчёт в единицах прибора без записи C не меняет значения шлюза.
func TestFahrenheitIgnored(t *testing.T) {
	d := newLG(t, "DEVICE_AIR_CONDITIONER", acFullProfile, acFullState, "")
	d.a.apply(json.RawMessage(`{"temperature":{"unit":"F","currentTemperature":80},"temperatureInUnits":[{"unit":"F","currentTemperature":80}]}`), false)
	if v := d.a.State().Values; v["temperatureInUnits.currentTemperatureC"] != 25.0 || v["temperature.currentTemperature"] != 25.0 {
		t.Fatalf("значения: %v", v)
	}
	// Единица не повторяется в следующем отчёте, а значение всё ещё в градусах Фаренгейта.
	d.a.apply(json.RawMessage(`{"temperature":{"currentTemperature":81}}`), false)
	if v := d.a.State().Values; v["temperature.currentTemperature"] != 25.0 {
		t.Fatalf("отчёт без единицы: %v", v)
	}
}

// Режим без записи и без запасного ресурса: команда не уходит.
func TestACNoWriteInMode(t *testing.T) {
	prof := `{"property":{"airConJobMode":{"currentJobMode":{"type":"enum","mode":["r","w"],"value":{"r":["COOL","FAN"],"w":["COOL","FAN"]}}},` +
		`"temperatureInUnits":[{"unit":"C","targetTemperature":` + numberRead + `,` +
		`"coolTargetTemperature":{"type":"range","mode":["w"],"value":{"w":{"min":18,"max":30,"step":1}}}}]}}`
	d := newLG(t, "DEVICE_AIR_CONDITIONER", prof, `{"airConJobMode":{"currentJobMode":"FAN"}}`, "")
	if code, body := d.act(alice.CapRange, "temperature", 22.0); code != alice.CodeWrongMode || body != "" {
		t.Fatalf("режим без записи: %q %s", code, body)
	}
}

// Прибор без записей по единицам: те же значения в ресурсе temperature.
func TestACTemperatureResource(t *testing.T) {
	prof := `{"property":{"temperature":{"currentTemperature":` + numberRead + `,` +
		`"targetTemperature":{"type":"range","mode":["r","w"],"value":{"w":{"min":18,"max":30,"step":1}}},` +
		`"unit":{"type":"enum","mode":["r"],"value":{"r":["C","F"]}}}}}`
	d := newLG(t, "DEVICE_AIR_CONDITIONER", prof, `{"temperature":{"currentTemperature":25,"targetTemperature":19,"unit":"C"}}`, "")
	if st := d.state(); st["range/temperature"] != 19.0 || st["float/temperature"] != 25.0 {
		t.Fatalf("значения: %v", st)
	}
	if code, body := d.act(alice.CapRange, "temperature", 22.0); code != "" || body != `{"temperature":{"targetTemperature":22,"unit":"C"}}` {
		t.Fatalf("команда: %q %s", code, body)
	}
}

func TestWaterHeaterTemperature(t *testing.T) {
	prof := `{"property":{"operation":{"waterHeaterOperationMode":{"type":"enum","mode":["r"],"value":{"r":["POWER_ON","POWER_OFF"]}}},` +
		`"temperatureInUnits":[{"unit":"C","currentTemperature":` + numberRead + `,` +
		`"targetTemperature":{"type":"range","mode":["r","w"],"value":{"r":{"min":30,"max":70,"step":1},"w":{"min":30,"max":70,"step":1}}}},` +
		`{"unit":"F","currentTemperature":` + numberRead + `,` +
		`"targetTemperature":{"type":"range","mode":["r","w"],"value":{"w":{"min":86,"max":158,"step":1}}}}]}}`
	state := `{"operation":{"waterHeaterOperationMode":"POWER_ON"},"temperatureInUnits":[{"unit":"C","currentTemperature":40,"targetTemperature":50},{"unit":"F","currentTemperature":104,"targetTemperature":122}]}`
	d := newLG(t, "DEVICE_WATER_HEATER", prof, state, "")
	if st := d.state(); st["range/temperature"] != 50.0 || st["float/temperature"] != 40.0 {
		t.Fatalf("значения: %v", st)
	}
	if code, body := d.act(alice.CapRange, "temperature", 55.0); code != "" || body != `{"temperatureInUnits":{"targetTemperature":55,"unit":"C"}}` {
		t.Fatalf("команда: %q %s", code, body)
	}
	// Питание только для чтения: «выключи» не обещается (ошибка 29).
	if slices.Contains(d.items(), "on_off") {
		t.Fatalf("on_off у питания только для чтения: %v", d.items())
	}
}

func TestBoilerTemperature(t *testing.T) {
	rng := func(lo, hi string) string {
		return `{"type":"range","mode":["w"],"value":{"w":{"min":` + lo + `,"max":` + hi + `,"step":1}}}`
	}
	prof := `{"property":{"boilerJobMode":{"currentJobMode":{"type":"enum","mode":["r","w"],"value":{"r":["HEAT","COOL"],"w":["HEAT","COOL"]}}},` +
		`"operation":{"boilerOperationMode":` + powerProp + `,"roomTempMode":{"type":"enum","mode":["r"],"value":{"r":["AIR","WATER"]}}},` +
		`"roomTemperatureInUnits":[{"unit":"C","airCurrentTemperature":` + numberRead + `,"targetTemperature":` + numberRead + `,` +
		`"airHeatTargetTemperature":` + rng("16", "30") + `,"airCoolTargetTemperature":` + rng("18", "30") + `,` +
		`"waterHeatTargetTemperature":` + rng("25", "55") + `,"waterCoolTargetTemperature":` + rng("6", "18") + `},` +
		`{"unit":"F","airCurrentTemperature":` + numberRead + `,"targetTemperature":` + numberRead + `,"airHeatTargetTemperature":` + rng("60", "86") + `}]}}`
	state := `{"boilerJobMode":{"currentJobMode":"HEAT"},"operation":{"boilerOperationMode":"POWER_ON"},` +
		`"roomTemperatureInUnits":[{"unit":"C","airCurrentTemperature":21,"targetTemperature":23},{"unit":"F","airCurrentTemperature":70,"targetTemperature":73}]}`
	d := newLG(t, "DEVICE_SYSTEM_BOILER", prof, state, "")
	if st := d.state(); st["range/temperature"] != 23.0 || st["float/temperature"] != 21.0 {
		t.Fatalf("значения: %v", st)
	}
	if code, body := d.act(alice.CapRange, "temperature", 24.0); code != "" ||
		body != `{"roomTemperatureInUnits":{"airHeatTargetTemperature":24,"unit":"C"}}` {
		t.Fatalf("воздух, нагрев: %q %s", code, body)
	}
	d.a.apply(json.RawMessage(`{"boilerJobMode":{"currentJobMode":"COOL"},"operation":{"roomTempMode":"WATER"}}`), false)
	if code, body := d.act(alice.CapRange, "temperature", 10.0); code != "" ||
		body != `{"roomTemperatureInUnits":{"unit":"C","waterCoolTargetTemperature":10}}` {
		t.Fatalf("вода, охлаждение: %q %s", code, body)
	}
}

func TestOven(t *testing.T) {
	part := func(name string) string {
		return `{"location":{"locationName":"` + name + `"},` +
			`"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["INITIAL","PREHEATING","COOKING","DONE","CLEANING_IS_DONE"]}}},` +
			`"operation":{"ovenOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP"]}}},` +
			`"remoteControlEnable":{"remoteControlEnabled":{"type":"boolean","mode":["r"]}},` +
			`"temperature":[{"unit":"C","targetTemperature":{"type":"range","mode":["r","w"],"value":{"w":{"min":80,"max":285,"step":5}}}},` +
			`{"unit":"F","targetTemperature":{"type":"range","mode":["r","w"],"value":{"w":{"min":170,"max":550,"step":5}}}}]}`
	}
	prof := `{"notification":{"push":["PREHEATING_IS_COMPLETE","COOKING_IS_COMPLETE"]},"property":[` + part("UPPER") + `,` + part("LOWER") + `]}`
	state := `[{"location":{"locationName":"UPPER"},"runState":{"currentState":"COOKING"},"temperature":{"targetTemperature":180,"unit":"C"}},` +
		`{"location":{"locationName":"LOWER"},"runState":{"currentState":"INITIAL"},"temperature":{"targetTemperature":350,"unit":"F"}}]`
	d := newLG(t, "DEVICE_OVEN", prof, state, "UPPER")
	if st := d.state(); st["range/temperature"] != 180.0 || st["on_off/on"] != true {
		t.Fatalf("значения: %v", st)
	}
	if got := d.params("range/temperature"); !strings.Contains(got, `"min":80`) || !strings.Contains(got, `"max":285`) {
		t.Fatalf("пределы: %s", got)
	}
	// Температура уходит только настройкой: без START нагрев сам не начнётся.
	if code, body := d.act(alice.CapRange, "temperature", 200.0); code != "" ||
		body != `{"location":{"locationName":"UPPER"},"temperature":{"targetTemperature":200,"unit":"C"}}` {
		t.Fatalf("температура: %q %s", code, body)
	}
	for _, end := range []string{"DONE", "CLEANING_IS_DONE"} {
		d.a.apply(json.RawMessage(`[{"location":{"locationName":"UPPER"},"runState":{"currentState":"`+end+`"}}]`), false)
		if d.state()["on_off/on"] != false {
			t.Fatalf("%s - прибор включён", end)
		}
	}
	// Значение в градусах Фаренгейта не становится значением шлюза.
	low := newLG(t, "DEVICE_OVEN", prof, state, "LOWER")
	if v, ok := low.a.State().Values["temperature.targetTemperatureC"]; ok {
		t.Fatalf("значение F: %v", v)
	}
}

const (
	towerSide = `"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["POWER_OFF","INITIAL","RUNNING","END"]}}},` +
		`"remoteControlEnable":{"remoteControlEnabled":{"type":"boolean","mode":["r"]}}`
	towerProfile = `{"washer":{"notification":{"push":["WASHING_IS_COMPLETE","ERROR_DURING_WASHING"]},"property":{` + towerSide + `,` +
		`"operation":{"washerOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP","POWER_OFF"]}}}}},` +
		`"dryer":{"notification":{"push":["DRYING_IS_COMPLETE","ERROR_DURING_DRYING"]},"property":{` + towerSide + `,` +
		`"operation":{"dryerOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP","POWER_OFF"]}}}}}}`
	towerState = `{"washer":{"runState":{"currentState":"RUNNING"},"remoteControlEnable":{"remoteControlEnabled":true}},` +
		`"dryer":{"runState":{"currentState":"INITIAL"},"remoteControlEnable":{"remoteControlEnabled":true}}}`
)

func TestWashTower(t *testing.T) {
	m, err := parseProfile("DEVICE_WASHTOWER", []byte(towerProfile), "")
	if err != nil || m.tower != "washer" || !slices.Equal(m.Locations, []string{"washer", "dryer"}) {
		t.Fatalf("колонна: %+v %v", m, err)
	}
	if _, err := parseProfile("DEVICE_WASHTOWER", []byte(towerProfile), "MAIN"); !errors.Is(err, errNoLocation) {
		t.Fatalf("чужая часть: %v", err)
	}
	washer := newLG(t, "DEVICE_WASHTOWER", towerProfile, towerState, "washer")
	dryer := newLG(t, "DEVICE_WASHTOWER", towerProfile, towerState, "dryer")
	if washer.state()["on_off/on"] != true || dryer.state()["on_off/on"] != false {
		t.Fatalf("состояния частей: %v %v", washer.state(), dryer.state())
	}
	if code, body := washer.act(alice.CapOnOff, "on", false); code != "" || body != `{"washer":{"operation":{"washerOperationMode":"STOP"}}}` {
		t.Fatalf("стиральная машина: %q %s", code, body)
	}
	if code, body := dryer.act(alice.CapOnOff, "on", true); code != "" || body != `{"dryer":{"operation":{"dryerOperationMode":"START"}}}` {
		t.Fatalf("сушильная машина: %q %s", code, body)
	}
	// Отчёт MQTT лежит под ключом части и меняет только её.
	report := json.RawMessage(`{"dryer":{"runState":{"currentState":"RUNNING"}}}`)
	washer.a.apply(report, false)
	dryer.a.apply(report, false)
	if washer.a.State().Values["runState.currentState"] != "RUNNING" || dryer.a.State().Values["runState.currentState"] != "RUNNING" {
		t.Fatal("отчёт части")
	}
	washer.a.apply(json.RawMessage(`{"washer":{"runState":{"currentState":"END"}}}`), false)
	dryer.a.apply(json.RawMessage(`{"washer":{"runState":{"currentState":"END"}}}`), false)
	if washer.state()["on_off/on"] != false || dryer.state()["on_off/on"] != true {
		t.Fatal("чужой отчёт изменил часть")
	}
	if got := dryer.items(); !slices.Contains(got, "on_off") || !slices.Contains(got, "event/button") {
		t.Fatalf("умения: %v", got)
	}
	if !strings.Contains(dryer.params("event/button"), "long_press") {
		t.Fatalf("ошибка сушильной машины: %s", dryer.params("event/button"))
	}
}

// Профиль одного прибора, который не разбирается, не портит ответ всего обновления.
func TestRefreshKeepsOthers(t *testing.T) {
	ca := newCA(t)
	api := newAPI(t, ca)
	api.mu.Lock()
	api.badID = "dev-2"
	api.mu.Unlock()
	c := newCloud(t, t.TempDir(), api, ca)
	if err := c.RefreshAccount(context.Background()); err != nil {
		t.Fatalf("обновление: %v", err)
	}
	if got := c.Locations("dev-3"); !slices.Equal(got, []string{"FRIDGE", "FREEZER"}) {
		t.Fatalf("остальные профили: %v", got)
	}
	if len(c.Account()) != 3 {
		t.Fatalf("список: %v", c.Account())
	}
}

const (
	hoodProfile = `{"property":{` +
		`"ventilation":{"fanSpeed":{"type":"range","mode":["r","w"],"value":{"r":{"min":0,"max":5,"step":1},"w":{"min":0,"max":5,"step":1}}}},` +
		`"lamp":{"lampBrightness":{"type":"range","mode":["r","w"],"value":{"r":{"min":0,"max":2,"step":1},"w":{"min":0,"max":2,"step":1}}}}}}`
	hoodState = `{"ventilation":{"fanSpeed":1},"lamp":{"lampBrightness":2}}`
)

func TestHood(t *testing.T) {
	d := newLG(t, "DEVICE_HOOD", hoodProfile, hoodState, "")
	// Уведомлений в профиле нет, поэтому кнопки нет (ошибка 38).
	if got := d.items(); !slices.Equal(got, []string{"mode/fan_speed", "toggle/backlight"}) {
		t.Fatalf("умения: %v", got)
	}
	// Яркость 2: подсветка горит, хотя такого значения у переключателя нет.
	if st := d.state(); st["toggle/backlight"] != true || st["mode/fan_speed"] != "one" {
		t.Fatalf("состояние: %v", st)
	}
	if code, body := d.act(alice.CapMode, "fan_speed", "three"); code != "" ||
		body != `{"lamp":{"lampBrightness":2},"ventilation":{"fanSpeed":3}}` {
		t.Fatalf("скорость: %q %s", code, body)
	}
	if code, body := d.act(alice.CapToggle, "backlight", false); code != "" ||
		body != `{"lamp":{"lampBrightness":0},"ventilation":{"fanSpeed":1}}` {
		t.Fatalf("подсветка выключена: %q %s", code, body)
	}
	// Подсветка уже горит: «включи» не меняет яркость.
	if code, body := d.act(alice.CapToggle, "backlight", true); code != "" ||
		body != `{"lamp":{"lampBrightness":2},"ventilation":{"fanSpeed":1}}` {
		t.Fatalf("подсветка горит: %q %s", code, body)
	}
	d.a.apply(json.RawMessage(`{"lamp":{"lampBrightness":0}}`), false)
	if st := d.state(); st["toggle/backlight"] != false {
		t.Fatalf("яркость 0: %v", st)
	}
	if code, body := d.act(alice.CapToggle, "backlight", true); code != "" ||
		body != `{"lamp":{"lampBrightness":1},"ventilation":{"fanSpeed":1}}` {
		t.Fatalf("подсветка включена: %q %s", code, body)
	}
	// Второго значения нет: команда не уходит с выдуманным значением.
	empty := newLG(t, "DEVICE_HOOD", hoodProfile, `{"ventilation":{"fanSpeed":1}}`, "")
	if code, body := empty.act(alice.CapMode, "fan_speed", "two"); code != alice.CodeWrongMode || body != "" {
		t.Fatalf("без яркости: %q %s", code, body)
	}
}

// Микроволновка без вытяжки: ни скорости, ни подсветки, только кнопка уведомлений.
func TestMicrowaveWithoutHood(t *testing.T) {
	prof := `{"notification":{"push":["COOKING_IS_COMPLETE"]},"property":{"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["INITIAL"]}}}}}`
	d := newLG(t, "DEVICE_MICROWAVE_OVEN", prof, `{"runState":{"currentState":"INITIAL"}}`, "")
	if got := d.items(); !slices.Equal(got, []string{"event/button"}) {
		t.Fatalf("умения: %v", got)
	}
}

// Настоящая раскладка стиральной машины: питание только для записи, но on_off и кнопка остаются.
func TestWasherKeepsCapabilities(t *testing.T) {
	d := newLG(t, "DEVICE_WASHER", washerProfile, washerState, "")
	if got := d.items(); !slices.Equal(got, []string{"event/button", "on_off"}) {
		t.Fatalf("умения: %v", got)
	}
	if got := d.params("event/button"); !strings.Contains(got, "click") || !strings.Contains(got, "long_press") {
		t.Fatalf("кнопка: %s", got)
	}
	desc := d.a.Describe()
	if op := desc.Options["operation.washerOperationMode"]; op.ReadOnly || !op.Setting || !desc.Knows("operation.washerOperationMode") {
		t.Fatalf("питание: %+v", op)
	}
	if st := desc.Options["runState.currentState"]; !st.ReadOnly || st.Setting {
		t.Fatalf("состояние: %+v", st)
	}
	if !desc.Knows(RemainKey) || !desc.EventsKnown {
		t.Fatalf("описание: %+v", desc)
	}
	if code, body := d.act(alice.CapOnOff, "on", true); code != "" ||
		body != `{"location":{"locationName":"MAIN"},"operation":{"washerOperationMode":"START"}}` {
		t.Fatalf("запуск: %q %s", code, body)
	}
	// Значение не из профиля, которое прибор прислал, владелец может сопоставить сам.
	own := `{"location":{"locationName":"MAIN"},"cycle":{"cycleCount":10}}`
	d.a.apply(json.RawMessage(own), false)
	if desc := d.a.Describe(); !desc.Knows("cycle.cycleCount") {
		t.Fatalf("значение не из профиля: %v", desc.Reports)
	}
}

// Кондиционер без датчиков и без качания не обещает их Алисе.
func TestReportsHideMissing(t *testing.T) {
	prof := `{"property":{"operation":{"airConOperationMode":` + powerProp + `},` +
		`"airConJobMode":{"currentJobMode":{"type":"enum","mode":["r","w"],"value":{"r":["COOL"],"w":["COOL"]}}}}}`
	d := newLG(t, "DEVICE_AIR_CONDITIONER", prof, `{"operation":{"airConOperationMode":"POWER_ON"}}`, "")
	if got := d.items(); !slices.Equal(got, []string{"mode/thermostat", "on_off"}) {
		t.Fatalf("умения: %v", got)
	}
	// Переключатель по свойству только для чтения не обещается.
	ro := `{"property":{"windDirection":{"rotateUpDown":{"type":"boolean","mode":["r"]}}}}`
	d = newLG(t, "DEVICE_AIR_CONDITIONER", ro, `{}`, "")
	if slices.Contains(d.items(), "toggle/oscillation") {
		t.Fatalf("переключатель только для чтения: %v", d.items())
	}
}

// Осушитель называет скорость windStrengthLevel: шлюз показывает и пишет её как windStrength.
func TestDehumidifierWindLevel(t *testing.T) {
	prof := `{"property":{"airFlow":{"windStrengthLevel":{"type":"enum","mode":["r","w"],"value":{"r":["LOW","HIGH"],"w":["LOW","HIGH"]}}},` +
		`"operation":{"dehumidifierOperationMode":` + powerProp + `}}}`
	d := newLG(t, "DEVICE_DEHUMIDIFIER", prof, `{"airFlow":{"windStrengthLevel":"HIGH"},"operation":{"dehumidifierOperationMode":"POWER_ON"}}`, "")
	if st := d.state(); st["mode/fan_speed"] != "high" {
		t.Fatalf("скорость: %v", st)
	}
	if code, body := d.act(alice.CapMode, "fan_speed", "low"); code != "" || body != `{"airFlow":{"windStrengthLevel":"LOW"}}` {
		t.Fatalf("команда: %q %s", code, body)
	}
}

func TestVentilatorAndFan(t *testing.T) {
	prof := `{"property":{"operation":{"ventOperationMode":` + powerProp + `},` +
		`"ventJobMode":{"currentJobMode":{"type":"enum","mode":["r","w"],"value":{"r":["VENT_AUTO","VENT_NATURE","VENT_HEAT_EXCHANGE"],"w":["VENT_AUTO","VENT_NATURE","VENT_HEAT_EXCHANGE"]}}},` +
		`"airQualitySensor":{"CO2":{"type":"enum","mode":["r"],"value":{"r":["GOOD"]}}}}}`
	d := newLG(t, "DEVICE_VENTILATOR", prof, `{"operation":{"ventOperationMode":"POWER_ON"},"ventJobMode":{"currentJobMode":"VENT_HEAT_EXCHANGE"},"airQualitySensor":{"CO2":"GOOD"}}`, "")
	if st := d.state(); st["on_off/on"] != true || st["mode/program"] != "heat" {
		t.Fatalf("вентиляция: %v", st)
	}
	if code, body := d.act(alice.CapOnOff, "on", false); code != "" || body != `{"operation":{"ventOperationMode":"POWER_OFF"}}` {
		t.Fatalf("выключение: %q %s", code, body)
	}
	if got := d.items(); slices.Contains(got, "float/co2_level") {
		t.Fatalf("CO2: %v", got)
	}

	prof = `{"property":{"operation":{"ceilingfanOperationMode":` + powerProp + `},` +
		`"airFlow":{"windStrength":{"type":"enum","mode":["r","w"],"value":{"r":["LOW","MID","HIGH","TURBO"],"w":["LOW","MID","HIGH","TURBO"]}}}}}`
	d = newLG(t, "DEVICE_CEILING_FAN", prof, `{"operation":{"ceilingfanOperationMode":"POWER_ON"},"airFlow":{"windStrength":"TURBO"}}`, "")
	if st := d.state(); st["on_off/on"] != true || st["mode/fan_speed"] != "turbo" {
		t.Fatalf("вентилятор: %v", st)
	}
	if code, body := d.act(alice.CapOnOff, "on", false); code != "" || body != `{"operation":{"ceilingfanOperationMode":"POWER_OFF"}}` {
		t.Fatalf("вентилятор выключен: %q %s", code, body)
	}
}

func TestAirFanHumidifierCleaners(t *testing.T) {
	// Режимы очистителя с вентилятором.
	prof := `{"property":{"operation":{"airFanOperationMode":` + powerProp + `},` +
		`"airFanJobMode":{"currentJobMode":{"type":"enum","mode":["r","w"],"value":{"r":["SPOT_CLEAN","SPACE_CLEAN","DIRECT_CLEAN","NATURE_CLEAN","UP_FEATURE"],"w":["SPOT_CLEAN","SPACE_CLEAN","DIRECT_CLEAN","NATURE_CLEAN"]}}}}}`
	d := newLG(t, "DEVICE_AIR_PURIFIER_FAN", prof, `{"operation":{"airFanOperationMode":"POWER_ON"},"airFanJobMode":{"currentJobMode":"SPOT_CLEAN"}}`, "")
	if st := d.state(); st["mode/program"] != "express" {
		t.Fatalf("режим: %v", st)
	}
	if code, body := d.act(alice.CapMode, "program", "auto"); code != "" || body != `{"airFanJobMode":{"currentJobMode":"NATURE_CLEAN"}}` {
		t.Fatalf("команда: %q %s", code, body)
	}

	// Ночник увлажнителя - перечисление ON и OFF.
	prof = `{"property":{"moodLamp":{"moodLampState":{"type":"enum","mode":["r","w"],"value":{"r":["ON","OFF"],"w":["ON","OFF"]}}}}}`
	d = newLG(t, "DEVICE_HUMIDIFIER", prof, `{"moodLamp":{"moodLampState":"ON"}}`, "")
	if st := d.state(); st["toggle/backlight"] != true {
		t.Fatalf("ночник: %v", st)
	}
	if code, body := d.act(alice.CapToggle, "backlight", false); code != "" || body != `{"moodLamp":{"moodLampState":"OFF"}}` {
		t.Fatalf("ночник выключен: %q %s", code, body)
	}

	// Заряд пылесоса - battery.percent; режим охраны и расписание - не работа.
	prof = `{"notification":{"push":["CLEANING_IS_COMPLETED"]},"property":{"operation":{"cleanOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","HOMING"]}}},` +
		`"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["WORKING","MONITORING_DETECTING","RESERVATION"]}}},"battery":{"percent":` + numberRead + `}}}`
	d = newLG(t, "DEVICE_ROBOT_CLEANER", prof, `{"runState":{"currentState":"WORKING"},"battery":{"percent":70}}`, "")
	if st := d.state(); st["float/battery_level"] != 70.0 || st["on_off/on"] != true {
		t.Fatalf("пылесос: %v", st)
	}
	for _, idle := range []string{"MONITORING_DETECTING", "MONITORING_MOVING", "MONITORING_POSITIONING", "MACROSECTOR", "RESERVATION", "SETDATE"} {
		d.a.apply(json.RawMessage(`{"runState":{"currentState":"`+idle+`"}}`), false)
		if d.state()["on_off/on"] != false {
			t.Fatalf("%s - пылесос работает", idle)
		}
	}
}

func TestCompartments(t *testing.T) {
	// Камера кимчи: температуру только показывает, текстовый режим - без значения, дверцы нет.
	prof := `{"notification":{"push":["DOOR_IS_OPEN"]},"property":{"temperature":[` +
		`{"locationName":"TOP","targetTemperature":{"type":"enum","mode":["r"],"value":{"r":["TEMPERATURE_NUMBER"]}}},` +
		`{"locationName":"MIDDLE","targetTemperature":{"type":"enum","mode":["r"],"value":{"r":["KIMCHI","MEAT_FISH"]}}}]}}`
	state := `{"temperature":[{"locationName":"TOP","targetTemperature":6},{"locationName":"MIDDLE","targetTemperature":"KIMCHI"}]}`
	d := newLG(t, "DEVICE_KIMCHI_REFRIGERATOR", prof, state, "TOP")
	if got := d.items(); !slices.Equal(got, []string{"event/button", "float/temperature"}) {
		t.Fatalf("кимчи: %v", got)
	}
	if st := d.state(); st["float/temperature"] != 6.0 {
		t.Fatalf("температура камеры: %v", st)
	}
	d = newLG(t, "DEVICE_KIMCHI_REFRIGERATOR", prof, state, "MIDDLE")
	if st := d.state(); st["float/temperature"] != nil {
		t.Fatalf("текстовый режим: %v", st)
	}

	// Винный шкаф: температура камеры и яркость в процентах вместо переключателя.
	prof = `{"property":{"temperatureInUnits":[` +
		`{"locationName":"WINE_UPPER","targetTemperatureC":{"type":"range","mode":["r","w"],"value":{"w":{"min":5,"max":18,"step":1}}}}],` +
		`"operation":{"lightStatus":{"type":"range","mode":["r","w"],"value":{"w":{"min":0,"max":100,"step":10}}}}}}`
	state = `{"temperatureInUnits":[{"locationName":"WINE_UPPER","targetTemperatureC":12,"unit":"C"}],"operation":{"lightStatus":40}}`
	d = newLG(t, "DEVICE_WINE_CELLAR", prof, state, "")
	if got := d.items(); !slices.Equal(got, []string{"range/brightness", "range/temperature"}) {
		t.Fatalf("винный шкаф: %v", got)
	}
	if st := d.state(); st["range/brightness"] != 40.0 || st["range/temperature"] != 12.0 {
		t.Fatalf("значения: %v", st)
	}
	if code, body := d.act(alice.CapRange, "brightness", 60.0); code != "" || body != `{"operation":{"lightStatus":60}}` {
		t.Fatalf("яркость: %q %s", code, body)
	}
	if code, body := d.act(alice.CapRange, "temperature", 10.0); code != "" ||
		body != `{"temperatureInUnits":{"locationName":"WINE_UPPER","targetTemperatureC":10,"unit":"C"}}` {
		t.Fatalf("температура камеры: %q %s", code, body)
	}
}

// Отчёт объектом с location чужой части не меняет состояние этой части.
func TestForeignPartReport(t *testing.T) {
	d := newLG(t, "DEVICE_WASHCOMBO_MAIN", washerProfile, washerState, "MAIN")
	d.a.apply(json.RawMessage(`{"location":{"locationName":"MINI"},"runState":{"currentState":"END"}}`), false)
	if v := d.a.State().Values["runState.currentState"]; v != "INITIAL" {
		t.Fatalf("чужая часть: %v", v)
	}
	d.a.apply(json.RawMessage(`{"location":{"locationName":"MAIN"},"runState":{"currentState":"RUNNING"}}`), false)
	if v := d.a.State().Values["runState.currentState"]; v != "RUNNING" {
		t.Fatalf("своя часть: %v", v)
	}
}

func TestLGButtonMaps(t *testing.T) {
	cases := []struct {
		typ, event, want string
	}{
		{"DEVICE_ROBOT_CLEANER", "CLEANING_IS_COMPLETED", "click"},
		{"DEVICE_AIR_PURIFIER", "FILTER_RESET_COMPLETE", "click"},
		{"DEVICE_OVEN", "COOKING_IS_COMPLETE", "click"},
		{"DEVICE_OVEN", "PREHEATING_IS_COMPLETE", "double_click"},
		{"DEVICE_ROBOT_CLEANER", "CLEANING_IS_FAILED", "long_press"},
		{"DEVICE_DRYER", "ERROR_DURING_DRYING", "long_press"},
		{"DEVICE_WASHTOWER_DRYER", "ERROR_DURING_DRYING", "long_press"},
		{"DEVICE_DRYER", "DRYING_FAILED", "long_press"},
		{"DEVICE_WASHER", "ERROR_DURING_WASHING", "long_press"},
	}
	for _, c := range cases {
		p, err := profile.Resolve(c.typ, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := p.Events.Match(c.event); got != c.want {
			t.Errorf("%s %s: %q", c.typ, c.event, got)
		}
	}
	// Уведомление об окончании стирки кнопку не нажимает: её нажимает переход в END.
	p, _ := profile.Resolve("DEVICE_WASHER", nil, nil)
	if _, ok := p.Events.Match("WASHING_IS_COMPLETE"); ok {
		t.Error("второй click за стирку")
	}
}

// Конец работы стайлера - COMPLETE и RUNNING_END: прибор выключен, кнопка нажата.
func TestStyler(t *testing.T) {
	prof := `{"property":[{"location":{"locationName":"MAIN"},` +
		`"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["RUNNING","COMPLETE","RUNNING_END"]}}},` +
		`"operation":{"stylerOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP"]}}}}]}`
	d := newLG(t, "DEVICE_STYLER", prof, `[{"location":{"locationName":"MAIN"},"runState":{"currentState":"RUNNING"}}]`, "")
	if d.state()["on_off/on"] != true {
		t.Fatal("стайлер работает")
	}
	tr := d.Profile.Events.Transitions()["runState.currentState"]
	slices.Sort(tr)
	if !slices.Equal(tr, []string{"COMPLETE", "RUNNING_END"}) {
		t.Fatalf("переходы: %v", tr)
	}
	for _, end := range []string{"COMPLETE", "RUNNING_END"} {
		d.a.apply(json.RawMessage(`[{"location":{"locationName":"MAIN"},"runState":{"currentState":"`+end+`"}}]`), false)
		if d.state()["on_off/on"] != false {
			t.Fatalf("%s - стайлер включён", end)
		}
	}
}

// В список попадают только небольшие записываемые диапазоны целых чисел.
func TestRangeValues(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`{"type":"range","mode":["r","w"],"value":{"w":{"min":0,"max":2,"step":1}}}`, []string{"0", "1", "2"}},
		{`{"type":"range","mode":["r"],"value":{"r":{"min":0,"max":2,"step":1}}}`, nil},
		{`{"type":"range","mode":["r","w"],"value":{"w":{"min":18,"max":30,"step":1}}}`, nil},
		{`{"type":"range","mode":["r","w"],"value":{"w":{"min":0,"max":2,"step":0.5}}}`, nil},
	}
	for _, c := range cases {
		if got := parseProp(json.RawMessage(c.raw)).Values; !slices.Equal(got, c.want) {
			t.Errorf("%s: %v", c.raw, got)
		}
	}
}

// Уведомления, у которых нет значения в отчёте: состояние держится до следующего запуска и переживает
// полный отчёт.
func TestStoredPush(t *testing.T) {
	const prof = `{"notification":{"push":["SALT_REFILL_IS_NEEDED","WATER_LEAK_HAS_OCCURRED"]},"property":{
		"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["END","RUNNING"]}}}}}`
	d := newLG(t, "DEVICE_DISH_WASHER", prof, `{"runState":{"currentState":"END"}}`, "")
	state := func(instance string) any { return d.state()[instance] }
	if d.params("event/water_leak") == "" {
		t.Fatalf("нет протечки: %v", d.items())
	}
	if got := d.Device.States(); len(got) != 1 || state("event/water_leak") != nil {
		t.Fatalf("до уведомления: %v", d.state())
	}
	d.a.push("SALT_REFILL_IS_NEEDED")
	d.a.push("WATER_LEAK_HAS_OCCURRED")
	d.a.apply(json.RawMessage(`{"runState":{"currentState":"END"}}`), true)
	if state("event/food_level") != "low" || state("event/water_leak") != "leak" {
		t.Fatalf("после уведомления: %v", d.state())
	}
	d.a.apply(json.RawMessage(`{"runState":{"currentState":"RUNNING"}}`), false)
	if state("event/food_level") != "normal" || state("event/water_leak") != "dry" {
		t.Fatalf("после запуска: %v", d.state())
	}
}
