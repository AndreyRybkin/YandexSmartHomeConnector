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
	// spinSpeed - скорость отжима. RPM600 и RPM1000 остаются без пары: у Алисы нет таких значений.
	spinSpeed = Mode{Instance: "work_speed", Option: "LaundryCare.Washer.Option.SpinSpeed", Values: map[string]string{
		"LaundryCare.Washer.EnumType.SpinSpeed.RPM400":  "slow",
		"LaundryCare.Washer.EnumType.SpinSpeed.RPM800":  "medium",
		"LaundryCare.Washer.EnumType.SpinSpeed.RPM1200": "fast",
		"LaundryCare.Washer.EnumType.SpinSpeed.RPM1400": "turbo",
		"LaundryCare.Washer.EnumType.SpinSpeed.RPM1600": "max",
	}}
	// iDosLevel - мало средства в дозаторе i-Dos: средство не вода, но так же устроен ополаскиватель.
	iDosLevel = []string{"LaundryCare.Washer.Event.IDos1FillLevelPoor", "LaundryCare.Washer.Event.IDos2FillLevelPoor"}
	// dryingTarget - степень сушки. У Алисы нет своего имени для неё, ближе всего режим нагрева.
	dryingTarget = Mode{Instance: "heat", Option: "LaundryCare.Dryer.Option.DryingTarget", Values: map[string]string{
		"LaundryCare.Dryer.EnumType.DryingTarget.GentleDry":       "min",
		"LaundryCare.Dryer.EnumType.DryingTarget.IronDry":         "low",
		"LaundryCare.Dryer.EnumType.DryingTarget.CupboardDry":     "normal",
		"LaundryCare.Dryer.EnumType.DryingTarget.CupboardDryPlus": "high",
		"LaundryCare.Dryer.EnumType.DryingTarget.ExtraDry":        "max",
	}}
	// Настройки стирки, которые есть не у всех приборов типа.
	drumLight = Toggle{Instance: "backlight", Setting: "LaundryCare.Common.Setting.DrumLight.Active", Optional: true}
	soundMute = Toggle{Instance: "mute", Setting: "LaundryCare.Common.Setting.Sound.Mute", Optional: true}
	// Тревоги холодильных приборов: дверь открыта слишком долго - double_click, морозильная камера
	// нагрелась - long_press. В домашней сети те же события называются иначе.
	refrigeratorAlarms = map[string]string{
		"Refrigeration.FridgeFreezer.Event.DoorAlarmRefrigerator": "double_click",
		"Refrigeration.Common.Event.Door.AlarmRefrigerator":       "double_click",
	}
	freezerAlarms = map[string]string{
		"Refrigeration.FridgeFreezer.Event.DoorAlarmFreezer":        "double_click",
		"Refrigeration.Common.Event.Door.AlarmFreezer":              "double_click",
		"Refrigeration.FridgeFreezer.Event.TemperatureAlarmFreezer": "long_press",
		"Refrigeration.Common.Event.Freezer.TemperatureAlarm":       "long_press",
	}
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
		"Cotton.CottonEco": "eco", "Mix.NightWash": "quiet", "Auto40": "six", "Auto60": "seven",
		"Towels": "eight", "ShirtsBlouses": "nine", "SportFitness": "ten",
	})}, spinSpeed},
	Toggles:    []Toggle{pause, drumLight, soundMute},
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
	})}, spinSpeed, dryingTarget},
	Toggles:    []Toggle{pause, drumLight, soundMute},
	Properties: []Property{doorOpen, aquaStop},
	Events:     &Events{Button: laundryButton, WaterLevelIfKnown: iDosLevel},
}

// Варочную панель и кухонный процессор облако только показывает: программ для запуска в
// API нет, а включит ли их облако по питанию, не проверено.
var hob = &Profile{
	YandexType: "cooking",
	OnOff:      "power",
	// Вытяжка, встроенная в панель. Значения Off и AfterRun остаются без пары: у Алисы нет таких режимов.
	Modes: []Mode{{Instance: "fan_speed", Option: "Cooking.Hob.Setting.Ventilation", Values: map[string]string{
		"Cooking.Hob.EnumType.Ventilation.Level01":     "one",
		"Cooking.Hob.EnumType.Ventilation.Level03":     "two",
		"Cooking.Hob.EnumType.Ventilation.Level05":     "three",
		"Cooking.Hob.EnumType.Ventilation.Level07":     "four",
		"Cooking.Hob.EnumType.Ventilation.Level09":     "five",
		"Cooking.Hob.EnumType.Ventilation.Level11":     "six",
		"Cooking.Hob.EnumType.Ventilation.Level13":     "seven",
		"Cooking.Hob.EnumType.Ventilation.Level15":     "eight",
		"Cooking.Hob.EnumType.Ventilation.Level17":     "nine",
		"Cooking.Hob.EnumType.Ventilation.BoostLevel1": "turbo",
		"Cooking.Hob.EnumType.Ventilation.BoostLevel2": "max",
	}}},
	Events: &Events{Button: map[string]string{
		// Кнопка избранного на самом приборе: действие назначают в приложении Home Connect.
		"BSH.Common.Event.Favorite.001.ExternalTrigger": "click",
		"BSH.Common.Event.Favorite.002.ExternalTrigger": "long_press",
		"BSH.Common.Event.AlarmClockElapsed":            "double_click",
	}},
}

// fridge - холодильный прибор. fallback - другое имя настройки температуры: в домашней сети модели
// сообщают то одно, то другое.
var fridge = func(setting, fallback, door string, alarms ...map[string]string) *Profile {
	p := &Profile{
		YandexType: "cooking",
		OnOff:      "none",
		// Пределы яркости - только из описания прибора: заданные в профиле создали бы умение и у прибора
		// без этой настройки.
		Ranges: []Range{{Instance: "temperature", Option: setting, FallbackOption: fallback, Unit: celsius},
			{Instance: "brightness", Option: "Refrigeration.Common.Setting.Light.Internal.Brightness", Unit: percent}},
		Toggles: []Toggle{{Instance: "backlight", Setting: "Refrigeration.Common.Setting.Light.Internal.Power", Optional: true}},
		Properties: []Property{{Type: "event", Instance: "open", Status: door,
			Values: fridgeDoorValues}},
	}
	if len(alarms) > 0 {
		button := map[string]string{}
		for _, a := range alarms {
			maps.Copy(button, a)
		}
		p.Events = &Events{Button: button}
	}
	return p
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
			"HotAirGrilling": "two", "GrillSmallArea": "three", "BottomHeating": "four", "TopBottomHeatingEco": "five",
			"IntensiveHeat": "six", "FrozenHeatupSpecial": "ten", "HotAir100Steam": "steam",
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
			"Auto1": "one", "Auto3": "two", "GlassCare": "three", "MachineCare": "four", "Quick65": "five",
			"Normal45": "six", "Intensiv45": "seven", "AutoHalfLoad": "eight", "Super60": "nine", "MixedLoad": "ten",
		})}},
		Toggles:    []Toggle{pause, {Instance: "backlight", Setting: "Dishcare.Dishwasher.Setting.InfoLight", Optional: true}},
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
			"Super40": "express", "Towels": "three", "Jeans": "four", "Delicates": "five", "Hygiene": "six",
			"Blankets": "seven", "BusinessShirts": "eight", "TimeCold": "nine", "TimeWarm": "ten",
		})}, dryingTarget},
		Toggles:    []Toggle{pause, soundMute},
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
			"LatteMacchiato": "one", "EspressoDoppio": "double_espresso", "Ristretto": "two", "EspressoMacchiato": "three",
			"XLCoffee": "four", "CaffeGrande": "five", "MilkFroth": "six", "WarmMilk": "seven", "HotWater": "eight",
		})}},
		Toggles: []Toggle{{Instance: "keep_warm", Setting: "ConsumerProducts.CoffeeMaker.Setting.CupWarmer", Optional: true}},
		// Счётчик сваренного кофе: другие напитки он не считает.
		Properties: []Property{{Type: "float", Instance: "meter", Status: "ConsumerProducts.CoffeeMaker.Status.BeverageCounterCoffee"}},
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
		}}, {Instance: "work_speed", Option: "Cooking.Common.Option.Hood.IntensiveLevel", Values: map[string]string{
			"Cooking.Hood.EnumType.IntensiveStage.IntensiveStageOff": "normal",
			"Cooking.Hood.EnumType.IntensiveStage.IntensiveStage1":   "fast",
			"Cooking.Hood.EnumType.IntensiveStage.IntensiveStage2":   "turbo",
		}}},
		// Пределы яркости - только из описания прибора: заданные в профиле создали бы умение и у вытяжки
		// без этой настройки.
		Ranges:  []Range{{Instance: "brightness", Option: "Cooking.Common.Setting.LightingBrightness", FallbackOption: "Cooking.Hood.Setting.LightingBrightness", Unit: percent}},
		Toggles: []Toggle{{Instance: "backlight", Setting: "Cooking.Common.Setting.Lighting", Optional: true}},
		Events: &Events{Button: map[string]string{
			// Кнопка избранного на самом приборе: действие назначают в приложении Home Connect.
			"BSH.Common.Event.Favorite.001.ExternalTrigger": "click",
			"BSH.Common.Event.Favorite.002.ExternalTrigger": "long_press",
			// Жировой фильтр пора помыть.
			"Cooking.Common.Event.Hood.GreaseFilterMaxSaturationNearlyReached": "double_click",
			"Cooking.Common.Event.Hood.GreaseFilterMaxSaturationReached":       "double_click",
		}},
	},
	"Refrigerator":  fridge("Refrigeration.FridgeFreezer.Setting.SetpointTemperatureRefrigerator", "Refrigeration.Common.Setting.Refrigerator.SetpointTemperature", "Refrigeration.Common.Status.Door.Refrigerator", refrigeratorAlarms),
	"FridgeFreezer": fridge("Refrigeration.FridgeFreezer.Setting.SetpointTemperatureRefrigerator", "Refrigeration.Common.Setting.Refrigerator.SetpointTemperature", "Refrigeration.Common.Status.Door.Refrigerator", refrigeratorAlarms, freezerAlarms),
	"Freezer":       fridge("Refrigeration.FridgeFreezer.Setting.SetpointTemperatureFreezer", "Refrigeration.Common.Setting.Freezer.SetpointTemperature", "Refrigeration.Common.Status.Door.Freezer", freezerAlarms),
	"WineCooler":    fridge("Refrigeration.Common.Setting.WineCompartment.SetpointTemperature", "", "Refrigeration.Common.Status.Door.WineCompartment"),
	// Кондиционер включается питанием, а режим - программа: выбор режима сразу её запускает.
	"AirConditioner": {
		YandexType: "thermostat.ac",
		OnOff:      "power",
		Modes: []Mode{{Instance: "thermostat", Programs: programs("HeatingVentilationAirConditioning.AirConditioner.Program.", map[string]string{
			"Auto": "auto", "Cool": "cool", "Dry": "dry", "Fan": "fan_only", "Heat": "heat",
		})}, {Instance: "fan_speed", Option: "HeatingVentilationAirConditioning.AirConditioner.Option.FanSpeedMode", Values: map[string]string{
			"HeatingVentilationAirConditioning.AirConditioner.EnumType.FanSpeedMode.Automatic": "auto",
			"HeatingVentilationAirConditioning.AirConditioner.EnumType.FanSpeedMode.Manual":    "medium",
		}}},
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
		Events: &Events{Button: map[string]string{
			"BSH.Common.Event.ProgramFinished": "click",
		}},
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
	"Cooktop": hob,
	"Hob":     hob,
	"CookProcessor": {YandexType: "cooking.multicooker", OnOff: "power", Events: &Events{Button: map[string]string{
		"BSH.Common.Event.ProgramFinished": "click",
		"BSH.Common.Event.ProgramAborted":  "double_click",
	}}},
	"CleaningRobot": {
		YandexType:     "vacuum_cleaner",
		OnOff:          "program",
		DefaultProgram: "ConsumerProducts.CleaningRobot.Program.Cleaning.CleanAll",
		Modes: []Mode{{Instance: "program", Programs: map[string]string{
			"ConsumerProducts.CleaningRobot.Program.Cleaning.CleanAll": "auto",
			"ConsumerProducts.CleaningRobot.Program.Cleaning.CleanMap": "one",
		}}, {Instance: "cleanup_mode", Option: "ConsumerProducts.CleaningRobot.Option.CleaningMode", Values: map[string]string{
			// Серия Roxxter.
			"ConsumerProducts.CleaningRobot.EnumType.CleaningModes.Silent":   "quiet",
			"ConsumerProducts.CleaningRobot.EnumType.CleaningModes.Standard": "normal",
			"ConsumerProducts.CleaningRobot.EnumType.CleaningModes.Power":    "express",
			// Серия Spotless.
			"ConsumerProducts.CleaningRobot.EnumType.CleaningMode.IntelligentMode": "auto",
			"ConsumerProducts.CleaningRobot.EnumType.CleaningMode.VacuumOnly":      "dry_cleaning",
			"ConsumerProducts.CleaningRobot.EnumType.CleaningMode.MopOnly":         "wet_cleaning",
			"ConsumerProducts.CleaningRobot.EnumType.CleaningMode.VacuumAndMop":    "mixed_cleaning",
			"ConsumerProducts.CleaningRobot.EnumType.CleaningMode.MopAfterVacuum":  "smart",
		}}, {Instance: "work_speed", Option: "ConsumerProducts.CleaningRobot.Option.SuctionPower", Values: map[string]string{
			"ConsumerProducts.CleaningRobot.EnumType.SuctionPower.Silent":   "min",
			"ConsumerProducts.CleaningRobot.EnumType.SuctionPower.Standard": "medium",
			"ConsumerProducts.CleaningRobot.EnumType.SuctionPower.Max":      "max",
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

// Блокировка панели: настройка есть не у всех приборов типа.
func init() {
	for _, t := range []string{"Oven", "Dishwasher", "Washer", "WasherDryer", "Dryer", "CoffeeMaker", "Microwave", "Hob", "CookProcessor"} {
		p := builtin[t]
		p.Toggles = append(slices.Clip(p.Toggles), Toggle{Instance: "controls_locked", Setting: "BSH.Common.Setting.ChildLock", Optional: true})
	}
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
