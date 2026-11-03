package hcdevice

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/metrics"
	"hcgateway/internal/profile"
)

func newDevice(t *testing.T, id, kind string) *Device {
	t.Helper()
	a, err := appliance.NewDemo(kind, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.Resolve(kind, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Device{ID: id, Name: id, Room: "Кухня", Profile: p, Appliance: a}
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

func TestRunEvents(t *testing.T) {
	d := newDevice(t, "oven", "Oven")
	m := manual{Demo: d.Appliance.(*appliance.Demo), notify: make(chan appliance.Notify, 1)}
	d.Appliance = m
	got := make(chan []alice.DeviceState, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, func(events []alice.DeviceState) { got <- events })
	notify := <-m.notify

	// Окончание программы - событие датчика, которое нужно отправить сразу.
	events := func() float64 {
		return metrics.Value("hcgateway_device_events_total", "device", "oven", "event", appliance.EventProgramFinished)
	}
	before := events()
	notify([]string{appliance.EventProgramFinished})
	raw, _ := json.Marshal(<-got)
	want := `[{"id":"oven.events","properties":[{"type":"devices.properties.event","state":{"instance":"button","value":"click"}}]}]`
	if string(raw) != want {
		t.Fatalf("событие: %s", raw)
	}
	if events() != before+1 {
		t.Fatal("событие не посчитано в метриках")
	}
	// Изменение без событий датчика провайдер отправит сам после паузы.
	notify(nil)
	if events := <-got; len(events) != 0 {
		t.Fatalf("срочных состояний %d", len(events))
	}
	// Последнее событие датчик помнит и в обычном состоянии.
	if st := d.States(); len(st) != 2 || len(st[1].Properties) != 1 {
		t.Fatalf("состояния %+v", st)
	}
}

func TestActSensor(t *testing.T) {
	d := newDevice(t, "oven", "Oven")
	a := alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: true}}
	if c := d.Act(context.Background(), "oven.events", &a); c != alice.CodeInvalidAction {
		t.Fatalf("команда датчику: %q", c)
	}
	if d.Appliance.State().Running() {
		t.Fatal("прибор не должен получить команду")
	}
}

func TestDeviceWithoutEvents(t *testing.T) {
	d := newDevice(t, "hood", "Oven")
	p, err := profile.Resolve("Oven", nil, &profile.Profile{Events: &profile.Events{}})
	if err != nil {
		t.Fatal(err)
	}
	d.Profile = p
	if ids := d.IDs(); len(ids) != 1 || ids[0] != "hood" {
		t.Fatalf("идентификаторы %v", ids)
	}
	if states := d.States(); len(states) != 1 || states[0].ID != "hood" {
		t.Fatalf("состояния %+v", states)
	}
	if infos := d.Info(); len(infos) != 1 {
		t.Fatalf("устройств %d, датчик событий не нужен", len(infos))
	}
}

// props - прибор со свойствами, как прибор LG: настройки меняются сразу.
type props struct {
	desc   appliance.Description
	state  appliance.State
	sets   []string
	notify chan appliance.Notify
}

func (p *props) Describe() appliance.Description { return p.desc }
func (p *props) State() appliance.State          { return p.state }
func (p *props) Run(ctx context.Context, notify appliance.Notify) {
	p.notify <- notify
	<-ctx.Done()
}
func (p *props) Select(context.Context, string) error { return appliance.ErrNotSupported }
func (p *props) Start(_ context.Context, program string, _ map[string]any) error {
	p.sets = append(p.sets, "start="+program)
	return nil
}
func (p *props) Stop(context.Context) error            { return appliance.ErrNotSupported }
func (p *props) Command(context.Context, string) error { return appliance.ErrNotSupported }
func (p *props) Set(_ context.Context, key string, value any) error {
	p.sets = append(p.sets, fmt.Sprintf("%s=%v", key, value))
	return nil
}

func newProps(values map[string]any) *props {
	return &props{notify: make(chan appliance.Notify, 1), state: appliance.State{Online: true, Values: values},
		desc: appliance.Description{Type: "DEVICE_AIR_CONDITIONER", Options: map[string]appliance.Option{
			"operation.mode":       {Values: []string{"POWER_ON", "POWER_OFF"}, Setting: true},
			"airFlow.windStrength": {Values: []string{"LOW", "MID"}, Setting: true},
			"temp.target":          {Min: 18, Max: 30, Step: 1, Setting: true},
			"wind.swing":           {Setting: true},
			"light":                {Values: []string{"ON", "OFF"}, Setting: true},
		}}}
}

func act(t *testing.T, d *Device, typ, instance string, value any, relative bool) string {
	t.Helper()
	return d.Act(context.Background(), d.ID, &alice.Action{Type: typ,
		State: alice.ActionState{Instance: instance, Value: value, Relative: relative}})
}

func TestPowerProperty(t *testing.T) {
	a := newProps(map[string]any{"operation.mode": "POWER_ON", "airFlow.windStrength": "MID", "temp.target": 22.0,
		"wind.swing": false, "light": "ON"})
	p, err := profile.Resolve("X", &profile.Profile{YandexType: "thermostat.ac", OnOff: "power",
		Power: &profile.Power{Setting: "operation.mode", On: "POWER_ON", Off: "POWER_OFF"},
		Modes: []profile.Mode{{Instance: "fan_speed", Option: "airFlow.windStrength",
			Values: map[string]string{"LOW": "low", "MID": "medium", "HIGH": "high"}}},
		Ranges: []profile.Range{{Instance: "temperature", Option: "temp.target"}},
		Toggles: []profile.Toggle{{Instance: "oscillation", Setting: "wind.swing"},
			{Instance: "backlight", Setting: "light", On: "ON", Off: "OFF"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "ac", Name: "Кондиционер", Profile: p, Appliance: a, Manufacturer: "LG"}
	info := d.Info()[0]
	modes := info.Capabilities[1].Parameters.(alice.ModeParams).Modes
	if info.DeviceInfo.Manufacturer != "LG" || len(info.Capabilities) != 5 || len(modes) != 2 {
		t.Fatalf("устройство: %+v", info)
	}
	raw, _ := json.Marshal(d.States()[0].Capabilities)
	want := `[{"type":"devices.capabilities.on_off","state":{"instance":"on","value":true}},` +
		`{"type":"devices.capabilities.mode","state":{"instance":"fan_speed","value":"medium"}},` +
		`{"type":"devices.capabilities.range","state":{"instance":"temperature","value":22}},` +
		`{"type":"devices.capabilities.toggle","state":{"instance":"oscillation","value":false}},` +
		`{"type":"devices.capabilities.toggle","state":{"instance":"backlight","value":true}}]`
	if string(raw) != want {
		t.Fatalf("состояние: %s", raw)
	}
	// Включённый прибор «включи» не трогает; настройки меняются сразу, без программы.
	for _, c := range []struct {
		typ, instance string
		value         any
		relative      bool
	}{
		{alice.CapOnOff, "on", true, false},
		{alice.CapOnOff, "on", false, false},
		{alice.CapMode, "fan_speed", "low", false},
		{alice.CapRange, "temperature", 1.0, true},
		{alice.CapToggle, "backlight", false, false},
		{alice.CapToggle, "oscillation", true, false},
	} {
		if code := act(t, d, c.typ, c.instance, c.value, c.relative); code != "" {
			t.Fatalf("%s %s: %s", c.typ, c.instance, code)
		}
	}
	if want := []string{"operation.mode=POWER_OFF", "airFlow.windStrength=LOW", "temp.target=23", "light=OFF", "wind.swing=true"}; !slices.Equal(a.sets, want) {
		t.Fatalf("команды: %v", a.sets)
	}
}

func TestPowerState(t *testing.T) {
	a := newProps(map[string]any{"run.state": "INITIAL", "remote.ok": true})
	p, err := profile.Resolve("X", &profile.Profile{YandexType: "washing_machine", OnOff: "power",
		Power: &profile.Power{Setting: "operation.mode", On: "START", Off: "STOP", State: "run.state",
			Idle: []string{"INITIAL", "END"}, Allowed: "remote.ok"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "washer", Profile: p, Appliance: a}
	if v := d.States()[0].Capabilities[0].State.(alice.StateValue).Value; v != false {
		t.Fatalf("до стирки: %v", v)
	}
	if code := act(t, d, alice.CapOnOff, "on", true, false); code != "" || !slices.Equal(a.sets, []string{"operation.mode=START"}) {
		t.Fatalf("запуск: %q %v", code, a.sets)
	}
	a.state.Values = map[string]any{"run.state": "RUNNING", "remote.ok": true}
	if v := d.States()[0].Capabilities[0].State.(alice.StateValue).Value; v != true {
		t.Fatalf("стирка идёт: %v", v)
	}
	a.state.Values = map[string]any{"run.state": "END", "remote.ok": false}
	if code := act(t, d, alice.CapOnOff, "on", true, false); code != alice.CodeRemoteDisabled || len(a.sets) != 1 {
		t.Fatalf("без удалённого управления: %q %v", code, a.sets)
	}
}

func TestTransitions(t *testing.T) {
	a := newProps(map[string]any{"run.state": "END"})
	p, err := profile.Resolve("X", &profile.Profile{YandexType: "washing_machine",
		Properties: []profile.Property{{Type: "event", Instance: "open", Status: "door",
			Values: map[string]string{"true": "opened", "false": "closed"}}},
		Events: &profile.Events{Button: map[string]string{"run.state=END": "click", "*_IS_COMPLETE": "double_click",
			"*": "long_press"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "washer", Profile: p, Appliance: a}
	got := make(chan []alice.DeviceState, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, func(events []alice.DeviceState) { got <- events })
	notify := <-a.notify
	button := func() string {
		t.Helper()
		ev := <-got
		if len(ev) == 0 {
			return ""
		}
		return ev[0].Properties[0].State.(alice.StateValue).Value.(string)
	}
	// Прибор уже в END при запуске: событие не повторяется.
	notify(nil)
	if b := button(); b != "" {
		t.Fatalf("событие при запуске: %s", b)
	}
	for _, c := range []struct {
		state  string
		events []string
		want   string
	}{
		{"RUNNING", nil, ""},
		{"END", nil, "click"},
		{"END", nil, ""},
		{"END", []string{"WASHING_IS_COMPLETE"}, "double_click"},
		{"END", []string{"WATER_IS_FULL"}, "long_press"},
	} {
		a.state.Values = map[string]any{"run.state": c.state, "door": true}
		notify(c.events)
		if b := button(); b != c.want {
			t.Fatalf("%s %v: %q", c.state, c.events, b)
		}
	}
	// Флаг как значение события: true - opened.
	if st := d.States()[0]; len(st.Properties) != 1 || st.Properties[0].State.(alice.StateValue).Value != "opened" {
		t.Fatalf("свойство: %+v", st)
	}
}

// Кофемашина De'Longhi: «включи» выключенной - питание, включённой - напиток по умолчанию,
// режим coffee_mode - напиток.
func TestCoffeeDrink(t *testing.T) {
	a := newProps(map[string]any{"DeLonghi.Status.CurrMode": 0.0})
	a.desc = appliance.Description{Type: profile.DeLonghiType, Programs: []string{"DeLonghi.Recipe.1", "DeLonghi.Recipe.6", "DeLonghi.Recipe.15"},
		Options: map[string]appliance.Option{"DeLonghi.Setting.TurnOnOff": {Values: []string{"1", "2"}, Setting: true}}}
	p, err := profile.Resolve(profile.DeLonghiType, nil, profile.DeLonghiLayer(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "coffee", Profile: p, Appliance: a}
	if code := act(t, d, alice.CapOnOff, "on", true, false); code != "" {
		t.Fatal(code)
	}
	a.state.Values["DeLonghi.Status.CurrMode"] = 7.0
	for _, c := range []struct {
		typ, instance string
		value         any
	}{{alice.CapOnOff, "on", true}, {alice.CapMode, "coffee_mode", "cappuccino"}, {alice.CapOnOff, "on", false}} {
		if code := act(t, d, c.typ, c.instance, c.value, false); code != "" {
			t.Fatalf("%s %v: %s", c.instance, c.value, code)
		}
	}
	want := []string{"DeLonghi.Setting.TurnOnOff=2", "start=DeLonghi.Recipe.6", "start=DeLonghi.Recipe.15", "DeLonghi.Setting.TurnOnOff=1"}
	if !slices.Equal(a.sets, want) {
		t.Fatalf("команды: %v", a.sets)
	}
}

// Своё свойство оставшегося времени: вне программы - 0, в программе - значение прибора.
// Новое значение Яндекс видит не чаще раза в 55 с, а начало и конец программы - сразу.
func TestRemaining(t *testing.T) {
	const key = "BSH.Common.Option.RemainingProgramTime"
	a := newProps(map[string]any{appliance.StatusOperationState: appliance.OperationReady, key: 5400.0})
	p, err := profile.Resolve("Dishwasher", nil, &profile.Profile{Properties: []profile.Property{
		{Type: "float", Instance: RemainingTime, Status: key, Unit: "unit.time.seconds"}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(0, 0)
	d := &Device{ID: "dish", Profile: p, Appliance: a, now: func() time.Time { return now }}
	get := func() (any, bool) {
		for _, pr := range d.States()[0].Properties {
			if sv := pr.State.(alice.StateValue); sv.Instance == RemainingTime {
				return sv.Value, true
			}
		}
		return nil, false
	}
	// Время выбранной, ещё не запущенной программы Алиса не видит.
	if v, ok := get(); !ok || v != 0.0 {
		t.Fatalf("до запуска: %v %v", v, ok)
	}
	a.state.Values[appliance.StatusOperationState] = appliance.OperationRun
	if v, _ := get(); v != 5400.0 {
		t.Fatalf("запуск: %v", v)
	}
	now = now.Add(30 * time.Second)
	a.state.Values[key] = 5370.0
	if v, _ := get(); v != 5400.0 {
		t.Fatalf("новое время раньше 55 с: %v", v)
	}
	now = now.Add(30 * time.Second)
	if v, _ := get(); v != 5370.0 {
		t.Fatalf("через минуту: %v", v)
	}
	a.state.Values[appliance.StatusOperationState] = appliance.OperationFinished
	if v, _ := get(); v != 0.0 {
		t.Fatalf("конец программы: %v", v)
	}
	// Программа идёт, а время прибор не прислал: свойства в состоянии нет.
	delete(a.state.Values, key)
	a.state.Values[appliance.StatusOperationState] = appliance.OperationRun
	if v, ok := get(); ok {
		t.Fatalf("без времени прибора: %v", v)
	}
}

// Своё свойство с пересчётом: прибор сообщает минуты, Алиса получает секунды; оставшееся
// время вне программы - 0 и после пересчёта.
func TestConvertedProperty(t *testing.T) {
	a := newProps(map[string]any{"run.state": "RUNNING", "timer.min": 85.0, "temp.f": 70.0})
	p, err := profile.Resolve("X", &profile.Profile{YandexType: "washing_machine", OnOff: "power",
		Power: &profile.Power{Setting: "operation.mode", On: "START", Off: "STOP", State: "run.state", Idle: []string{"END"}},
		Properties: []profile.Property{
			{Type: "float", Instance: RemainingTime, Status: "timer.min", Unit: "unit.time.seconds", Convert: "minutes_to_seconds"},
			{Type: "float", Instance: "temperature", Status: "temp.f", Unit: "unit.temperature.celsius", Convert: "fahrenheit_to_celsius"},
		}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "washer", Profile: p, Appliance: a}
	raw, _ := json.Marshal(d.States()[0].Properties)
	want := `[{"type":"devices.properties.float","state":{"instance":"remaining_time","value":5100}},` +
		`{"type":"devices.properties.float","state":{"instance":"temperature","value":21.11}}]`
	if string(raw) != want {
		t.Fatalf("свойства: %s", raw)
	}
	a.state.Values["run.state"] = "END"
	if v, ok := Remaining(p, &a.state); !ok || v != 0 {
		t.Fatalf("после стирки: %v %v", v, ok)
	}
}

// Температура духовки с номером камеры в имени доходит до Алисы по встроенному профилю.
func TestOvenCavityTemperature(t *testing.T) {
	const other = "Cooking.Oven.Status.Cavity.001.CurrentTemperature"
	a := newProps(map[string]any{other: 91.0})
	p, err := profile.Resolve("Oven", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "oven", Profile: p, Appliance: a}
	raw, _ := json.Marshal(d.States()[0].Properties)
	if want := `{"type":"devices.properties.float","state":{"instance":"temperature","value":91}}`; !strings.Contains(string(raw), want) {
		t.Fatalf("свойства: %s", raw)
	}
}

// Нет соли - low, как и «соль скоро закончится»; оба события Off - normal.
func TestDishwasherFoodLevel(t *testing.T) {
	const off, present = "BSH.Common.EnumType.EventPresentState.Off", appliance.EventPresent
	level := func(values map[string]any) string {
		t.Helper()
		p, err := profile.Resolve("Dishwasher", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := &Device{ID: "dish", Profile: p, Appliance: newProps(values)}
		raw, _ := json.Marshal(d.States())
		for _, l := range []string{"low", "normal"} {
			if strings.Contains(string(raw), `"instance":"food_level","value":"`+l+`"`) {
				return l
			}
		}
		t.Fatalf("нет food_level: %s", raw)
		return ""
	}
	if got := level(map[string]any{"Dishcare.Dishwasher.Event.SaltNearlyEmpty": off, "Dishcare.Dishwasher.Event.SaltLack": off}); got != "normal" {
		t.Errorf("всё есть: %s", got)
	}
	for _, event := range []string{"SaltLack", "RinseAidLack", "SaltNearlyEmpty", "RinseAidNearlyEmpty"} {
		if got := level(map[string]any{"Dishcare.Dishwasher.Event." + event: present}); got != "low" {
			t.Errorf("%s: %s", event, got)
		}
	}
}

// Подсветка камеры: есть, если прибор описывает настройку и разрешает её менять.
func TestOvenBacklight(t *testing.T) {
	const light = "Cooking.Oven.Setting.Light.Cavity.001.Power"
	has := func(opts map[string]appliance.Option) bool {
		t.Helper()
		a := newProps(map[string]any{light: true})
		a.desc.Options = opts
		p, err := profile.Resolve("Oven", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := &Device{ID: "oven", Profile: p, Appliance: a}
		for _, c := range d.Info()[0].Capabilities {
			if ip, ok := c.Parameters.(alice.InstanceParams); ok && c.Type == alice.CapToggle && ip.Instance == "backlight" {
				return true
			}
		}
		return false
	}
	if !has(map[string]appliance.Option{light: {Key: light, Setting: true}}) {
		t.Error("настройку можно менять, а подсветки нет")
	}
	if has(map[string]appliance.Option{light: {Key: light, Setting: true, ReadOnly: true}}) {
		t.Error("подсветка только для чтения")
	}
	if has(map[string]appliance.Option{}) {
		t.Error("подсветка у прибора без такой настройки")
	}
}

// Вода духовки: уровень есть, только если прибор описывает эти события.
func TestOvenWater(t *testing.T) {
	const fill = "Cooking.Oven.Event.Cavity.001.FillWaterContainer"
	p, err := profile.Resolve("Oven", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	water := func(events []string, values map[string]any) (bool, string) {
		t.Helper()
		a := newProps(values)
		a.desc.Events = events
		d := &Device{ID: "oven", Profile: p, Appliance: a}
		info, _ := json.Marshal(d.Info())
		raw, _ := json.Marshal(d.States())
		state := ""
		for _, l := range []string{"low", "normal"} {
			if strings.Contains(string(raw), `"instance":"food_level","value":"`+l+`"`) {
				state = l
			}
		}
		return strings.Contains(string(info), `"instance":"food_level"`), state
	}
	if has, state := water(nil, map[string]any{}); has || state != "" {
		t.Errorf("прибор без этих событий: %v %q", has, state)
	}
	if has, state := water([]string{fill}, map[string]any{fill: "BSH.Common.EnumType.EventPresentState.Off"}); !has || state != "normal" {
		t.Errorf("вода есть: %v %q", has, state)
	}
	if has, state := water([]string{fill}, map[string]any{fill: appliance.EventPresent}); !has || state != "low" {
		t.Errorf("нужно долить: %v %q", has, state)
	}
}

// Новые изменения уходят в журнал, сохранённая история видна сразу, а первое состояние только запоминается.
func TestHistoryJournal(t *testing.T) {
	saved := Change{Time: time.Now(), Kind: "value", Key: "door", Value: "open"}
	var got []Change
	d := &Device{Journal: func(c []Change) { got = append(got, c...) }}
	d.Restore([]Change{saved})
	st := appliance.State{Online: true, Values: map[string]any{"door": "closed"}}
	d.record(&st, nil)
	if len(got) != 0 {
		t.Fatalf("первое состояние в журнале: %+v", got)
	}
	st.Values = map[string]any{"door": "open"}
	d.record(&st, []string{"done"})
	if len(got) != 2 || got[0].Key != "door" || got[1].Kind != "event" {
		t.Fatalf("журнал: %+v", got)
	}
	if h := d.History(); len(h) != 3 || h[2].Key != saved.Key {
		t.Fatalf("история: %+v", h)
	}
}
