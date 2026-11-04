package profile

import (
	"slices"
	"testing"
)

// Кнопка приборов Home Connect: новые события и значения, которые заняты другими событиями.
func TestHomeConnectButtons(t *testing.T) {
	for _, c := range []struct {
		kind, event, want string
	}{
		{"Hood", "BSH.Common.Event.Favorite.001.ExternalTrigger", "click"},
		{"Hood", "BSH.Common.Event.Favorite.002.ExternalTrigger", "long_press"},
		{"Hood", "Cooking.Common.Event.Hood.GreaseFilterMaxSaturationReached", "double_click"},
		{"Hob", "BSH.Common.Event.Favorite.001.ExternalTrigger", "click"},
		{"Cooktop", "BSH.Common.Event.Favorite.002.ExternalTrigger", "long_press"},
		{"Hob", "BSH.Common.Event.AlarmClockElapsed", "double_click"},
		{"Refrigerator", "Refrigeration.FridgeFreezer.Event.DoorAlarmRefrigerator", "double_click"},
		{"Refrigerator", "Refrigeration.Common.Event.Door.AlarmRefrigerator", "double_click"},
		{"Freezer", "Refrigeration.FridgeFreezer.Event.DoorAlarmFreezer", "double_click"},
		{"Freezer", "Refrigeration.Common.Event.Door.AlarmFreezer", "double_click"},
		{"Freezer", "Refrigeration.FridgeFreezer.Event.TemperatureAlarmFreezer", "long_press"},
		{"Freezer", "Refrigeration.Common.Event.Freezer.TemperatureAlarm", "long_press"},
		{"FridgeFreezer", "Refrigeration.FridgeFreezer.Event.DoorAlarmRefrigerator", "double_click"},
		{"FridgeFreezer", "Refrigeration.Common.Event.Freezer.TemperatureAlarm", "long_press"},
		{"CookProcessor", "BSH.Common.Event.ProgramFinished", "click"},
		{"CookProcessor", "BSH.Common.Event.ProgramAborted", "double_click"},
		// Проверенные значения духовки и посудомойки остались.
		{"Oven", "BSH.Common.Event.ProgramFinished", "click"},
		{"Oven", "Cooking.Oven.Event.PreheatFinished", "double_click"},
		{"Oven", "BSH.Common.Status.OperationState=BSH.Common.EnumType.OperationState.Error", "long_press"},
		{"Dishwasher", "BSH.Common.Event.ProgramFinished", "click"},
	} {
		e := BuiltinProfile(c.kind).Events
		if e == nil {
			t.Fatalf("%s: нет раздела events", c.kind)
		}
		if got := e.Button[c.event]; got != c.want {
			t.Errorf("%s %s: %q, нужно %q", c.kind, c.event, got, c.want)
		}
	}
	// У холодильника нет морозильной камеры, а у морозильника - холодильной.
	if _, ok := BuiltinProfile("Refrigerator").Events.Button["Refrigeration.FridgeFreezer.Event.DoorAlarmFreezer"]; ok {
		t.Error("дверь морозильной камеры у холодильника")
	}
	if _, ok := BuiltinProfile("Freezer").Events.Button["Refrigeration.FridgeFreezer.Event.DoorAlarmRefrigerator"]; ok {
		t.Error("дверь холодильной камеры у морозильника")
	}
	if BuiltinProfile("WineCooler").Events != nil {
		t.Error("у винного шкафа в документации нет тревог")
	}
	// Тревоги и кнопки одного прибора не попадают в профиль другого.
	// long_press - авария: таймер и отмена, в том числе командой «выключи», её не запускают.
	for _, typ := range []string{"Dryer", "Dishwasher", "Microwave", "CleaningRobot"} {
		if _, ok := BuiltinProfile(typ).Events.Button["BSH.Common.Event.ProgramAborted"]; ok {
			t.Errorf("отмена у %s", typ)
		}
	}
	if _, ok := BuiltinProfile("Oven").Events.Button["BSH.Common.Event.AlarmClockElapsed"]; ok {
		t.Error("таймер у духовки")
	}
}

// Новые настройки необязательны: у прибора без них умения не появляются, а значения режимов Алисы
// есть в списке Яндекса (остальное проверяет Validate).
func TestHomeConnectOptionalSkills(t *testing.T) {
	for _, c := range []struct {
		kind, instance, setting string
	}{
		{"Refrigerator", "backlight", "Refrigeration.Common.Setting.Light.Internal.Power"},
		{"FridgeFreezer", "backlight", "Refrigeration.Common.Setting.Light.Internal.Power"},
		{"Freezer", "backlight", "Refrigeration.Common.Setting.Light.Internal.Power"},
		{"WineCooler", "backlight", "Refrigeration.Common.Setting.Light.Internal.Power"},
		{"Dishwasher", "backlight", "Dishcare.Dishwasher.Setting.InfoLight"},
		{"Washer", "backlight", "LaundryCare.Common.Setting.DrumLight.Active"},
		{"WasherDryer", "backlight", "LaundryCare.Common.Setting.DrumLight.Active"},
		{"Washer", "mute", "LaundryCare.Common.Setting.Sound.Mute"},
		{"WasherDryer", "mute", "LaundryCare.Common.Setting.Sound.Mute"},
		{"Dryer", "mute", "LaundryCare.Common.Setting.Sound.Mute"},
		{"CoffeeMaker", "keep_warm", "ConsumerProducts.CoffeeMaker.Setting.CupWarmer"},
	} {
		i := slices.IndexFunc(BuiltinProfile(c.kind).Toggles, func(x Toggle) bool { return x.Instance == c.instance })
		if i < 0 {
			t.Errorf("%s: нет %s", c.kind, c.instance)
			continue
		}
		if x := BuiltinProfile(c.kind).Toggles[i]; x.Setting != c.setting || !x.Optional {
			t.Errorf("%s %s: %+v", c.kind, c.instance, x)
		}
	}
	// Яркость без пределов в профиле: иначе умение появится и у прибора без этой настройки.
	for kind, option := range map[string]string{
		"Hood":          "Cooking.Common.Setting.LightingBrightness",
		"Refrigerator":  "Refrigeration.Common.Setting.Light.Internal.Brightness",
		"FridgeFreezer": "Refrigeration.Common.Setting.Light.Internal.Brightness",
		"Freezer":       "Refrigeration.Common.Setting.Light.Internal.Brightness",
		"WineCooler":    "Refrigeration.Common.Setting.Light.Internal.Brightness",
	} {
		ranges := BuiltinProfile(kind).Ranges
		i := slices.IndexFunc(ranges, func(r Range) bool { return r.Instance == "brightness" })
		if i < 0 {
			t.Errorf("%s: нет яркости", kind)
			continue
		}
		if r := ranges[i]; r.Option != option || r.Unit != percent || r.Min != nil || r.Max != nil {
			t.Errorf("%s: %+v", kind, r)
		}
	}
	// Другие имена настроек температуры и яркости.
	for kind, want := range map[string]string{
		"Refrigerator":  "Refrigeration.Common.Setting.Refrigerator.SetpointTemperature",
		"FridgeFreezer": "Refrigeration.Common.Setting.Refrigerator.SetpointTemperature",
		"Freezer":       "Refrigeration.Common.Setting.Freezer.SetpointTemperature",
		"Hood":          "Cooking.Hood.Setting.LightingBrightness",
	} {
		if got := BuiltinProfile(kind).Ranges[0].FallbackOption; got != want {
			t.Errorf("%s: запасное имя %q", kind, got)
		}
	}
}

func TestHomeConnectModes(t *testing.T) {
	mode := func(kind, instance string) *Mode {
		t.Helper()
		modes := BuiltinProfile(kind).Modes
		i := slices.IndexFunc(modes, func(m Mode) bool { return m.Instance == instance })
		if i < 0 {
			t.Fatalf("%s: нет режима %s", kind, instance)
		}
		return &modes[i]
	}
	for _, c := range []struct {
		kind, instance, option, value, want string
	}{
		{"CleaningRobot", "cleanup_mode", "ConsumerProducts.CleaningRobot.Option.CleaningMode", "ConsumerProducts.CleaningRobot.EnumType.CleaningModes.Silent", "quiet"},
		{"CleaningRobot", "cleanup_mode", "ConsumerProducts.CleaningRobot.Option.CleaningMode", "ConsumerProducts.CleaningRobot.EnumType.CleaningMode.MopOnly", "wet_cleaning"},
		{"CleaningRobot", "work_speed", "ConsumerProducts.CleaningRobot.Option.SuctionPower", "ConsumerProducts.CleaningRobot.EnumType.SuctionPower.Max", "max"},
		{"Hood", "work_speed", "Cooking.Common.Option.Hood.IntensiveLevel", "Cooking.Hood.EnumType.IntensiveStage.IntensiveStage2", "turbo"},
		{"Hob", "fan_speed", "Cooking.Hob.Setting.Ventilation", "Cooking.Hob.EnumType.Ventilation.Level17", "nine"},
		{"Cooktop", "fan_speed", "Cooking.Hob.Setting.Ventilation", "Cooking.Hob.EnumType.Ventilation.BoostLevel2", "max"},
		{"Dryer", "heat", "LaundryCare.Dryer.Option.DryingTarget", "LaundryCare.Dryer.EnumType.DryingTarget.ExtraDry", "max"},
		{"WasherDryer", "heat", "LaundryCare.Dryer.Option.DryingTarget", "LaundryCare.Dryer.EnumType.DryingTarget.GentleDry", "min"},
		{"AirConditioner", "fan_speed", "HeatingVentilationAirConditioning.AirConditioner.Option.FanSpeedMode", "HeatingVentilationAirConditioning.AirConditioner.EnumType.FanSpeedMode.Automatic", "auto"},
	} {
		m := mode(c.kind, c.instance)
		if m.Option != c.option || m.Values[c.value] != c.want {
			t.Errorf("%s %s: %s = %q", c.kind, c.instance, c.value, m.Values[c.value])
		}
	}
	for _, c := range []struct {
		kind, instance, prefix string
		programs               map[string]string
	}{
		{"Oven", "program", "Cooking.Oven.Program.HeatingMode.", map[string]string{
			"HotAir": "baking", "Defrost": "one", "HotAirGrilling": "two", "HotAir100Steam": "steam", "FrozenHeatupSpecial": "ten"}},
		{"Dishwasher", "dishwashing", "Dishcare.Dishwasher.Program.", map[string]string{
			"Auto2": "auto", "NightWash": "quiet", "Auto1": "one", "MixedLoad": "ten"}},
		{"Washer", "program", "LaundryCare.Washer.Program.", map[string]string{
			"Cotton": "normal", "Cotton.CottonEco": "eco", "Mix.NightWash": "quiet", "SportFitness": "ten"}},
		{"Dryer", "program", "LaundryCare.Dryer.Program.", map[string]string{
			"Cotton": "normal", "Super40": "express", "TimeWarm": "ten"}},
		{"CoffeeMaker", "coffee_mode", "ConsumerProducts.CoffeeMaker.Program.Beverage.", map[string]string{
			"Espresso": "espresso", "EspressoDoppio": "double_espresso", "HotWater": "eight"}},
	} {
		m := mode(c.kind, c.instance)
		for k, want := range c.programs {
			if got := m.Programs[c.prefix+k]; got != want {
				t.Errorf("%s %s: %q, нужно %q", c.kind, k, got, want)
			}
		}
	}
	// Вытяжка без режима программ: default_options ушли бы и в программы, которым они не подходят.
	if BuiltinProfile("Hood").ProgramMode() != nil {
		t.Error("у вытяжки режим программ")
	}
}

func TestHomeConnectOtherNames(t *testing.T) {
	for status, other := range map[string]string{
		"BSH.Common.Status.DoorState":                   "Cooking.Oven.Status.Cavity.001.DoorState",
		"Refrigeration.Common.Status.Door.Refrigerator": "Refrigeration.FridgeFreezer.Status.DoorRefrigerator",
		"Refrigeration.Common.Status.Door.Freezer":      "Refrigeration.FridgeFreezer.Status.DoorFreezer",
	} {
		if v, ok := StatusValue(map[string]any{other: "x"}, status); !ok || v != "x" {
			t.Errorf("%s: %v", other, v)
		}
		// Основное имя важнее: у проверенных приборов ничего не меняется.
		if v, _ := StatusValue(map[string]any{other: "x", status: "y"}, status); v != "y" {
			t.Errorf("%s: основное имя не главное: %v", status, v)
		}
	}
	coffee := BuiltinProfile("CoffeeMaker")
	if i := slices.IndexFunc(coffee.Properties, func(p Property) bool { return p.Instance == "meter" }); i < 0 ||
		coffee.Properties[i].Status != "ConsumerProducts.CoffeeMaker.Status.BeverageCounterCoffee" || coffee.Properties[i].Unit != "" {
		t.Errorf("счётчик кофе: %+v", coffee.Properties)
	}
}
