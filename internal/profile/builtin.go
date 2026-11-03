package profile

import (
	"maps"
	"slices"
)

// Встроенные профили по типам Home Connect. Имена программ и параметров взяты
// из документации Home Connect, на настоящих приборах профили пока не проверены.

const celsius = "unit.temperature.celsius"

var (
	doorOpen = Property{Type: "event", Instance: "open", Status: "BSH.Common.Status.DoorState",
		Values: map[string]string{"BSH.Common.EnumType.DoorState.Open": "opened", "BSH.Common.EnumType.DoorState.Closed": "closed"}}
	pause    = Toggle{Instance: "pause"}
	finished = map[string]string{"BSH.Common.Event.ProgramFinished": "click"}
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
	Properties: []Property{doorOpen},
	Events:     &Events{Button: finished},
}

var fridge = func(setting, door string) *Profile {
	return &Profile{
		YandexType: "cooking",
		OnOff:      "none",
		Ranges:     []Range{{Instance: "temperature", Option: setting, Unit: celsius}},
		Properties: []Property{{Type: "event", Instance: "open", Status: door,
			Values: doorOpen.Values}},
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
			"BSH.Common.Event.ProgramFinished":   "click",
			"Cooking.Oven.Event.PreheatFinished": "double_click",
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
		Properties: []Property{doorOpen},
		Events: &Events{
			Button: finished,
			// NearlyEmpty - скоро закончится, Lack - уже нет: прибор сообщает их по отдельности.
			FoodLevel:  []string{"Dishcare.Dishwasher.Event.SaltNearlyEmpty", "Dishcare.Dishwasher.Event.SaltLack"},
			WaterLevel: []string{"Dishcare.Dishwasher.Event.RinseAidNearlyEmpty", "Dishcare.Dishwasher.Event.RinseAidLack"},
			Empty:      []string{"Dishcare.Dishwasher.Event.SaltLack", "Dishcare.Dishwasher.Event.RinseAidLack"},
		},
	},
	"Washer":      washer,
	"WasherDryer": washer,
	"Dryer": {
		YandexType:     "washing_machine",
		OnOff:          "program",
		DefaultProgram: "LaundryCare.Dryer.Program.Cotton",
		Modes: []Mode{{Instance: "program", Programs: programs("LaundryCare.Dryer.Program.", map[string]string{
			"Cotton": "normal", "Synthetic": "one", "Mix": "two",
		})}},
		Toggles:    []Toggle{pause},
		Properties: []Property{doorOpen},
		Events:     &Events{Button: finished},
	},
	"CoffeeMaker": {
		YandexType: "cooking.coffee_maker",
		OnOff:      "power",
		Modes: []Mode{{Instance: "coffee_mode", Programs: programs("ConsumerProducts.CoffeeMaker.Program.Beverage.", map[string]string{
			"Espresso": "espresso", "Coffee": "americano", "Cappuccino": "cappuccino", "CaffeLatte": "latte",
			"LatteMacchiato": "one",
		})}},
		Events: &Events{
			FoodLevel:  []string{"ConsumerProducts.CoffeeMaker.Event.BeanContainerEmpty"},
			WaterLevel: []string{"ConsumerProducts.CoffeeMaker.Event.WaterTankEmpty"},
			Empty:      []string{"ConsumerProducts.CoffeeMaker.Event.BeanContainerEmpty", "ConsumerProducts.CoffeeMaker.Event.WaterTankEmpty"},
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
		Toggles: []Toggle{{Instance: "backlight", Setting: "Cooking.Common.Setting.Lighting"}},
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
	// Варочную панель и кухонный процессор облако только показывает: программ для запуска в
	// API нет, а включит ли их облако по питанию, не проверено.
	"Hob":           {YandexType: "cooking", OnOff: "power"},
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
		Events:  &Events{Button: finished},
	},
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
