package thinq

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/profile"
)

// Значения и раскладки ниже - заглушки по библиотеке LG и фикстурам Home Assistant.

// enumProp - перечисление для чтения и записи.
func enumProp(values ...string) string {
	list, _ := json.Marshal(values)
	return `{"type":"enum","mode":["r","w"],"value":{"r":` + string(list) + `,"w":` + string(list) + `}}`
}

const boolProp = `{"type":"boolean","mode":["r","w"],"value":{"r":[true,false],"w":[true,false]}}`

func has(items []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(items, w) {
			return false
		}
	}
	return true
}

func TestRobotPause(t *testing.T) {
	prof := `{"property":{"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["WORKING","PAUSE","CHARGING"]}}},` +
		`"operation":{"cleanOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","PAUSE","RESUME","HOMING"]}}}}}`
	d := newLG(t, "DEVICE_ROBOT_CLEANER", prof, `{"runState":{"currentState":"WORKING"}}`, "")
	if st := d.state(); st["toggle/pause"] != false || st["on_off/on"] != true {
		t.Fatalf("состояние: %v", st)
	}
	// Снятие паузы вне паузы запустило бы прибор зря: команда не уходит.
	if code, body := d.act(alice.CapToggle, "pause", false); code != "" || body != "" {
		t.Fatalf("снятие паузы в работе: %q %s", code, body)
	}
	if code, body := d.act(alice.CapToggle, "pause", true); code != "" || body != `{"operation":{"cleanOperationMode":"PAUSE"}}` {
		t.Fatalf("пауза: %q %s", code, body)
	}
	d.a.apply(json.RawMessage(`{"runState":{"currentState":"PAUSE"}}`), false)
	if st := d.state(); st["toggle/pause"] != true {
		t.Fatalf("на паузе: %v", st)
	}
	if code, body := d.act(alice.CapToggle, "pause", false); code != "" || body != `{"operation":{"cleanOperationMode":"RESUME"}}` {
		t.Fatalf("продолжение: %q %s", code, body)
	}
	// Без записи переключателя нет.
	noWrite := newLG(t, "DEVICE_ROBOT_CLEANER", `{"property":{"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["WORKING"]}}}}}`,
		`{"runState":{"currentState":"WORKING"}}`, "")
	if got := noWrite.items(); slices.Contains(got, "toggle/pause") {
		t.Fatalf("пауза без записи: %v", got)
	}
}

func TestHoodPower(t *testing.T) {
	d := newLG(t, "DEVICE_HOOD", hoodProfile, hoodState, "")
	if st := d.state(); st["on_off/on"] != true {
		t.Fatalf("работает: %v", st)
	}
	if code, body := d.act(alice.CapOnOff, "on", false); code != "" ||
		body != `{"lamp":{"lampBrightness":2},"ventilation":{"fanSpeed":0}}` {
		t.Fatalf("выключение: %q %s", code, body)
	}
	d.a.apply(json.RawMessage(`{"ventilation":{"fanSpeed":0}}`), false)
	if st := d.state(); st["on_off/on"] != false {
		t.Fatalf("выключена: %v", st)
	}
	if code, body := d.act(alice.CapOnOff, "on", true); code != "" ||
		body != `{"lamp":{"lampBrightness":2},"ventilation":{"fanSpeed":1}}` {
		t.Fatalf("включение: %q %s", code, body)
	}
	// У микроволновки вытяжка - не весь прибор: on_off нет.
	mw := newLG(t, "DEVICE_MICROWAVE_OVEN", hoodProfile, hoodState, "")
	if got := mw.items(); slices.Contains(got, "on_off") {
		t.Fatalf("микроволновка: %v", got)
	}
}

func TestAirFanAdditions(t *testing.T) {
	prof := `{"property":{"operation":{"airFanOperationMode":` + powerProp + `},` +
		`"airFlow":{"windStrength":` + enumProp("WIND_1", "WIND_10", "AUTO", "POWER") + `,` +
		`"windAngle":` + enumProp("OFF", "ANGLE_45", "ANGLE_140") + `,` +
		`"warmMode":` + enumProp("WARM_ON", "WARM_OFF") + `,` +
		`"windTemperature":{"type":"range","mode":["r","w"],"value":{"r":{"min":16,"max":30,"step":1},"w":{"min":16,"max":30,"step":1}}}}}}`
	state := `{"operation":{"airFanOperationMode":"POWER_ON"},"airFlow":{"windStrength":"WIND_10","windAngle":"ANGLE_140","warmMode":"WARM_OFF","windTemperature":22}}`
	d := newLG(t, "DEVICE_AIR_PURIFIER_FAN", prof, state, "")
	st := d.state()
	if st["mode/fan_speed"] != "ten" || st["mode/swing"] != "four" || st["toggle/keep_warm"] != false || st["range/temperature"] != 22.0 {
		t.Fatalf("состояние: %v", st)
	}
	if got := d.params("mode/fan_speed"); strings.Contains(got, `"low"`) || !strings.Contains(got, `"one"`) {
		t.Fatalf("скорости только прибора: %s", got)
	}
	for _, c := range []struct {
		typ, instance string
		value         any
		body          string
	}{
		{alice.CapMode, "fan_speed", "one", `{"airFlow":{"windStrength":"WIND_1"}}`},
		{alice.CapMode, "swing", "stationary", `{"airFlow":{"windAngle":"OFF"}}`},
		{alice.CapToggle, "keep_warm", true, `{"airFlow":{"warmMode":"WARM_ON"}}`},
		{alice.CapRange, "temperature", 24.0, `{"airFlow":{"windTemperature":24}}`},
	} {
		if code, body := d.act(c.typ, c.instance, c.value); code != "" || body != c.body {
			t.Fatalf("%s: %q %s", c.instance, code, body)
		}
	}
	// У модели без этих свойств умений нет.
	bare := newLG(t, "DEVICE_AIR_PURIFIER_FAN", `{"property":{"operation":{"airFanOperationMode":`+powerProp+`}}}`, `{}`, "")
	if got := bare.items(); !slices.Equal(got, []string{"on_off"}) {
		t.Fatalf("без свойств: %v", got)
	}
}

func TestHumidifiersProgram(t *testing.T) {
	prof := `{"property":{"operation":{"humidifierOperationMode":` + powerProp + `},` +
		`"humidifierJobMode":{"currentJobMode":` + enumProp("HUMIDIFY", "HUMIDIFY_AND_AIR_CLEAN", "AIR_CLEAN") + `},` +
		`"humidity":{"warmMode":` + enumProp("WARM_ON", "WARM_OFF") + `}}}`
	d := newLG(t, "DEVICE_HUMIDIFIER", prof, `{"humidifierJobMode":{"currentJobMode":"AIR_CLEAN"},"humidity":{"warmMode":"WARM_ON"}}`, "")
	if st := d.state(); st["mode/program"] != "three" || st["toggle/keep_warm"] != true {
		t.Fatalf("увлажнитель: %v", st)
	}
	if code, body := d.act(alice.CapMode, "program", "two"); code != "" ||
		body != `{"humidifierJobMode":{"currentJobMode":"HUMIDIFY_AND_AIR_CLEAN"}}` {
		t.Fatalf("режим: %q %s", code, body)
	}
	if code, body := d.act(alice.CapToggle, "keep_warm", false); code != "" || body != `{"humidity":{"warmMode":"WARM_OFF"}}` {
		t.Fatalf("тёплый пар: %q %s", code, body)
	}

	dry := `{"property":{"operation":{"dehumidifierOperationMode":` + powerProp + `},` +
		`"dehumidifierJobMode":{"currentJobMode":` + enumProp("SMART_HUMIDITY", "RAPID_HUMIDITY", "QUIET_HUMIDITY", "INTENSIVE_DRY", "CLOTHES_DRY") + `}}}`
	de := newLG(t, "DEVICE_DEHUMIDIFIER", dry, `{"dehumidifierJobMode":{"currentJobMode":"CLOTHES_DRY"}}`, "")
	if st := de.state(); st["mode/program"] != "dry" {
		t.Fatalf("осушитель: %v", st)
	}
	if code, body := de.act(alice.CapMode, "program", "quiet"); code != "" ||
		body != `{"dehumidifierJobMode":{"currentJobMode":"QUIET_HUMIDITY"}}` {
		t.Fatalf("режим осушителя: %q %s", code, body)
	}
	// Режим только для чтения: команда получает ошибку, а не молчаливое «готово».
	readOnly := `{"property":{"operation":{"dehumidifierOperationMode":` + powerProp + `},` +
		`"dehumidifierJobMode":{"currentJobMode":{"type":"enum","mode":["r"],"value":{"r":["CLOTHES_DRY"]}}}}}`
	ro := newLG(t, "DEVICE_DEHUMIDIFIER", readOnly, `{}`, "")
	if code, body := ro.act(alice.CapMode, "program", "dry"); code != alice.CodeInvalidAction || body != "" {
		t.Fatalf("только чтение: %q %s", code, body)
	}
}

func TestACAdditions(t *testing.T) {
	prof := `{"property":{"operation":{"airConOperationMode":` + powerProp + `},` +
		`"airConJobMode":{"currentJobMode":` + enumProp("COOL", "ENERGY_SAVING") + `},` +
		`"airFlow":{"windStrength":` + enumProp("SLOW", "LOW") + `},` +
		`"display":{"light":` + enumProp("ON", "OFF") + `},` +
		`"windDirection":{"rotateUpDown":` + boolProp + `,"rotateLeftRight":` + boolProp + `}}}`
	state := `{"airConJobMode":{"currentJobMode":"ENERGY_SAVING"},"airFlow":{"windStrength":"SLOW"},"display":{"light":"ON"},` +
		`"windDirection":{"rotateUpDown":true,"rotateLeftRight":false}}`
	d := newLG(t, "DEVICE_AIR_CONDITIONER", prof, state, "")
	st := d.state()
	if st["mode/thermostat"] != "eco" || st["mode/fan_speed"] != "quiet" || st["toggle/backlight"] != true ||
		st["mode/swing"] != "stationary" || st["toggle/oscillation"] != true {
		t.Fatalf("состояние: %v", st)
	}
	if code, body := d.act(alice.CapMode, "swing", "horizontal"); code != "" || body != `{"windDirection":{"rotateLeftRight":true}}` {
		t.Fatalf("качание: %q %s", code, body)
	}
	if code, body := d.act(alice.CapMode, "fan_speed", "quiet"); code != "" || body != `{"airFlow":{"windStrength":"SLOW"}}` {
		t.Fatalf("тихая скорость: %q %s", code, body)
	}
	if code, body := d.act(alice.CapToggle, "backlight", false); code != "" || body != `{"display":{"light":"OFF"}}` {
		t.Fatalf("подсветка: %q %s", code, body)
	}
	// Значения появились только у флагов режимов: остальные флаги редактор видит как раньше.
	desc := d.a.Describe()
	if o := desc.Options["windDirection.rotateUpDown"]; len(o.Values) != 0 {
		t.Fatalf("чужой флаг: %+v", o)
	}
	// Кондиционер без этих свойств их не обещает.
	bare := newLG(t, "DEVICE_AIR_CONDITIONER", `{"property":{"operation":{"airConOperationMode":`+powerProp+`}}}`, `{}`, "")
	if got := bare.items(); !slices.Equal(got, []string{"on_off"}) {
		t.Fatalf("без свойств: %v", got)
	}
}

func TestPurifierAndHeaters(t *testing.T) {
	pur := `{"property":{"operation":{"airPurifierOperationMode":` + powerProp + `},` +
		`"airPurifierJobMode":{"currentJobMode":` + enumProp("FAST", "SMART") + `},` +
		`"airFlow":{"windStrength":` + enumProp("SLOW") + `}}}`
	p := newLG(t, "DEVICE_AIR_PURIFIER", pur, `{"airPurifierJobMode":{"currentJobMode":"SMART"},"airFlow":{"windStrength":"SLOW"}}`, "")
	if st := p.state(); st["mode/program"] != "smart" || st["mode/fan_speed"] != "quiet" {
		t.Fatalf("очиститель: %v", st)
	}

	heater := `{"property":{"operation":{"waterHeaterOperationMode":` + powerProp + `},` +
		`"waterHeaterJobMode":{"currentJobMode":` + enumProp("HEAT_PUMP", "VACATION") + `}}}`
	h := newLG(t, "DEVICE_WATER_HEATER", heater, `{"waterHeaterJobMode":{"currentJobMode":"HEAT_PUMP"}}`, "")
	if st := h.state(); st["mode/heat"] != "normal" {
		t.Fatalf("водонагреватель: %v", st)
	}
	if code, body := h.act(alice.CapMode, "heat", "min"); code != "" || body != `{"waterHeaterJobMode":{"currentJobMode":"VACATION"}}` {
		t.Fatalf("отпуск: %q %s", code, body)
	}

	boiler := `{"property":{"operation":{"boilerOperationMode":` + powerProp + `,"hotWaterMode":` + enumProp("ON", "OFF") + `}}}`
	b := newLG(t, "DEVICE_SYSTEM_BOILER", boiler, `{"operation":{"hotWaterMode":"OFF"}}`, "")
	if st := b.state(); st["toggle/keep_warm"] != false {
		t.Fatalf("котёл: %v", st)
	}
	if code, body := b.act(alice.CapToggle, "keep_warm", true); code != "" || body != `{"operation":{"hotWaterMode":"ON"}}` {
		t.Fatalf("горячая вода: %q %s", code, body)
	}
}

func TestFridgeWorkSpeed(t *testing.T) {
	for _, c := range []struct{ prop, wire string }{{"rapidFreeze", "rapidFreeze"}, {"expressMode", "expressMode"}, {"expressFridge", "expressFridge"}} {
		prof := `{"property":{"refrigeration":{"` + c.prop + `":` + boolProp + `}}}`
		d := newLG(t, "DEVICE_REFRIGERATOR", prof, `{"refrigeration":{"`+c.prop+`":true}}`, "")
		if st := d.state(); st["mode/work_speed"] != "turbo" {
			t.Fatalf("%s: %v", c.prop, st)
		}
		if code, body := d.act(alice.CapMode, "work_speed", "normal"); code != "" || body != `{"refrigeration":{"`+c.wire+`":false}}` {
			t.Fatalf("%s: %q %s", c.prop, code, body)
		}
	}
	// Флаг только для чтения режимом не становится: команда не прошла бы.
	ro := newLG(t, "DEVICE_REFRIGERATOR", `{"property":{"refrigeration":{"rapidFreeze":{"type":"boolean","mode":["r"],"value":{"r":[true,false]}}}}}`, `{}`, "")
	if got := ro.items(); slices.Contains(got, "mode/work_speed") {
		t.Fatalf("только чтение: %v", got)
	}
}

func TestOvenModeWithoutStart(t *testing.T) {
	prof := `{"property":[{"location":{"locationName":"UPPER"},` +
		`"operation":{"ovenOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP"]}}},` +
		`"cook":{"cookMode":{"type":"enum","mode":["w"],"value":{"w":["BAKE","ROAST","CONVECTION_BAKE"]}}}}]}`
	d := newLG(t, "DEVICE_OVEN", prof, `[{"location":{"locationName":"UPPER"},"runState":{"currentState":"INITIAL"}}]`, "UPPER")
	if got := d.params("mode/program"); !strings.Contains(got, `"baking"`) || !strings.Contains(got, `"one"`) || strings.Contains(got, `"three"`) {
		t.Fatalf("режимы: %s", got)
	}
	if st := d.state(); st["mode/program"] != nil {
		t.Fatalf("состояния режима нет: %v", st)
	}
	code, body := d.act(alice.CapMode, "program", "frying")
	if code != "" || body != `{"cook":{"cookMode":"ROAST"},"location":{"locationName":"UPPER"}}` {
		t.Fatalf("режим: %q %s", code, body)
	}
	if strings.Contains(body, "START") {
		t.Fatalf("команда запускает нагрев: %s", body)
	}
	// Облако отвергло команду: Алиса получает ошибку.
	d.api.mu.Lock()
	d.api.controlCode = "2305"
	d.api.mu.Unlock()
	if code, _ := d.act(alice.CapMode, "program", "baking"); code != alice.CodeWrongMode {
		t.Fatalf("отказ облака: %q", code)
	}
}

func TestHomeBrew(t *testing.T) {
	prof := `{"property":{"recipe":{"beerRemain":{"type":"number","mode":["r"]}}}}`
	d := newLG(t, "DEVICE_HOME_BREW", prof, `{"recipe":{"beerRemain":40}}`, "")
	if got := d.items(); !slices.Equal(got, []string{"float/water_level"}) {
		t.Fatalf("умения: %v", got)
	}
	if st := d.state(); st["float/water_level"] != 40.0 {
		t.Fatalf("уровень: %v", st)
	}
}

// energyDevice - прибор deviceID из fakeDevices в работе: облако знает его профиль.
func energyDevice(t *testing.T, id, location string) (*Cloud, *fakeAPI, *lgDevice) {
	t.Helper()
	ca := newCA(t)
	api := newAPI(t, ca)
	c := newCloud(t, t.TempDir(), api, ca)
	d := attachDevice(t, c, api, id, location)
	return c, api, d
}

func attachDevice(t *testing.T, c *Cloud, api *fakeAPI, id, location string) *lgDevice {
	t.Helper()
	if err := c.loadProfile(context.Background(), id, fakeDevices[id].Type); err != nil {
		t.Fatal(err)
	}
	a, err := c.Appliance(id, location)
	if err != nil {
		t.Fatal(err)
	}
	c.attach(a)
	a.apply(json.RawMessage(fakeDevices[id].State), true)
	c.setConnected(true)
	t.Cleanup(func() { c.setConnected(false) })
	p, err := profile.Resolve(fakeDevices[id].Type, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &lgDevice{Device: &hcdevice.Device{ID: "x", Name: "Прибор", Profile: p, Appliance: a, Manufacturer: "LG"}, t: t, api: api, a: a}
}

func (f *fakeAPI) setEnergy(prof, usage string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.energyProfile, f.energyUsage, f.energyStatus = prof, usage, status
}

// energyStats - сколько было запросов профиля энергии и какие были запросы потребления.
func (f *fakeAPI) energyStats() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.energyProfiles, slices.Clone(f.energyUsages)
}

const (
	energyProfileBody = `{"result":{"property":["energyUsage"]}}`
	energyUsageBody   = `{"result":{"dataList":[{"energyUsage":1500},{"energyUsage":250}]}}`
)

func TestEnergyMeter(t *testing.T) {
	c, api, d := energyDevice(t, "dev-1", "")
	api.setEnergy(energyProfileBody, energyUsageBody, 0)
	ctx := context.Background()
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.Local)
	if got := d.items(); slices.Contains(got, "float/electricity_meter") {
		t.Fatalf("до профиля энергии: %v", got)
	}
	c.pollEnergy(ctx, now)
	if got := d.items(); !slices.Contains(got, "float/electricity_meter") {
		t.Fatalf("показание не объявлено: %v", got)
	}
	if !strings.Contains(d.params("float/electricity_meter"), "unit.kilowatt_hour") {
		t.Fatalf("единица: %s", d.params("float/electricity_meter"))
	}
	if st := d.state(); st["float/electricity_meter"] != 1.75 {
		t.Fatalf("показание: %v", st)
	}
	_, usages := api.energyStats()
	if len(usages) != 1 || !strings.Contains(usages[0], "period=MONTHLY") ||
		!strings.Contains(usages[0], "startDate=202603") || !strings.Contains(usages[0], "endDate=202603") ||
		!strings.Contains(usages[0], "property=energyUsage") {
		t.Fatalf("запрос: %v", usages)
	}
	// Полный отчёт прибора показание не стирает.
	d.a.apply(json.RawMessage(fakeDevices["dev-1"].State), true)
	if st := d.state(); st["float/electricity_meter"] != 1.75 {
		t.Fatalf("после отчёта: %v", st)
	}
	// Не чаще раза в час.
	c.pollEnergy(ctx, now.Add(59*time.Minute))
	if profiles, usages := api.energyStats(); len(usages) != 1 || profiles != 1 {
		t.Fatalf("запросы раньше часа: %d, %d", len(usages), profiles)
	}
	c.pollEnergy(ctx, now.Add(time.Hour))
	if profiles, usages := api.energyStats(); len(usages) != 2 || profiles != 1 {
		t.Fatalf("запросы через час: %d, %d", len(usages), profiles)
	}
	// Ошибка: один запрос без повторов, состояние и показание прежние.
	before := d.a.State()
	api.setEnergy(energyProfileBody, energyUsageBody, http.StatusInternalServerError)
	c.pollEnergy(ctx, now.Add(2*time.Hour))
	if _, usages := api.energyStats(); len(usages) != 3 {
		t.Fatalf("повторы после ошибки: %d", len(usages))
	}
	after := d.a.State()
	if after.Online != before.Online || after.Values[EnergyKey] != 1.75 || len(after.Values) != len(before.Values) {
		t.Fatalf("ошибка изменила состояние: %+v", after)
	}
	// Список свойств сохранён: после перезапуска показание объявлено сразу, без запроса профиля.
	data, err := os.ReadFile(c.profilePath("dev-1"))
	if err != nil || !strings.Contains(string(data), `"energyUsage"`) {
		t.Fatalf("файл профиля: %v %s", err, data)
	}
	// Обновление профиля прибора его не теряет.
	if err := c.loadProfile(ctx, "dev-1", "DEVICE_WASHER"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.profiles["dev-1"].Energy, energyUsageProp) {
		t.Fatalf("после обновления профиля: %+v", c.profiles["dev-1"].Energy)
	}
	again, err := New(c.path, c.dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	a2, err := again.Appliance("dev-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if desc := a2.Describe(); !desc.Knows(EnergyKey) {
		t.Fatalf("после перезапуска: %v", desc.Reports)
	}
}

func TestEnergyAbsent(t *testing.T) {
	c, api, d := energyDevice(t, "dev-1", "")
	ctx := context.Background()
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.Local)
	c.pollEnergy(ctx, now)
	if profiles, usages := api.energyStats(); profiles != 1 || len(usages) != 0 {
		t.Fatalf("без профиля энергии: %d, %v", profiles, usages)
	}
	if got := d.items(); slices.Contains(got, "float/electricity_meter") {
		t.Fatalf("показания нет: %v", got)
	}
	// Профиля нет: следующий вопрос - через сутки.
	c.pollEnergy(ctx, now.Add(23*time.Hour))
	if profiles, _ := api.energyStats(); profiles != 1 {
		t.Fatalf("вопросов за сутки: %d", profiles)
	}
	c.pollEnergy(ctx, now.Add(24*time.Hour))
	if profiles, _ := api.energyStats(); profiles != 2 {
		t.Fatalf("вопросов через сутки: %d", profiles)
	}
	// Сбой облака - не «профиля нет»: вопрос через час, и запросов потребления нет.
	api.setEnergy("", "", http.StatusBadGateway)
	c.pollEnergy(ctx, now.Add(48*time.Hour))
	c.pollEnergy(ctx, now.Add(49*time.Hour))
	if profiles, usages := api.energyStats(); profiles != 4 || len(usages) != 0 {
		t.Fatalf("сбой: %d, %v", profiles, usages)
	}
	// Чужие свойства и пустой ответ не считаются профилем.
	api.setEnergy(`{"result":{"property":["somethingElse"]}}`, "", 0)
	c.pollEnergy(ctx, now.Add(50*time.Hour))
	if _, usages := api.energyStats(); slices.Contains(d.items(), "float/electricity_meter") || len(usages) != 0 {
		t.Fatalf("чужое свойство: %v, %v", d.items(), usages)
	}
}

// Показание общее на прибор получает одна камера, иначе потребление считалось бы дважды.
func TestEnergyOneCamera(t *testing.T) {
	c, api, first := energyDevice(t, "dev-3", "FRIDGE")
	second := attachDevice(t, c, api, "dev-3", "FREEZER")
	api.setEnergy(energyProfileBody, energyUsageBody, 0)
	c.pollEnergy(context.Background(), time.Date(2026, 3, 15, 12, 0, 0, 0, time.Local))
	if !has(first.items(), "float/electricity_meter") || slices.Contains(second.items(), "float/electricity_meter") {
		t.Fatalf("камеры: %v, %v", first.items(), second.items())
	}
	if _, usages := api.energyStats(); len(usages) != 1 {
		t.Fatalf("запросов: %v", usages)
	}
}

func TestEnergyTowerParts(t *testing.T) {
	m, err := parseProfile("DEVICE_WASHTOWER", []byte(towerProfile), "dryer")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.energyProperty([]string{"energyUsage_washer", "energyUsage_dryer"}); got != "energyUsage_dryer" {
		t.Fatalf("часть: %q", got)
	}
	if got := m.energyProperty([]string{"energyUsage"}); got != "" {
		t.Fatalf("общее у второй части: %q", got)
	}
}

// Имя значения в профиле то же, что у шлюза: иначе показание не дойдёт до Алисы.
func TestEnergyKeyInProfiles(t *testing.T) {
	p, err := profile.Resolve("DEVICE_DRYER", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(p.Properties, func(pr profile.Property) bool {
		return pr.Instance == "electricity_meter" && pr.Status == EnergyKey
	}) {
		t.Fatalf("свойства: %+v", p.Properties)
	}
}

// Стиральная машина: умения, кнопка и команды не меняются, а показание добавляется только с профилем
// энергии.
func TestWasherUnchangedByEnergy(t *testing.T) {
	c, api, d := energyDevice(t, "dev-1", "")
	check := func(when string, want []string) {
		t.Helper()
		if got := d.items(); !slices.Equal(got, want) {
			t.Fatalf("%s: умения %v", when, got)
		}
		button := d.params("event/button")
		if !strings.Contains(button, "click") || !strings.Contains(button, "long_press") {
			t.Fatalf("%s: кнопка %s", when, button)
		}
		start := `{"location":{"locationName":"MAIN"},"operation":{"washerOperationMode":"START"}}`
		if code, body := d.act(alice.CapOnOff, "on", true); code != "" || strings.TrimPrefix(body, "dev-1 ") != start {
			t.Fatalf("%s: запуск %q %s", when, code, body)
		}
		d.a.apply(json.RawMessage(`{"location":{"locationName":"MAIN"},"runState":{"currentState":"RUNNING"}}`), false)
		stop := `{"location":{"locationName":"MAIN"},"operation":{"washerOperationMode":"STOP"}}`
		if code, body := d.act(alice.CapOnOff, "on", false); code != "" || strings.TrimPrefix(body, "dev-1 ") != stop {
			t.Fatalf("%s: остановка %q %s", when, code, body)
		}
		d.a.apply(json.RawMessage(`{"location":{"locationName":"MAIN"},"runState":{"currentState":"INITIAL"}}`), false)
	}
	check("без профиля энергии", []string{"event/button", "on_off"})
	api.setEnergy(energyProfileBody, energyUsageBody, 0)
	c.pollEnergy(context.Background(), time.Now().Add(48*time.Hour))
	check("с профилем энергии", []string{"event/button", "float/electricity_meter", "on_off"})
}
