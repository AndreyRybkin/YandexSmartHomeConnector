package profile

import (
	"maps"
	"slices"
)

// Встроенные профили по типам Home Connect. Имена программ и параметров взяты
// из документации Home Connect, на настоящих приборах профили пока не проверены.

const celsius = "unit.temperature.celsius"

var (
	// Заблокированная дверь закрыта, приоткрытая - открыта.
	doorOpen = Property{Type: "event", Instance: "open", Status: "BSH.Common.Status.DoorState",
		Values: map[string]string{
			"BSH.Common.EnumType.DoorState.Open":   "opened",
			"BSH.Common.EnumType.DoorState.Ajar":   "opened",
			"BSH.Common.EnumType.DoorState.Closed": "closed",
			"BSH.Common.EnumType.DoorState.Locked": "closed",
		}}
	// Дверь холодильных приборов: документация считает основными значения Refrigeration, но
	// разбирать велит и прежние значения BSH.
	fridgeDoorValues = map[string]string{
		"Refrigeration.Common.EnumType.Door.States.Open":   "opened",
		"Refrigeration.Common.EnumType.Door.States.Closed": "closed",
		"BSH.Common.EnumType.DoorState.Open":               "opened",
		"BSH.Common.EnumType.DoorState.Closed":             "closed",
	}
	pause    = Toggle{Instance: "pause"}
	finished = map[string]string{"BSH.Common.Event.ProgramFinished": "click"}
	// aquaStop - прибор сообщил о протечке. Ключа нет в документации облака, поэтому свойство есть,
	// только если прибор описывает это событие.
	aquaStop = Property{Type: "event", Instance: "water_leak", Status: "BSH.Common.Event.AquaStopOccured", IfKnown: true,
		Values: map[string]string{
			"BSH.Common.EnumType.EventPresentState.Present":   "leak",
			"BSH.Common.EnumType.EventPresentState.Off":       "dry",
			"BSH.Common.EnumType.EventPresentState.Confirmed": "dry",
		}}
	// laundryButton - стирка: напоминание и мало средства - double_click, неполадки - long_press.
	laundryButton = map[string]string{
		"BSH.Common.Event.ProgramFinished":                    "click",
		"LaundryCare.Washer.Event.DrumCleanReminder":          "double_click",
		"LaundryCare.Washer.Event.IDos1FillLevelPoor":         "double_click",
		"LaundryCare.Washer.Event.IDos2FillLevelPoor":         "double_click",
		"LaundryCare.Washer.Event.PumpError":                  "long_press",
		"LaundryCare.Washer.Event.Spin.SpinAbort":             "long_press",
		"LaundryCare.Common.Event.FoamDetection":              "long_press",
		"LaundryCare.Common.Event.FatalErrorOccured":          "long_press",
		"LaundryCare.Common.Event.DoorLock.WaterLevelTooHigh": "long_press",
		"BSH.Common.Event.LowWaterPressure":                   "long_press",
	}
	// iDosLevel - мало средства в дозаторе i-Dos: средство не вода, но так же устроен ополаскиватель.
	iDosLevel = []string{"LaundryCare.Washer.Event.IDos1FillLevelPoor", "LaundryCare.Washer.Event.IDos2FillLevelPoor"}
)

func programs(prefix string, m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[prefix+k] = v
	}
	return out
}

var washer = &Profile{
	YandexType:     "washing_machine",
	OnOff:          "program",
	DefaultProgram: "LaundryCare.Washer.Program.Cotton",
	Modes: []Mode{{Instance: "program", Programs: programs("LaundryCare.Washer.Program.", map[string]string{
		"Cotton": "normal", "EasyCare": "one", "Mix": "two", "DelicatesSilk": "three", "Wool": "four",
		"Sensitive": "five", "Auto30": "auto", "Super153045.Super1530": "express",
	})}},
	Toggles:    []Toggle{pause},
	Properties: []Property{doorOpen, aquaStop},
	Events:     &Events{Button: laundryButton, WaterLevelIfKnown: iDosLevel},
}

// У стирально-сушильной машины в документации свои имена программ. Добавить их к профилю
// стиральной машины нельзя: один режим Алисы не отдаётся двум программам.
var washerDryer = &Profile{
	YandexType:     "washing_machine",
	OnOff:          "program",
	DefaultProgram: "LaundryCare.WasherDryer.Program.Cotton",
	Modes: []Mode{{Instance: "program", Programs: programs("LaundryCare.WasherDryer.Program.", map[string]string{
		"Cotton": "normal", "Cotton.Eco4060": "eco", "EasyCare": "one", "Mix": "two",
		"WashAndDry.60": "three", "WashAndDry.90": "four",
	})}},
	Toggles:    []Toggle{pause},
	Properties: []Property{doorOpen, aquaStop},
	Events:     &Events{Button: laundryButton, WaterLevelIfKnown: iDosLevel},
}

// Варочную панель и кухонный процессор облако только показывает: программ для запуска в
// API нет, а включит ли их облако по питанию, не проверено.
var hob = &Profile{YandexType: "cooking", OnOff: "power"}

var fridge = func(setting, door string) *Profile {
	return &Profile{
		YandexType: "cooking",
		OnOff:      "none",
		Ranges:     []Range{{Instance: "temperature", Option: setting, Unit: celsius}},
		Properties: []Property{{Type: "event", Instance: "open", Status: door,
			Values: fridgeDoorValues}},
	}
}

var builtin = map[string]*Profile{
	"Oven": {
		YandexType:     "cooking",
		OnOff:          "program",
		DefaultProgram: "Cooking.Oven.Program.HeatingMode.HotAir",
		DefaultOptions: map[string]any{"Cooking.Oven.Option.SetpointTemperature": 180.0},
		Modes: []Mode{{Instance: "program", Programs: programs("Cooking.Oven.Program.HeatingMode.", map[string]string{
			"HotAir": "baking", "TopBottomHeating": "normal", "HotAirEco": "eco", "PizzaSetting": "pizza",
			"PreHeating": "preheat", "SlowCook": "slow_cook", "GrillLargeArea": "frying", "Defrost": "one",
		})}},
		Ranges:  []Range{{Instance: "temperature", Option: "Cooking.Oven.Option.SetpointTemperature", Unit: celsius}},
		Toggles: []Toggle{pause, {Instance: "backlight", Setting: "Cooking.Oven.Setting.Light.Cavity.001.Power", Optional: true}},
		Properties: []Property{doorOpen,
			{Type: "float", Instance: "temperature", Status: "Cooking.Oven.Status.CurrentCavityTemperature", Unit: celsius}},
		Events: &Events{Button: map[string]string{
			"BSH.Common.Event.ProgramFinished":          "click",
			"Cooking.Oven.Event.PreheatFinished":        "double_click",
			"Cooking.Oven.Event.RegularPreheatFinished": "double_click",
			// Те же события у духовки с номером камеры в именах.
			"Cooking.Oven.Event.Cavity.001.ProgramFinished":        "click",
			"Cooking.Oven.Event.Cavity.001.RegularPreheatFinished": "double_click",
			"Cooking.Oven.Event.Cavity.001.FastPreheatFinished":    "double_click",
		},
			// Ёмкость для воды у духовки с паром.
			WaterLevelIfKnown: []string{"Cooking.Oven.Event.Cavity.001.FillWaterContainer", "Cooking.Oven.Event.Cavity.001.EmptyWaterContainer"},
		},
	},
	"Dishwasher": {
		YandexType:     "dishwasher",
		OnOff:          "program",
		DefaultProgram: "Dishcare.Dishwasher.Program.Auto2",
		Modes: []Mode{{Instance: "dishwashing", Programs: programs("Dishcare.Dishwasher.Program.", map[string]string{
			"Auto2": "auto", "Eco50": "eco", "Quick45": "express", "Intensiv70": "intensive",
			"Normal65": "normal", "Glas40": "glass", "PreRinse": "pre_rinse", "NightWash": "quiet",
		})}},
		Toggles:    []Toggle{pause},
		Properties: []Property{doorOpen, aquaStop},
		Events: &Events{
			Button: map[string]string{
				"BSH.Common.Event.ProgramFinished": "click",
				// Напоминания об уходе.
				"Dishcare.Dishwasher.Event.MachineCareReminder":                                "double_click",
				"Dishcare.Dishwasher.Event.MachineCareAndFilterCleaningReminder":               "double_click",
				"Dishcare.Dishwasher.Event.MachineCareAndLowMaintenanceFilterCleaningReminder": "double_click",
				"Dishcare.Dishwasher.Event.SmartFilterCleaningReminder":                        "double_click",
				// Неполадки.
				"Dishcare.Dishwasher.Event.CheckFilterSystem":    "long_press",
				"Dishcare.Dishwasher.Event.DrainingNotPossible":  "long_press",
				"Dishcare.Dishwasher.Event.DrainPumpBlocked":     "long_press",
				"Dishcare.Dishwasher.Event.WaterheaterCalcified": "long_press",
				"BSH.Common.Event.LowWaterPressure":              "long_press",
			},
			// NearlyEmpty - скоро закончится, Lack - уже нет: прибор сообщает их по отдельности.
			FoodLevel:  []string{"Dishcare.Dishwasher.Event.SaltNearlyEmpty", "Dishcare.Dishwasher.Event.SaltLack", "Dishcare.Dishwasher.Event.ProgramBlockedSaltLack"},
			WaterLevel: []string{"Dishcare.Dishwasher.Event.RinseAidNearlyEmpty", "Dishcare.Dishwasher.Event.RinseAidLack"},
			Empty:      []string{"Dishcare.Dishwasher.Event.SaltLack", "Dishcare.Dishwasher.Event.ProgramBlockedSaltLack", "Dishcare.Dishwasher.Event.RinseAidLack"},
		},
	},
	"Washer":      washer,
	"WasherDryer": washerDryer,
	"Dryer": {
		YandexType:     "washing_machine",
		OnOff:          "program",
		DefaultProgram: "LaundryCare.Dryer.Program.Cotton",
		Modes: []Mode{{Instance: "program", Programs: programs("LaundryCare.Dryer.Program.", map[string]string{
			"Cotton": "normal", "Synthetic": "one", "Mix": "two",
		})}},
		Toggles:    []Toggle{pause},
		Properties: []Property{doorOpen},
		Events: &Events{Button: map[string]string{
			"BSH.Common.Event.ProgramFinished": "click",
			// Сушка закончилась, а защита от складок ещё идёт: бельё уже можно забрать.
			"LaundryCare.Dryer.Event.DryingProcessFinished":   "double_click",
			"LaundryCare.Dryer.Event.CondensateContainerFull": "long_press",
			"LaundryCare.Dryer.Event.LintFilterFull":          "long_press",
			"LaundryCare.Dryer.Event.Maintenance.Remind":      "long_press",
		}},
	},
	"CoffeeMaker": {
		YandexType: "cooking.coffee_maker",
		OnOff:      "power",
		Modes: []Mode{{Instance: "coffee_mode", Programs: programs("ConsumerProducts.CoffeeMaker.Program.Beverage.", map[string]string{
			"Espresso": "espresso", "Coffee": "americano", "Cappuccino": "cappuccino", "CaffeLatte": "latte",
			"LatteMacchiato": "one",
		})}},
		Events: &Events{
			Button: map[string]string{
				// Обслуживание.
				"ConsumerProducts.CoffeeMaker.Event.DeviceShouldBeDescaled":     "double_click",
				"ConsumerProducts.CoffeeMaker.Event.DeviceDescalingOverdue":     "double_click",
				"ConsumerProducts.CoffeeMaker.Event.DeviceDescalingBlockage":    "double_click",
				"ConsumerProducts.CoffeeMaker.Event.DeviceShouldBeCleaned":      "double_click",
				"ConsumerProducts.CoffeeMaker.Event.DeviceCleaningOverdue":      "double_click",
				"ConsumerProducts.CoffeeMaker.Event.DeviceShouldBeCalcNCleaned": "double_click",
				"ConsumerProducts.CoffeeMaker.Event.DeviceCalcNCleanOverdue":    "double_click",
				"ConsumerProducts.CoffeeMaker.Event.DeviceCalcNCleanBlockage":   "double_click",
				// Поддон и молоко.
				"ConsumerProducts.CoffeeMaker.Event.DripTrayFull":        "long_press",
				"ConsumerProducts.CoffeeMaker.Event.DripTrayNotInserted": "long_press",
				"ConsumerProducts.CoffeeMaker.Event.KeepMilkTankCool":    "long_press",
			},
			FoodLevel:         []string{"ConsumerProducts.CoffeeMaker.Event.BeanContainerEmpty"},
			WaterLevel:        []string{"ConsumerProducts.CoffeeMaker.Event.WaterTankEmpty"},
			WaterLevelIfKnown: []string{"ConsumerProducts.CoffeeMaker.Event.WaterTankNearlyEmpty", "ConsumerProducts.CoffeeMaker.Event.WaterTankNotInserted"},
			Empty: []string{"ConsumerProducts.CoffeeMaker.Event.BeanContainerEmpty", "ConsumerProducts.CoffeeMaker.Event.WaterTankEmpty",
				"ConsumerProducts.CoffeeMaker.Event.WaterTankNotInserted"},
		},
	},
	"Hood": {
		YandexType:     "ventilation",
		OnOff:          "program",
		DefaultProgram: "Cooking.Common.Program.Hood.Venting",
		DefaultOptions: map[string]any{"Cooking.Common.Option.Hood.VentingLevel": "Cooking.Hood.EnumType.Stage.FanStage02"},
		Modes: []Mode{{Instance: "fan_speed", Option: "Cooking.Common.Option.Hood.VentingLevel", Values: map[string]string{
			"Cooking.Hood.EnumType.Stage.FanStage01": "one",
			"Cooking.Hood.EnumType.Stage.FanStage02": "two",
			"Cooking.Hood.EnumType.Stage.FanStage03": "three",
			"Cooking.Hood.EnumType.Stage.FanStage04": "four",
			"Cooking.Hood.EnumType.Stage.FanStage05": "five",
		}}},
		Toggles: []Toggle{{Instance: "backlight", Setting: "Cooking.Common.Setting.Lighting", Optional: true}},
		// Жировой фильтр пора помыть.
		Events: &Events{Button: map[string]string{
			"Cooking.Common.Event.Hood.GreaseFilterMaxSaturationNearlyReached": "double_click",
			"Cooking.Common.Event.Hood.GreaseFilterMaxSaturationReached":       "double_click",
		}},
	},
	"Refrigerator":  fridge("Refrigeration.FridgeFreezer.Setting.SetpointTemperatureRefrigerator", "Refrigeration.Common.Status.Door.Refrigerator"),
	"FridgeFreezer": fridge("Refrigeration.FridgeFreezer.Setting.SetpointTemperatureRefrigerator", "Refrigeration.Common.Status.Door.Refrigerator"),
	"Freezer":       fridge("Refrigeration.FridgeFreezer.Setting.SetpointTemperatureFreezer", "Refrigeration.Common.Status.Door.Freezer"),
	"WineCooler":    fridge("Refrigeration.Common.Setting.WineCompartment.SetpointTemperature", "Refrigeration.Common.Status.Door.WineCompartment"),
	// Кондиционер включается питанием, а режим - программа: выбор режима сразу её запускает.
	"AirConditioner": {
		YandexType: "thermostat.ac",
		OnOff:      "power",
		Modes: []Mode{{Instance: "thermostat", Programs: programs("HeatingVentilationAirConditioning.AirConditioner.Program.", map[string]string{
			"Auto": "auto", "Cool": "cool", "Dry": "dry", "Fan": "fan_only", "Heat": "heat",
		})}},
		Ranges: []Range{{Instance: "temperature", Option: "HeatingVentilationAirConditioning.AirConditioner.Option.SetpointTemperature", Unit: celsius}},
	},
	// Микроволновка без программы по умолчанию: «включи» не греет, пока не выбрана мощность.
	"Microwave": {
		YandexType:     "cooking",
		OnOff:          "program",
		DefaultOptions: map[string]any{"BSH.Common.Option.Duration": 30.0},
		Modes: []Mode{{Instance: "program", Programs: programs("Cooking.Oven.Program.Microwave.", map[string]string{
			"90Watt": "one", "180Watt": "two", "360Watt": "three", "450Watt": "four", "600Watt": "five",
			"900Watt": "six", "1000Watt": "seven", "Max": "max",
		})}},
		Toggles:    []Toggle{pause},
		Properties: []Property{doorOpen},
		Events:     &Events{Button: finished},
	},
	"WarmingDrawer": {
		YandexType:     "cooking",
		OnOff:          "program",
		DefaultProgram: "Cooking.Oven.Program.HeatingMode.WarmingDrawer",
		Modes: []Mode{{Instance: "heat", Option: "Cooking.Oven.Option.WarmingLevel", Values: map[string]string{
			"Cooking.Oven.EnumType.WarmingLevel.Low":    "low",
			"Cooking.Oven.EnumType.WarmingLevel.Medium": "medium",
			"Cooking.Oven.EnumType.WarmingLevel.High":   "high",
		}}},
		Properties: []Property{doorOpen},
		Events:     &Events{Button: finished},
	},
	// Документация Home Connect называет варочную панель Cooktop, области доступа - Hob.
	"Cooktop":       hob,
	"Hob":           hob,
	"CookProcessor": {YandexType: "cooking.multicooker", OnOff: "power"},
	"CleaningRobot": {
		YandexType:     "vacuum_cleaner",
		OnOff:          "program",
		DefaultProgram: "ConsumerProducts.CleaningRobot.Program.Cleaning.CleanAll",
		Modes: []Mode{{Instance: "program", Programs: map[string]string{
			"ConsumerProducts.CleaningRobot.Program.Cleaning.CleanAll": "auto",
			"ConsumerProducts.CleaningRobot.Program.Cleaning.CleanMap": "one",
		}}},
		Toggles: []Toggle{pause},
		// Заряд сообщают серии Roxxter и Spotless, от 0 до 100 %.
		Properties: []Property{{Type: "float", Instance: "battery_level", Status: "BSH.Common.Status.BatteryLevel", Unit: percent}},
		Events: &Events{Button: map[string]string{
			"BSH.Common.Event.ProgramFinished":                                "click",
			"ConsumerProducts.CleaningRobot.Event.EmptyDustBoxAndCleanFilter": "double_click",
			"ConsumerProducts.CleaningRobot.Event.RobotIsStuck":               "long_press",
			"ConsumerProducts.CleaningRobot.Event.DockingStationNotFound":     "long_press",
			"ConsumerProducts.CleaningRobot.Status.Lost=true":                 "long_press",
			"ConsumerProducts.CleaningRobot.Status.Lifted=true":               "long_press",
			"ConsumerProducts.CleaningRobot.Status.DustBoxInserted=false":     "long_press",
		}},
	},
}

// Авария прибора - long_press. ActionRequired сюда не входит: прибор просто ждёт человека, и при
// обычной работе сценарий срабатывал бы зря.
func init() {
	const failure = "BSH.Common.Status.OperationState=BSH.Common.EnumType.OperationState.Error"
	for _, t := range []string{"Oven", "Dishwasher", "Washer", "WasherDryer", "Dryer", "Microwave", "CleaningRobot", "CoffeeMaker", "AirConditioner"} {
		p := builtin[t]
		e := Events{}
		if p.Events != nil {
			e = *p.Events
		}
		e.Button = maps.Clone(e.Button)
		if e.Button == nil {
			e.Button = map[string]string{}
		}
		e.Button[failure] = "long_press"
		p.Events = &e
	}
}

// BuiltinTypes - типы Home Connect со встроенным профилем.
func BuiltinTypes() []string {
	return slices.Sorted(maps.Keys(builtin))
}

// BuiltinProfile - копия встроенного профиля типа или nil, если его нет.
func BuiltinProfile(haType string) *Profile {
	if b, ok := builtin[haType]; ok {
		return b.clone()
	}
	return nil
}
