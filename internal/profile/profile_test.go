package profile

import (
	"maps"
	"math"
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
	// Кнопка из раздела events - свойство самого устройства, и такой профиль законный.
	if err := (&Profile{YandexType: "other", OnOff: "none", Events: &Events{Button: map[string]string{"E": "click"}}}).Validate(); err != nil {
		t.Fatalf("профиль только с кнопкой: %v", err)
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
	if len(m.Toggles) != 0 || len(m.Modes) != 1 || len(base.Toggles) != 3 {
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
	scale, offset, zero, hundred, one := 0.1, 5.0, 0.0, 100.0, 1.0
	for _, c := range []struct {
		pr       Property
		in, want float64
	}{
		{Property{Type: "float"}, 21.123, 21.123},
		{Property{Type: "float", Convert: "minutes_to_seconds"}, 85, 5100},
		{Property{Type: "float", Convert: "fahrenheit_to_celsius"}, 70, 21.11},
		{Property{Type: "float", Scale: &scale, Offset: &offset}, 123, 17.3},
		{Property{Type: "float", Convert: "fraction_to_percent", Min: &zero, Max: &hundred}, 1.5, 100},
		{Property{Type: "float", Convert: "fraction_to_percent", Min: &zero, Max: &hundred}, -0.2, 0},
		{Property{Type: "float", Convert: "fraction_to_percent", Min: &zero, Max: &hundred}, 0.25, 25},
		{Property{Type: "float", Convert: "fraction_to_percent", Max: &hundred}, -0.2, -20},
		{Property{Type: "float", Min: &offset}, 1, 5},
		{Property{Type: "float", Convert: WeightToPercent}, 850, 50},
		{Property{Type: "float", Convert: WeightToPercent, Volume: &one}, 500, 50},
		{Property{Type: "float", Convert: WeightToPercent, Volume: &one, Max: &hundred}, 1200, 100},
	} {
		if got := c.pr.Value(c.in); got != c.want {
			t.Errorf("%+v: %v -> %v, нужно %v", c.pr, c.in, got, c.want)
		}
	}
	inf := math.Inf(1)
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
		{Type: "float", Instance: "x", Status: "s", Min: &hundred, Max: &zero},
		{Type: "float", Instance: "x", Status: "s", Volume: &one},
		{Type: "float", Instance: "x", Status: "s", Convert: WeightToPercent, Volume: &zero},
		{Type: "float", Instance: "x", Status: "s", Convert: WeightToPercent, Volume: &hundred},
		{Type: "float", Instance: "x", Status: "s", Convert: WeightToPercent, Volume: &inf},
		{Type: "float", Instance: "x", Status: "s", Max: &inf},
		{Type: "event", Instance: "open", Status: "s", Values: map[string]string{"A": "opened"}, Min: &zero},
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
	if !slices.Contains(oven.WaterLevelIfKnown, "Cooking.Oven.Event.Cavity.001.FillWaterContainer") ||
		!slices.Contains(oven.WaterLevelIfKnown, "Cooking.Oven.Event.Cavity.001.EmptyWaterContainer") || len(oven.FoodLevelIfKnown) != 0 {
		t.Errorf("вода духовки: %+v", oven)
	}
	dish := BuiltinProfile("Dishwasher").Events
	for _, event := range []string{"SaltNearlyEmpty", "SaltLack"} {
		if !slices.Contains(dish.FoodLevel, "Dishcare.Dishwasher.Event."+event) {
			t.Errorf("нет %s", event)
		}
	}
	for _, event := range []string{"RinseAidNearlyEmpty", "RinseAidLack"} {
		if !slices.Contains(dish.WaterLevel, "Dishcare.Dishwasher.Event."+event) {
			t.Errorf("нет %s", event)
		}
	}
	// Правка основных списков не отключает то, чего нет в редакторе.
	merged := Merge(BuiltinProfile("Oven"), &Profile{Events: &Events{Button: map[string]string{"x": "click"}}})
	if len(merged.Events.WaterLevelIfKnown) != 2 {
		t.Errorf("уровень воды духовки пропал: %+v", merged.Events)
	}
}

// Кофемашина De'Longhi: 17 тревог дают нажатие double_click, а уровни воды и зёрен остаются уровнями.
func TestDeLonghiAlarms(t *testing.T) {
	p, err := Resolve(DeLonghiType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for k, v := range p.Events.Button {
		if v == "double_click" {
			n++
			if !strings.HasPrefix(k, deLonghiAlarmPrefix) || !strings.HasSuffix(k, "="+deLonghiPresent) {
				t.Errorf("не тревога: %s", k)
			}
		}
	}
	if n != 17 {
		t.Fatalf("тревог с нажатием: %d", n)
	}
	for _, level := range []string{"NoCoffee", "TankLevel", "TankPresence"} {
		if _, ok := p.Events.Button[deLonghiAlarmPrefix+level+"="+deLonghiPresent]; ok {
			t.Errorf("%s даёт нажатие", level)
		}
	}
	if !slices.Contains(p.Events.WaterLevel, deLonghiAlarmPrefix+"TankPresence") || !slices.Contains(p.Events.Empty, deLonghiAlarmPrefix+"TankPresence") {
		t.Fatalf("снятый бак: %+v", p.Events)
	}
}

// AllInOne проверяет у Яндекса весь список из документации: каждое свойство и каждое значение события.
func TestAllInOneCoversYandexLists(t *testing.T) {
	floats, events := map[string]bool{}, map[string][]string{}
	for _, pr := range BuiltinProfile(AllInOneType).Properties {
		if pr.Type == "float" {
			floats[pr.Instance] = true
			if units := FloatUnits[pr.Instance]; len(units) > 0 && !slices.Contains(units, pr.Unit) {
				t.Errorf("float %s: единица %q, допустимо %v", pr.Instance, pr.Unit, units)
			}
			continue
		}
		for v := range pr.Values {
			events[pr.Instance] = append(events[pr.Instance], v)
		}
	}
	for instance := range FloatUnits {
		if !floats[instance] {
			t.Errorf("нет float %s", instance)
		}
	}
	for instance, values := range EventValues {
		for _, v := range values {
			if !slices.Contains(events[instance], v) {
				t.Errorf("нет event %s значения %s", instance, v)
			}
		}
	}
}

// Стирально-сушильная машина: только свои программы, программа по умолчанию среди них.
func TestWasherDryerPrograms(t *testing.T) {
	p := BuiltinProfile("WasherDryer")
	if p == nil || !strings.HasPrefix(p.DefaultProgram, "LaundryCare.WasherDryer.Program.") {
		t.Fatalf("профиль: %+v", p)
	}
	mode := p.ProgramMode()
	if mode == nil || mode.Programs[p.DefaultProgram] == "" {
		t.Fatal("программа по умолчанию не в режимах")
	}
	want := map[string]string{"Cotton": "normal", "Cotton.Eco4060": "eco", "EasyCare": "one", "Mix": "two",
		"WashAndDry.60": "three", "WashAndDry.90": "four"}
	if len(mode.Programs) != len(want) {
		t.Fatalf("программы: %v", mode.Programs)
	}
	for k, v := range want {
		if got := mode.Programs["LaundryCare.WasherDryer.Program."+k]; got != v {
			t.Errorf("%s: %q", k, got)
		}
	}
	if BuiltinProfile("Washer").DefaultProgram != "LaundryCare.Washer.Program.Cotton" {
		t.Fatal("профиль стиральной машины изменился")
	}
}

// Дверь: заблокированная закрыта, приоткрытая открыта.
func TestDoorValues(t *testing.T) {
	const prefix = "BSH.Common.EnumType.DoorState."
	want := map[string]string{"Open": "opened", "Ajar": "opened", "Closed": "closed", "Locked": "closed"}
	for _, typ := range []string{"Oven", "Dishwasher", "Washer", "WasherDryer", "Dryer", "Microwave", "WarmingDrawer"} {
		i := slices.IndexFunc(BuiltinProfile(typ).Properties, func(pr Property) bool { return pr.Instance == "open" })
		if i < 0 {
			t.Fatalf("%s: нет двери", typ)
		}
		for k, v := range want {
			if got := BuiltinProfile(typ).Properties[i].Values[prefix+k]; got != v {
				t.Errorf("%s %s: %q", typ, k, got)
			}
		}
	}
}

// Дверь холодильных приборов понимает оба варианта имён значений.
func TestFridgeDoorValues(t *testing.T) {
	want := map[string]string{
		"Refrigeration.Common.EnumType.Door.States.Open":   "opened",
		"Refrigeration.Common.EnumType.Door.States.Closed": "closed",
		"BSH.Common.EnumType.DoorState.Open":               "opened",
		"BSH.Common.EnumType.DoorState.Closed":             "closed",
	}
	for _, typ := range []string{"Refrigerator", "FridgeFreezer", "Freezer", "WineCooler"} {
		pr := BuiltinProfile(typ).Properties[0]
		if !maps.Equal(pr.Values, want) {
			t.Errorf("%s: %v", typ, pr.Values)
		}
	}
}

// Cooktop - тот же профиль, что Hob.
func TestCooktop(t *testing.T) {
	a, err := Resolve("Cooktop", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Resolve("Hob", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.YandexType != b.YandexType || a.OnOff != b.OnOff || a.OnOff != "power" {
		t.Fatalf("Cooktop: %+v, Hob: %+v", a, b)
	}
}

// Подсветка вытяжки необязательна: без настройки в описании переключателя нет.
func TestHoodLightOptional(t *testing.T) {
	tg := BuiltinProfile("Hood").Toggles[0]
	if tg.Instance != "backlight" || !tg.Optional {
		t.Fatalf("подсветка: %+v", tg)
	}
}

// Запасная настройка и другие значения режима: проверка, чтение строгим YAML и запись обратно.
func TestModeExtras(t *testing.T) {
	const src = `instance: thermostat
option: a.mode
fallback_option: b.mode
values:
    wind: fan_only
also_values:
    fan: fan_only
`
	var m Mode
	dec := yaml.NewDecoder(strings.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.FallbackOption != "b.mode" || m.AlsoValues["fan"] != "fan_only" {
		t.Fatalf("прочитано: %+v", m)
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "fallback_option: b.mode") || !strings.Contains(string(out), "also_values:") {
		t.Fatalf("записано: %s", out)
	}
	plain, _ := yaml.Marshal(Mode{Instance: "thermostat", Option: "a.mode", Values: map[string]string{"x": "auto"}})
	if strings.Contains(string(plain), "fallback_option") || strings.Contains(string(plain), "also_values") {
		t.Fatalf("у обычного режима лишние поля: %s", plain)
	}

	// Значение из Values важнее, а значение отсюда - только если оно есть у прибора.
	if got := m.ValueFor("fan_only", []string{"fan"}); got != "fan" {
		t.Fatalf("у прибора только fan: %q", got)
	}
	if got := m.ValueFor("fan_only", []string{"fan", "wind"}); got != "wind" {
		t.Fatalf("у прибора оба: %q", got)
	}
	if got := m.ValueFor("fan_only", nil); got != "wind" {
		t.Fatalf("прибор значений не сообщает: %q", got)
	}
	if got := m.ValueFor("auto", []string{"fan"}); got != "" {
		t.Fatalf("режима нет в профиле: %q", got)
	}
	// Без AlsoValues выбор не зависит от значений прибора.
	plainMode := Mode{Option: "a", Values: map[string]string{"x": "auto"}}
	if got := plainMode.ValueFor("auto", []string{"y"}); got != "x" {
		t.Fatalf("обычный режим: %q", got)
	}

	for name, bad := range map[string]Mode{
		"запасная равна основной":    {Instance: "thermostat", Option: "a", FallbackOption: "a", Values: map[string]string{"x": "auto"}},
		"запасная у программ":        {Instance: "program", Programs: map[string]string{"P": "auto"}, FallbackOption: "a"},
		"другие значения у программ": {Instance: "program", Programs: map[string]string{"P": "auto"}, AlsoValues: map[string]string{"y": "auto"}},
		"значение в обоих списках":   {Instance: "thermostat", Option: "a", Values: map[string]string{"x": "auto"}, AlsoValues: map[string]string{"x": "cool"}},
		"чужой режим":                {Instance: "thermostat", Option: "a", Values: map[string]string{"x": "auto"}, AlsoValues: map[string]string{"y": "nope"}},
		"два значения в values":      {Instance: "thermostat", Option: "a", Values: map[string]string{"x": "auto", "y": "auto"}},
	} {
		p := Profile{YandexType: "other", OnOff: "none", Modes: []Mode{bad}}
		if err := p.Validate(); err == nil {
			t.Fatalf("%s: профиль принят", name)
		}
	}
}

// Кнопка окончания работы машины - свойством, а не разделом events: иначе её получит и машина
// без этого значения. У пульта оба значения удержания и двойного нажатия понятны.
func TestSmartThingsProfiles(t *testing.T) {
	for typ, status := range map[string]string{"Washer": "washerOperatingState.washerJobState", "Dryer": "dryerOperatingState.dryerJobState",
		"Dishwasher": "dishwasherOperatingState.dishwasherJobState"} {
		p := builtin[SmartThingsPrefix+typ]
		if p.Events != nil || p.Properties[0].Instance != "button" || p.Properties[0].Status != status {
			t.Fatalf("%s: %+v", typ, p)
		}
	}
	values := builtin[SmartThingsPrefix+"Button"].Properties[0].Values
	if values["pushed_2x"] != "double_click" || values["down_hold"] != "long_press" {
		t.Fatalf("кнопка: %v", values)
	}
	ac := builtin[SmartThingsPrefix+"AirConditioner"]
	if !slices.ContainsFunc(ac.Toggles, func(tg Toggle) bool { return tg.Instance == "ionization" && tg.Optional }) ||
		!slices.ContainsFunc(ac.Modes, func(m Mode) bool { return m.Instance == "swing" }) {
		t.Fatalf("кондиционер: %+v", ac)
	}
	for typ := range builtin {
		if SmartThingsType(typ) {
			if _, err := Resolve(typ, nil, nil); err != nil {
				t.Fatalf("%s: %v", typ, err)
			}
		}
	}
}

// Запасная настройка числа: необязательна, читается из YAML и не совпадает с основной. Другие имена
// значения ищутся по всем именам, а основное имя главнее.
func TestRangeFallback(t *testing.T) {
	var r Range
	if err := yaml.Unmarshal([]byte("instance: open\noption: a\nfallback_option: b\n"), &r); err != nil || r.FallbackOption != "b" {
		t.Fatalf("%v %+v", err, r)
	}
	out, err := yaml.Marshal(Range{Instance: "open", Option: "a"})
	if err != nil || strings.Contains(string(out), "fallback_option") {
		t.Fatalf("%v %s", err, out)
	}
	p := &Profile{YandexType: "openable", OnOff: "none", Ranges: []Range{{Instance: "open", Option: "a", FallbackOption: "a"}}}
	if p.Validate() == nil {
		t.Fatal("запасная равна основной")
	}
	p.Ranges[0].FallbackOption = "b"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"fineDustSensor.fineDustLevel": 1.0}
	if v, ok := StatusValue(values, "dustSensor.fineDustLevel"); !ok || v != 1.0 {
		t.Fatalf("другое имя: %v %v", v, ok)
	}
	values["dustSensor.fineDustLevel"] = 2.0
	if v, _ := StatusValue(values, "dustSensor.fineDustLevel"); v != 2.0 {
		t.Fatalf("основное имя: %v", v)
	}
}

// Свои рецепты без напитка по умолчанию: «включи» варит один из них, а не встроенный рецепт.
func TestDeLonghiLayerDefaultDrink(t *testing.T) {
	for _, c := range []struct {
		recipes map[string]int
		drink   string
		want    string
	}{
		{nil, "", DeLonghiRecipePrefix + "6"},
		{map[string]int{"latte": 9, "espresso": 1}, "", DeLonghiRecipePrefix + "1"},
		{map[string]int{"latte": 9, "espresso": 1}, "latte", DeLonghiRecipePrefix + "9"},
	} {
		if got := DeLonghiLayer(c.recipes, c.drink).DefaultProgram; got != c.want {
			t.Fatalf("%v %q: %s", c.recipes, c.drink, got)
		}
	}
}

// Датчики есть у всех устройств SmartThings, без повторов; газ и присутствие читаются по другим именам.
func TestSmartThingsSensorsEverywhere(t *testing.T) {
	for _, category := range []string{"Light", "Washer", "Refrigerator", "Oven", "Button", "RobotCleaner", "Siren", "PresenceSensor"} {
		p, err := Resolve(SmartThingsPrefix+category, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", category, err)
		}
		for _, instance := range []string{"gas", "vibration", "pressure"} {
			if !slices.ContainsFunc(p.Properties, func(pr Property) bool { return pr.Instance == instance }) {
				t.Fatalf("%s: нет %s", category, instance)
			}
		}
	}
	if v, ok := StatusValue(map[string]any{"carbonMonoxideDetector.carbonMonoxide": "detected"}, "gasDetector.gas"); !ok || v != "detected" {
		t.Fatalf("угарный газ: %v %v", v, ok)
	}
	if v, ok := StatusValue(map[string]any{"presenceSensor.presence": "not present"}, "motionSensor.motion"); !ok || v != "not present" {
		t.Fatalf("присутствие: %v %v", v, ok)
	}
}

// Новые значения кнопки: обслуживание отдельно от неполадок; у духовки LG double_click только разогрев.
func TestServiceButtons(t *testing.T) {
	for _, c := range []struct{ kind, event, want string }{
		{"Dishwasher", "Dishcare.Dishwasher.Event.MachineCareReminder", "double_click"},
		{"Dishwasher", "Dishcare.Dishwasher.Event.DrainPumpBlocked", "long_press"},
		{"Washer", "LaundryCare.Washer.Event.PumpError", "long_press"},
		{"Dryer", "LaundryCare.Dryer.Event.DryingProcessFinished", "double_click"},
		{"CoffeeMaker", "ConsumerProducts.CoffeeMaker.Event.DripTrayFull", "long_press"},
		{"Hood", "Cooking.Common.Event.Hood.GreaseFilterMaxSaturationReached", "double_click"},
		{"CleaningRobot", "ConsumerProducts.CleaningRobot.Event.RobotIsStuck", "long_press"},
		{"DEVICE_DISH_WASHER", "TIME_TO_CLEAN_FILTER", "double_click"},
		{"DEVICE_DISH_WASHER", "SALT_REFILL_IS_NEEDED", "double_click"},
		{"DEVICE_DISH_WASHER", "SOME_FAILURE", "long_press"},
		{"DEVICE_OVEN", "TIME_TO_CLEAN_FILTER", "long_press"},
		{PolarisKettleType, "error/code=06", "double_click"},
	} {
		p, err := Resolve(c.kind, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := p.Events.Match(c.event); got != c.want {
			t.Fatalf("%s %s: %q", c.kind, c.event, got)
		}
	}
}
