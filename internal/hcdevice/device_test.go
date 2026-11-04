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

	// Окончание программы - нажатие кнопки, которое нужно отправить сразу.
	events := func() float64 {
		return metrics.Value("hcgateway_device_events_total", "device", "oven", "event", appliance.EventProgramFinished)
	}
	before := events()
	notify([]string{appliance.EventProgramFinished})
	raw, _ := json.Marshal(<-got)
	want := `[{"id":"oven","properties":[{"type":"devices.properties.event","state":{"instance":"button","value":"click"}}]}]`
	if string(raw) != want {
		t.Fatalf("событие: %s", raw)
	}
	if events() != before+1 {
		t.Fatal("событие не посчитано в метриках")
	}
	// Изменение без нажатий провайдер отправит сам после паузы.
	notify(nil)
	if events := <-got; len(events) != 0 {
		t.Fatalf("срочных состояний %d", len(events))
	}
	// Последнее нажатие прибор помнит и в обычном состоянии.
	st := d.States()
	if len(st) != 1 || !slices.ContainsFunc(st[0].Properties, func(c alice.CapState) bool {
		return c.State == alice.StateValue{Instance: alice.InstanceButton, Value: "click"}
	}) {
		t.Fatalf("состояния %+v", st)
	}
}

func TestActOtherID(t *testing.T) {
	d := newDevice(t, "oven", "Oven")
	a := alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: true}}
	if c := d.Act(context.Background(), "other", &a); c != alice.CodeInvalidAction {
		t.Fatalf("команда чужому устройству: %q", c)
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
	infos := d.Info()
	if len(infos) != 1 {
		t.Fatalf("устройств %d", len(infos))
	}
	for _, c := range infos[0].Properties {
		if params, ok := c.Parameters.(alice.EventParams); ok && (params.Instance == alice.InstanceButton || params.Instance == "food_level" || params.Instance == "water_level") {
			t.Fatalf("раздел events пуст, а есть свойство %s", params.Instance)
		}
	}
}

// props - прибор со свойствами, как прибор LG: настройки меняются сразу.
type props struct {
	desc  appliance.Description
	state appliance.State
	sets  []string
	// options - параметры последнего запуска.
	options map[string]any
	notify  chan appliance.Notify
}

func (p *props) Describe() appliance.Description { return p.desc }
func (p *props) State() appliance.State          { return p.state }
func (p *props) Run(ctx context.Context, notify appliance.Notify) {
	p.notify <- notify
	<-ctx.Done()
}
func (p *props) Select(context.Context, string) error { return appliance.ErrNotSupported }
func (p *props) Start(_ context.Context, program string, options map[string]any) error {
	p.sets = append(p.sets, "start="+program)
	p.options = options
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
	if st := d.States()[0]; !slices.ContainsFunc(st.Properties, func(c alice.CapState) bool {
		return c.State == alice.StateValue{Instance: "open", Value: "opened"}
	}) {
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
			if strings.Contains(string(raw), `"instance":"water_level","value":"`+l+`"`) {
				state = l
			}
		}
		return strings.Contains(string(info), `"instance":"water_level"`), state
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

// Посудомойка: соль - food_level, ополаскиватель - water_level, а «уже нет» - empty.
func TestDishwasherLevels(t *testing.T) {
	const present = appliance.EventPresent
	p, err := profile.Resolve("Dishwasher", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := func(values map[string]any) string {
		d := &Device{ID: "dish", Profile: p, Appliance: newProps(values)}
		raw, _ := json.Marshal(d.States())
		return string(raw)
	}
	has := func(raw, instance, value string) bool {
		return strings.Contains(raw, `"instance":"`+instance+`","value":"`+value+`"`)
	}
	if raw := state(map[string]any{}); !has(raw, "food_level", "normal") || !has(raw, "water_level", "normal") {
		t.Fatalf("всё есть: %s", raw)
	}
	if raw := state(map[string]any{"Dishcare.Dishwasher.Event.SaltNearlyEmpty": present}); !has(raw, "food_level", "low") || !has(raw, "water_level", "normal") {
		t.Fatalf("соль кончается: %s", raw)
	}
	if raw := state(map[string]any{"Dishcare.Dishwasher.Event.SaltNearlyEmpty": present, "Dishcare.Dishwasher.Event.SaltLack": present}); !has(raw, "food_level", "empty") {
		t.Fatalf("соли нет: %s", raw)
	}
	if raw := state(map[string]any{"Dishcare.Dishwasher.Event.RinseAidNearlyEmpty": present}); !has(raw, "water_level", "low") || !has(raw, "food_level", "normal") {
		t.Fatalf("ополаскиватель кончается: %s", raw)
	}
	if raw := state(map[string]any{"Dishcare.Dishwasher.Event.RinseAidLack": present}); !has(raw, "water_level", "empty") {
		t.Fatalf("ополаскивателя нет: %s", raw)
	}
	d := &Device{ID: "dish", Profile: p, Appliance: newProps(map[string]any{})}
	info, _ := json.Marshal(d.Info())
	if !strings.Contains(string(info), `"instance":"water_level","events":[{"value":"empty"},{"value":"low"},{"value":"normal"}]`) {
		t.Fatalf("устройство: %s", info)
	}
}

// Кнопка самого прибора: каждое нажатие - отдельное срочное состояние, даже с тем же значением. Переход
// значения и событие источника об одном нажатии не удваивают его. У кнопки нет запроса состояния.
func TestButtonProperty(t *testing.T) {
	a := newProps(map[string]any{"button.button": "pushed"})
	p, err := profile.Resolve("X", &profile.Profile{YandexType: "sensor.button", Properties: []profile.Property{
		{Type: "event", Instance: alice.InstanceButton, Status: "button.button", Values: map[string]string{"pushed": "click", "held": "long_press"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "b", Profile: p, Appliance: a}
	got := make(chan []alice.DeviceState, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, func(events []alice.DeviceState) { got <- events })
	notify := <-a.notify
	notify(nil)
	if ev := <-got; len(ev) != 0 {
		t.Fatalf("при запуске: %+v", ev)
	}
	const click = `[{"id":"b","properties":[{"type":"devices.properties.event","state":{"instance":"button","value":"click"}}]}]`
	for range 2 {
		notify([]string{"button.button=pushed"})
		if raw, _ := json.Marshal(<-got); string(raw) != click {
			t.Fatalf("нажатие: %s", raw)
		}
	}
	a.state.Values = map[string]any{"button.button": "held"}
	notify([]string{"button.button=held"})
	if ev := <-got; len(ev) != 1 || ev[0].Properties[0].State.(alice.StateValue).Value != "long_press" {
		t.Fatalf("долгое нажатие: %+v", ev)
	}
	if info := d.Info(); info[0].Properties[0].Retrievable || !info[0].Properties[0].Reportable {
		t.Fatalf("описание кнопки: %+v", info[0].Properties[0])
	}
}

// Тревоги кофемашины: тревога, которая была при запуске, события не даёт, появление тревоги - нажатие
// double_click, повторное сообщение о ней - нет. Снятый бак - уровень воды empty, без нажатия.
func TestCoffeeAlarms(t *testing.T) {
	const (
		waste = "DeLonghi.Alarm.Waste"
		tank  = "DeLonghi.Alarm.TankPresence"
	)
	a := newProps(map[string]any{"DeLonghi.Status.CurrMode": 7.0, waste: appliance.EventPresent, tank: appliance.EventOff})
	a.desc = appliance.Description{Type: profile.DeLonghiType}
	p, err := profile.Resolve(profile.DeLonghiType, nil, profile.DeLonghiLayer(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "coffee", Profile: p, Appliance: a}
	got := make(chan []alice.DeviceState, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, func(events []alice.DeviceState) { got <- events })
	notify := <-a.notify
	quiet := func(what string) {
		t.Helper()
		notify(nil)
		if ev := <-got; len(ev) != 0 {
			t.Fatalf("%s: %+v", what, ev)
		}
	}
	quiet("тревога при запуске")
	a.state.Values[waste] = appliance.EventOff
	quiet("тревога прошла")
	a.state.Values[waste] = appliance.EventPresent
	notify(nil)
	const press = `[{"id":"coffee","properties":[{"type":"devices.properties.event","state":{"instance":"button","value":"double_click"}}]}]`
	if raw, _ := json.Marshal(<-got); string(raw) != press {
		t.Fatalf("появление тревоги: %s", raw)
	}
	quiet("повторное сообщение о той же тревоге")
	level := func() any {
		t.Helper()
		for _, c := range d.States()[0].Properties {
			if sv := c.State.(alice.StateValue); sv.Instance == "water_level" {
				return sv.Value
			}
		}
		return nil
	}
	if got := level(); got != "normal" {
		t.Fatalf("уровень воды с баком: %v", got)
	}
	a.state.Values[tank] = appliance.EventPresent
	quiet("снятый бак")
	if got := level(); got != "empty" {
		t.Fatalf("уровень воды без бака: %v", got)
	}
}

// AllInOne: одно устройство, все свойства из документации Яндекса в списке и состоянии,
// все три нажатия кнопки уходят от него самого.
func TestAllInOne(t *testing.T) {
	a, err := appliance.NewDemo(appliance.DemoAllInOne, 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.Resolve(profile.AllInOneType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "all", Name: "all", Room: "Кухня", Profile: p, Appliance: a}
	if ids := d.IDs(); len(ids) != 1 || ids[0] != "all" {
		t.Fatalf("устройства %v, ждём одно", ids)
	}
	info := d.Info()
	if len(info) != 1 || len(info[0].Properties) != 30 || len(info[0].Capabilities) != 4 {
		t.Fatalf("описание: %+v", info)
	}
	states := d.States()
	// До первого нажатия кнопке сообщать нечего.
	if len(states) != 1 || len(states[0].Properties) != 29 {
		t.Fatalf("состояние: %+v", states)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	presses := make(chan string, 16)
	go d.Run(ctx, func(changed []alice.DeviceState) {
		for _, s := range changed {
			for _, pr := range s.Properties {
				if v, ok := pr.State.(alice.StateValue); ok && v.Instance == alice.InstanceButton {
					presses <- s.ID + " " + fmt.Sprint(v.Value)
				}
			}
		}
	})
	time.Sleep(100 * time.Millisecond)
	if err := a.Start(ctx, "Demo.Program.One", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"all double_click", "all long_press", "all click"} {
		select {
		case got := <-presses:
			if got != want {
				t.Fatalf("нажатие %q, ждём %q", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("нет нажатия %q", want)
		}
	}
}

// eventProps - свойства event с этим instance в описании устройства.
func eventProps(info alice.DeviceInfo, instance string) []alice.Capability {
	var out []alice.Capability
	for _, c := range info.Properties {
		if params, ok := c.Parameters.(alice.EventParams); ok && params.Instance == instance {
			out = append(out, c)
		}
	}
	return out
}

// Кнопка и уровни раздела events - свойства самого прибора: кнопка приходит только уведомлением,
// уровень можно запросить. Отдельного устройства нет.
func TestEventsInDevice(t *testing.T) {
	d := newDevice(t, "dish", "Dishwasher")
	if ids := d.IDs(); !slices.Equal(ids, []string{"dish"}) {
		t.Fatalf("устройства %v", ids)
	}
	info := d.Info()
	if len(info) != 1 || info[0].Type != alice.TypePrefix+"dishwasher" {
		t.Fatalf("описание: %+v", info)
	}
	button := eventProps(info[0], alice.InstanceButton)
	if len(button) != 1 || button[0].Retrievable || !button[0].Reportable {
		t.Fatalf("кнопка: %+v", button)
	}
	for _, instance := range []string{"food_level", "water_level"} {
		level := eventProps(info[0], instance)
		if len(level) != 1 || !level[0].Retrievable || !level[0].Reportable {
			t.Fatalf("%s: %+v", instance, level)
		}
	}
	states := d.States()
	if len(states) != 1 || !slices.ContainsFunc(states[0].Properties, func(c alice.CapState) bool {
		return c.State == alice.StateValue{Instance: "food_level", Value: "normal"}
	}) {
		t.Fatalf("состояние: %+v", states)
	}
}

// Свойство из properties главнее раздела events: одно свойство в устройстве и одно нажатие на событие.
func TestOwnPropertyOverEvents(t *testing.T) {
	d := newDevice(t, "dish", "Dishwasher")
	const rinse, finish = "Rinse.Status", "Finish.Status"
	p, err := profile.Resolve("Dishwasher", nil, &profile.Profile{Properties: []profile.Property{
		{Type: "event", Instance: "water_level", Status: rinse, Values: map[string]string{"empty": "empty"}},
		{Type: "event", Instance: alice.InstanceButton, Status: finish, Values: map[string]string{"done": "double_click"}},
	}, Events: &profile.Events{Button: map[string]string{finish + "=done": "click"}, FoodLevel: []string{"Salt"}, WaterLevel: []string{"Rinse"}}})
	if err != nil {
		t.Fatal(err)
	}
	d.Profile = p
	info := d.Info()[0]
	for _, instance := range []string{"water_level", "food_level", alice.InstanceButton} {
		if got := eventProps(info, instance); len(got) != 1 {
			t.Fatalf("%s: %+v", instance, got)
		}
	}
	if got := eventProps(info, alice.InstanceButton)[0].Parameters.(alice.EventParams).Events; len(got) != 1 || got[0].Value != "double_click" {
		t.Fatalf("значения кнопки: %+v", got)
	}
	if got := d.buttonFor([]string{finish + "=done"}); len(got) != 0 {
		t.Fatalf("нажатие из раздела events: %v", got)
	}
	if got := d.pressed([]string{finish + "=done"}); len(got) != 1 {
		t.Fatalf("нажатия свойства: %+v", got)
	}
	desc := d.Appliance.Describe()
	active := ActiveProfile(p, &desc)
	if active.Events.Button != nil || active.Events.WaterLevel != nil || len(active.Events.FoodLevel) == 0 {
		t.Fatalf("раздел events без перекрытого: %+v", active.Events)
	}
	if p.Events.Button == nil {
		t.Fatal("исходный профиль изменён")
	}
}

// «Включи» запускает программу, выбранную на панели, даже вне профиля, и не подставляет ей параметры
// по умолчанию. Программа по умолчанию нужна, только если ничего не выбрано.
func TestTurnOnSelected(t *testing.T) {
	const washer, cotton, dessous = "LaundryCare.Washer", "LaundryCare.Washer.Program.Cotton", "LaundryCare.Washer.Program.Dessous"
	p := &profile.Profile{YandexType: "washing_machine", OnOff: "program", DefaultProgram: cotton,
		DefaultOptions: map[string]any{"speed": 800.0},
		Modes:          []profile.Mode{{Instance: "program", Programs: map[string]string{cotton: "normal"}}}}
	for _, c := range []struct {
		selected, want string
		options        int
	}{{dessous, dessous, 0}, {cotton, cotton, 1}, {"", cotton, 1}} {
		a := newProps(map[string]any{appliance.StatusOperationState: appliance.OperationReady})
		a.desc = appliance.Description{Type: washer, Programs: []string{cotton, dessous}, Options: map[string]appliance.Option{"speed": {Min: 0, Max: 1400, Step: 200}}}
		a.state.SelectedProgram = c.selected
		d := &Device{ID: "w", Profile: p, Appliance: a}
		action := &alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: true}}
		if code := d.Act(context.Background(), "w", action); code != "" {
			t.Fatalf("выбрана %q: код %s", c.selected, code)
		}
		if !slices.Equal(a.sets, []string{"start=" + c.want}) || len(a.options) != c.options {
			t.Fatalf("выбрана %q: %v параметры %v", c.selected, a.sets, a.options)
		}
	}
}

// Программа, которая ждёт действия человека, идёт: Алиса видит прибор включённым.
func TestActionRequiredIsRunning(t *testing.T) {
	st := appliance.State{Values: map[string]any{appliance.StatusOperationState: appliance.OperationActionRequired}}
	if !st.Running() {
		t.Fatal("ActionRequired не считается работой")
	}
}

// Духовка без быстрого нагрева сообщает о конце разогрева другим событием.
func TestOvenRegularPreheat(t *testing.T) {
	p, err := profile.Resolve("Oven", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := p.Events.Match("Cooking.Oven.Event.RegularPreheatFinished"); !ok || v != "double_click" {
		t.Fatalf("кнопка: %q %v", v, ok)
	}
}

// Питание, которого прибор не сообщает, или только для чтения: on_off нет ни в описании, ни в состоянии.
func TestNoOnOffWithoutPower(t *testing.T) {
	p := &profile.Profile{YandexType: "other", OnOff: "power", Power: &profile.Power{Setting: "p", On: "1", Off: "0"},
		Properties: []profile.Property{{Type: "float", Instance: "humidity", Status: "h", Unit: "unit.percent"}}}
	hasOnOff := func(a *props) (bool, bool) {
		d := &Device{ID: "x", Profile: p, Appliance: a}
		info := d.Info()
		if len(info) != 1 {
			t.Fatalf("устройств: %d", len(info))
		}
		inInfo := slices.ContainsFunc(info[0].Capabilities, func(c alice.Capability) bool { return c.Type == alice.CapOnOff })
		inState := slices.ContainsFunc(d.States()[0].Capabilities, func(c alice.CapState) bool { return c.Type == alice.CapOnOff })
		return inInfo, inState
	}
	a := newProps(map[string]any{"h": 40.0})
	a.desc = appliance.Description{Reports: []string{"h"}}
	if i, s := hasOnOff(a); i || s {
		t.Fatalf("прибор не сообщает питание: %v %v", i, s)
	}
	a.desc = appliance.Description{Reports: []string{"h", "p"}, Options: map[string]appliance.Option{"p": {ReadOnly: true}}}
	if i, s := hasOnOff(a); i || s {
		t.Fatalf("питание только для чтения: %v %v", i, s)
	}
	a.desc = appliance.Description{Reports: []string{"h", "p"}}
	if i, s := hasOnOff(a); !i || !s {
		t.Fatalf("питание есть: %v %v", i, s)
	}
}

// Устройство без умений и свойств не уходит Яндексу.
func TestEmptyDeviceNotListed(t *testing.T) {
	p := &profile.Profile{YandexType: "other", OnOff: "none",
		Properties: []profile.Property{{Type: "float", Instance: "humidity", Status: "h", Unit: "unit.percent"}}}
	a := newProps(map[string]any{})
	a.desc = appliance.Description{Reports: []string{"t"}}
	if info := (&Device{ID: "x", Profile: p, Appliance: a}).Info(); len(info) != 0 {
		t.Fatalf("устройство: %+v", info)
	}
}

// Прибор перечисляет все события: кнопка объявляет только то, что он может вызвать.
func TestButtonReachable(t *testing.T) {
	p := &profile.Profile{YandexType: "other", OnOff: "none", Events: &profile.Events{Button: map[string]string{
		"DONE": "click", "PRE_*": "double_click", "state=END": "long_press"}}}
	values := func(desc appliance.Description) []string {
		return buttonValues(p.Events, &desc)
	}
	if got := values(appliance.Description{}); len(got) != 3 {
		t.Fatalf("список неизвестен: %v", got)
	}
	if got := values(appliance.Description{EventsKnown: true, Events: []string{"PRE_HEAT"}, Reports: []string{"other"}}); !slices.Equal(got, []string{"double_click"}) {
		t.Fatalf("известный список: %v", got)
	}
	if got := values(appliance.Description{EventsKnown: true, Reports: []string{"state"}}); !slices.Equal(got, []string{"long_press"}) {
		t.Fatalf("переход значения: %v", got)
	}
}

// Значение режима вне объявленного списка не уходит; снятие паузы настройкой - только с паузы; команда по
// свойству только для чтения у прибора без программ получает отказ.
func TestModeValuePauseReadOnly(t *testing.T) {
	p := &profile.Profile{YandexType: "other", OnOff: "none",
		Modes:   []profile.Mode{{Instance: "fan_speed", Option: "fan", Values: map[string]string{"LOW": "low", "AUTO": "auto"}}, {Instance: "swing", Option: "ro", Values: map[string]string{"A": "auto"}}},
		Toggles: []profile.Toggle{{Instance: "pause", Setting: "job", On: "PAUSE", Off: "RUN"}}}
	a := newProps(map[string]any{"fan": "AUTO", "job": "RUN"})
	a.desc = appliance.Description{Options: map[string]appliance.Option{
		"fan": {Values: []string{"LOW"}, Setting: true},
		"ro":  {Values: []string{"A"}},
		"job": {Values: []string{"PAUSE", "RUN"}, Setting: true},
	}}
	d := &Device{ID: "x", Profile: p, Appliance: a}
	for _, c := range d.States()[0].Capabilities {
		if sv, ok := c.State.(alice.StateValue); ok && sv.Instance == "fan_speed" {
			t.Fatalf("режим вне списка: %v", sv.Value)
		}
	}
	act := func(capability, instance string, value any) string {
		return d.Act(context.Background(), "x", &alice.Action{Type: capability, State: alice.ActionState{Instance: instance, Value: value}})
	}
	for _, job := range []string{"RUN", "STOP"} {
		a.state.Values["job"] = job
		if code := act(alice.CapToggle, "pause", false); code != "" {
			t.Fatalf("снятие паузы при %s: %q", job, code)
		}
	}
	if code := act(alice.CapMode, "swing", "auto"); code != alice.CodeInvalidAction {
		t.Fatalf("только для чтения: %q", code)
	}
	if len(a.sets) != 0 {
		t.Fatalf("команды: %v", a.sets)
	}
}

// fixedOptions - демо-прибор с заданными значениями настроек.
type fixedOptions struct {
	*appliance.Demo
	options map[string][]string
	values  map[string]any
	set     []string
}

func (f *fixedOptions) Describe() appliance.Description {
	desc := appliance.Description{Options: map[string]appliance.Option{}}
	for k, v := range f.options {
		desc.Options[k] = appliance.Option{Key: k, Setting: true, Values: v}
		desc.Reports = append(desc.Reports, k)
	}
	return desc
}

func (f *fixedOptions) State() appliance.State {
	return appliance.State{Online: true, Values: f.values}
}

func (f *fixedOptions) Set(_ context.Context, key string, value any) error {
	f.set = append(f.set, fmt.Sprint(key, "=", value))
	return nil
}

// Режим с запасной настройкой и другими значениями: список, состояние и команда берут то, что есть у
// этого прибора, и режим Алисы виден один раз.
func TestModeExtras(t *testing.T) {
	demo, err := appliance.NewDemo("Oven", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m := profile.Mode{Instance: "thermostat", Option: "main.mode", FallbackOption: "other.mode",
		Values: map[string]string{"auto": "auto", "wind": "fan_only"}, AlsoValues: map[string]string{"aIComfort": "auto", "fan": "fan_only"}}
	f := &fixedOptions{Demo: demo, options: map[string][]string{"other.mode": {"auto", "aIComfort", "fan"}}, values: map[string]any{"other.mode": "fan"}}
	d := &Device{ID: "ac", Name: "ac", Appliance: f, Profile: &profile.Profile{YandexType: "other", OnOff: "none", Modes: []profile.Mode{m}}}

	raw, _ := json.Marshal(d.Info()[0].Capabilities)
	if !strings.Contains(string(raw), `"modes":[{"value":"auto"},{"value":"fan_only"}]`) {
		t.Fatalf("режимы: %s", raw)
	}
	raw, _ = json.Marshal(d.States())
	if !strings.Contains(string(raw), `"instance":"thermostat","value":"fan_only"`) {
		t.Fatalf("состояние: %s", raw)
	}
	act := func(value string) {
		t.Helper()
		if c := d.Act(context.Background(), "ac", &alice.Action{Type: alice.CapMode, State: alice.ActionState{Instance: "thermostat", Value: value}}); c != "" {
			t.Fatalf("%s: %s", value, c)
		}
	}
	act("fan_only")
	act("auto")
	if !slices.Equal(f.set, []string{"other.mode=fan", "other.mode=auto"}) {
		t.Fatalf("команды: %v", f.set)
	}

	// Основная настройка со значениями важнее запасной.
	f.options["main.mode"] = []string{"wind"}
	f.values["main.mode"] = "wind"
	f.set = nil
	act("fan_only")
	if !slices.Equal(f.set, []string{"main.mode=wind"}) {
		t.Fatalf("основная настройка: %v", f.set)
	}
}
