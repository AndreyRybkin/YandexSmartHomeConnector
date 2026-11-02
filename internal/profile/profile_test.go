package profile

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"hcgateway/internal/samsungtv"
	"hcgateway/internal/tv"
	"hcgateway/internal/webos"
)

func TestBuiltinValid(t *testing.T) {
	for haType := range builtin {
		if _, err := Resolve(haType, nil, nil); err != nil {
			t.Errorf("%s: %v", haType, err)
		}
	}
}

func TestResolveOverride(t *testing.T) {
	forType := &Profile{DefaultProgram: "Cooking.Oven.Program.HeatingMode.PizzaSetting"}
	forDevice := &Profile{Modes: []Mode{{Instance: "program", Programs: map[string]string{
		"Cooking.Oven.Program.HeatingMode.HotAir": "one",
	}}}}
	p, err := Resolve("Oven", forType, forDevice)
	if err != nil {
		t.Fatal(err)
	}
	if p.DefaultProgram != "Cooking.Oven.Program.HeatingMode.PizzaSetting" {
		t.Fatalf("default_program %q", p.DefaultProgram)
	}
	// Заданный список заменяет встроенный целиком.
	if pm := p.ProgramMode(); len(pm.Programs) != 1 || pm.Programs["Cooking.Oven.Program.HeatingMode.HotAir"] != "one" {
		t.Fatalf("programs %v", pm.Programs)
	}
	if len(p.Ranges) != 1 || p.YandexType != "cooking" {
		t.Fatalf("остальное из встроенного: %+v", p)
	}
	// Переопределение не меняет встроенный профиль.
	if builtin["Oven"].DefaultProgram != "Cooking.Oven.Program.HeatingMode.HotAir" {
		t.Fatal("встроенный профиль изменился")
	}
}

func TestResolveUnknownType(t *testing.T) {
	if _, err := Resolve("Kettle", nil, nil); err == nil {
		t.Fatal("ожидалась ошибка: нет профиля")
	}
	p, err := Resolve("Kettle", &Profile{YandexType: "cooking", Toggles: []Toggle{{Instance: "pause"}}}, nil)
	if err != nil || p.OnOff != "none" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestValidateErrors(t *testing.T) {
	prog := map[string]string{"P": "auto"}
	cases := map[string]*Profile{
		"неизвестный режим":    {Modes: []Mode{{Instance: "program", Programs: map[string]string{"P": "cotton"}}}},
		"неизвестный instance": {Modes: []Mode{{Instance: "washing", Programs: prog}}},
		"режим дважды":         {Modes: []Mode{{Instance: "program", Programs: map[string]string{"A": "one", "B": "one"}}}},
		"два выбора программы": {Modes: []Mode{{Instance: "program", Programs: prog}, {Instance: "dishwashing", Programs: prog}}},
		"mode без значений":    {Modes: []Mode{{Instance: "fan_speed", Option: "O"}}},
		"on_off":               {OnOff: "toggle"},
		"program без запуска":  {OnOff: "program", Modes: nil, DefaultProgram: ""},
		"range без option":     {Ranges: []Range{{Instance: "temperature"}}},
		"toggle без setting":   {Toggles: []Toggle{{Instance: "backlight"}}},
		"event без values":     {Properties: []Property{{Type: "event", Instance: "open", Status: "S"}}},
		"button":               {Events: &Events{Button: map[string]string{"E": "press"}}},
		"power без значений":   {Power: &Power{Setting: "operation.mode", On: "POWER_ON"}},
		"power on равно off":   {Power: &Power{Setting: "S", On: "A", Off: "A"}},
		"state без idle":       {Power: &Power{Setting: "S", On: "A", Off: "B", State: "runState.currentState"}},
		"idle без state":       {Power: &Power{Setting: "S", On: "A", Off: "B", Idle: []string{"END"}}},
		"toggle без off":       {Toggles: []Toggle{{Instance: "backlight", Setting: "S", On: "ON"}}},
		"toggle on равно off":  {Toggles: []Toggle{{Instance: "backlight", Setting: "S", On: "ON", Off: "ON"}}},
		"переход без значения": {Events: &Events{Button: map[string]string{"runState.currentState=": "click"}}},
		"переход без имени":    {Events: &Events{Button: map[string]string{"=END": "click"}}},
		"неверный шаблон":      {Events: &Events{Button: map[string]string{"[": "click"}}},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			p := &Profile{YandexType: "cooking", OnOff: "none", Toggles: []Toggle{{Instance: "pause"}}}
			p.apply(o)
			if o.OnOff == "program" {
				p.Modes = nil
				p.DefaultProgram = ""
			}
			if err := p.Validate(); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
	if err := (&Profile{YandexType: "cooking", OnOff: "none"}).Validate(); err == nil || !strings.Contains(err.Error(), "нет ни умений") {
		t.Fatalf("пустое устройство: %v", err)
	}
}

func TestMarshalYAML(t *testing.T) {
	// Незаданное поле не пишется, а пустой список и пустое сопоставление - пишутся:
	// это переопределение пустым, и после чтения оно должно остаться таким же.
	p := &Profile{OnOff: "power", DefaultOptions: map[string]any{"X": 180.0},
		Modes: []Mode{{Instance: "program", Programs: map[string]string{}},
			{Instance: "fan_speed", Option: "O", Values: map[string]string{}}},
		Toggles: []Toggle{}}
	data, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Profile
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	back.Normalize()
	if !reflect.DeepEqual(&back, p) || strings.Contains(string(data), "ranges") || strings.Contains(string(data), "yandex_type") {
		t.Fatalf("после записи и чтения:\n%s%+v", data, back)
	}
}

func TestMerge(t *testing.T) {
	base := BuiltinProfile("Oven")
	m := Merge(base, &Profile{Toggles: []Toggle{}})
	if len(m.Toggles) != 0 || len(m.Modes) != 1 || len(base.Toggles) != 2 {
		t.Fatalf("Merge: %+v", m)
	}
	if BuiltinProfile("Kettle") != nil || !slices.Contains(BuiltinTypes(), "Dishwasher") {
		t.Fatal("встроенные профили")
	}
}

func TestMatch(t *testing.T) {
	e := &Events{Button: map[string]string{"*": "long_press", "*_IS_COMPLETE": "click",
		"PREHEATING_IS_COMPLETE": "double_click", "runState.currentState=END": "click"}}
	for event, want := range map[string]string{
		"PREHEATING_IS_COMPLETE":    "double_click",
		"WASHING_IS_COMPLETE":       "click",
		"WATER_IS_FULL":             "long_press",
		"runState.currentState=END": "click",
	} {
		if got, ok := e.Match(event); !ok || got != want {
			t.Errorf("%s: %q", event, got)
		}
	}
	if _, ok := (&Events{Button: map[string]string{"A": "click"}}).Match("B"); ok {
		t.Error("событие без сопоставления")
	}
	if tr := e.Transitions(); len(tr) != 1 || !slices.Equal(tr["runState.currentState"], []string{"END"}) {
		t.Errorf("переходы: %v", tr)
	}
}

func TestLGProfiles(t *testing.T) {
	if !LGType("DEVICE_WASHER") || LGType("Washer") {
		t.Fatal("тип LG")
	}
	p, err := Resolve("DEVICE_WASHER", nil, nil)
	if err != nil || p.OnOff != "power" || p.Power.State != "runState.currentState" || p.Power.Allowed == "" {
		t.Fatalf("стиральная машина LG: %+v %v", p, err)
	}
	// Поле power пишется в настройки и читается обратно.
	data, err := yaml.Marshal(&Profile{Power: p.Power, Toggles: []Toggle{{Instance: "backlight", Setting: "S", On: "ON", Off: "OFF"}}})
	if err != nil {
		t.Fatal(err)
	}
	var back Profile
	if err := yaml.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back.Power, p.Power) || back.Toggles[0].Off != "OFF" {
		t.Fatalf("после записи и чтения:\n%s%+v", data, back)
	}
	if !strings.Contains(string(data), "power:") {
		t.Fatalf("нет power:\n%s", data)
	}
}

// Встроенные профили передают только свойства из документации Яндекса: уведомление с
// незнакомым свойством Яндекс отвергает целиком, и Алиса не узнаёт о приборе ничего.
func TestBuiltinDocumented(t *testing.T) {
	for _, typ := range BuiltinTypes() {
		for _, pr := range BuiltinProfile(typ).Properties {
			_, isFloat := FloatUnits[pr.Instance]
			_, isEvent := EventValues[pr.Instance]
			if pr.Type == "float" && !isFloat || pr.Type == "event" && !isEvent || slices.Contains(Undocumented, pr.Unit) {
				t.Errorf("%s: свойство %s %s %s нет в документации Яндекса", typ, pr.Type, pr.Instance, pr.Unit)
			}
		}
	}
}

// Пересчёт свойства: готовый convert или своя формула; пересчитанное - до сотых.
func TestConversion(t *testing.T) {
	scale, offset, zero := 0.1, 5.0, 0.0
	for _, c := range []struct {
		pr       Property
		in, want float64
	}{
		{Property{Type: "float"}, 21.123, 21.123},
		{Property{Type: "float", Convert: "minutes_to_seconds"}, 85, 5100},
		{Property{Type: "float", Convert: "fahrenheit_to_celsius"}, 70, 21.11},
		{Property{Type: "float", Scale: &scale, Offset: &offset}, 123, 17.3},
	} {
		if got := c.pr.Value(c.in); got != c.want {
			t.Errorf("%+v: %v -> %v, нужно %v", c.pr, c.in, got, c.want)
		}
	}
	check := func(pr Property) error {
		_, err := Resolve("X", &Profile{YandexType: "other", Properties: []Property{pr}}, nil)
		return err
	}
	if err := check(Property{Type: "float", Instance: "my_counter", Status: "s", Unit: "unit.time.seconds", Convert: "hours_to_seconds"}); err != nil {
		t.Fatalf("своё число: %v", err)
	}
	if err := check(Property{Type: "event", Instance: "my_state", Status: "s", Values: map[string]string{"ON": "my_on"}}); err != nil {
		t.Fatalf("своё событие: %v", err)
	}
	for _, pr := range []Property{
		{Type: "event", Instance: "open", Status: "s", Values: map[string]string{"A": "opened"}, Convert: "minutes_to_seconds"},
		{Type: "float", Instance: "x", Status: "s", Convert: "nope"},
		{Type: "float", Instance: "x", Status: "s", Convert: "minutes_to_seconds", Scale: &scale},
		{Type: "float", Instance: "x", Status: "s", Scale: &zero},
		{Type: "float", Instance: "Bad Instance", Status: "s"},
		{Type: "float", Instance: "x", Status: "s", Unit: "seconds"},
		{Type: "event", Instance: "x", Status: "s", Values: map[string]string{"A": "Bad"}},
	} {
		if check(pr) == nil {
			t.Errorf("принято: %+v", pr)
		}
	}
}

// Профили телевизоров берут те же имена функций и типы, что источники телевизоров.
func TestTVNames(t *testing.T) {
	if webos.Type != LGTVType || samsungtv.Type != SamsungTVType {
		t.Fatal("тип телевизора не совпадает с источником")
	}
	for _, typ := range []string{LGTVType, SamsungTVType} {
		p := BuiltinProfile(typ)
		keys := []string{p.Power.Setting, p.Modes[0].Option, p.Ranges[0].Option, p.Ranges[1].Option, p.Toggles[0].Setting, p.Toggles[1].Setting}
		if want := []string{tv.Power, tv.Input, tv.Volume, tv.Channel, tv.Mute, tv.Playback}; !slices.Equal(keys, want) {
			t.Fatalf("%s: функции %v", typ, keys)
		}
		if p.Power.On != tv.On || p.Toggles[1].On != tv.Paused || p.Toggles[1].Off != tv.Playing {
			t.Fatalf("%s: значения функций", typ)
		}
	}
	for _, in := range samsungtv.Inputs {
		if _, ok := BuiltinProfile(SamsungTVType).Modes[0].Values[in]; !ok {
			t.Errorf("вход Samsung %s без режима", in)
		}
	}
}

// Значение с другим именем находится по основному; основное имя важнее.
func TestStatusNames(t *testing.T) {
	const status, other = "Cooking.Oven.Status.CurrentCavityTemperature", "Cooking.Oven.Status.Cavity.001.CurrentTemperature"
	if v, ok := StatusValue(map[string]any{other: 91.0}, status); !ok || v != 91.0 {
		t.Fatalf("другое имя: %v %v", v, ok)
	}
	if v, ok := StatusValue(map[string]any{other: 91.0, status: 22.0}, status); !ok || v != 22.0 {
		t.Fatalf("оба имени: %v %v", v, ok)
	}
	if _, ok := StatusValue(map[string]any{"x": 1.0}, status); ok {
		t.Fatal("значения нет")
	}
	if v, ok := StatusValue(map[string]any{other: 91.0}, other); !ok || v != 91.0 {
		t.Fatalf("само другое имя: %v %v", v, ok)
	}
	if got := StatusNames(status); !slices.Equal(got, []string{status, other}) {
		t.Fatalf("имена: %v", got)
	}
	if got := StatusNames("x"); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("имя без других: %v", got)
	}
	values := map[string]any{other: 91.0}
	if got := WithStatusNames(values); got[status] != 91.0 || got[other] != 91.0 {
		t.Fatalf("значения: %v", got)
	}
	if _, ok := values[status]; ok {
		t.Fatal("исходные значения изменились")
	}
	if got := WithStatusNames(map[string]any{status: 22.0, other: 91.0}); got[status] != 22.0 {
		t.Fatalf("основное имя заменено: %v", got)
	}
}

// Встроенные профили понимают имена событий, которые приборы сообщают в домашней сети.
func TestBuiltinEventNames(t *testing.T) {
	oven := BuiltinProfile("Oven").Events
	for event, want := range map[string]string{
		"BSH.Common.Event.ProgramFinished":                     "click",
		"Cooking.Oven.Event.Cavity.001.ProgramFinished":        "click",
		"Cooking.Oven.Event.PreheatFinished":                   "double_click",
		"Cooking.Oven.Event.Cavity.001.RegularPreheatFinished": "double_click",
		"Cooking.Oven.Event.Cavity.001.FastPreheatFinished":    "double_click",
	} {
		if got, ok := oven.Match(event); !ok || got != want {
			t.Errorf("%s: %q", event, got)
		}
	}
	if !slices.Contains(oven.FoodLevelIfKnown, "Cooking.Oven.Event.Cavity.001.FillWaterContainer") ||
		!slices.Contains(oven.FoodLevelIfKnown, "Cooking.Oven.Event.Cavity.001.EmptyWaterContainer") || len(oven.FoodLevel) != 0 {
		t.Errorf("вода духовки: %+v", oven)
	}
	food := BuiltinProfile("Dishwasher").Events.FoodLevel
	for _, event := range []string{"SaltNearlyEmpty", "RinseAidNearlyEmpty", "SaltLack", "RinseAidLack"} {
		if !slices.Contains(food, "Dishcare.Dishwasher.Event."+event) {
			t.Errorf("нет %s", event)
		}
	}
}
