package profile

import (
	"slices"
	"strings"
)

// Встроенные профили по типам LG ThinQ Connect. Имя функции прибора LG - путь
// «ресурс.свойство», значения - как в ThinQ Connect, заглавными. Ресурсы и значения взяты
// из библиотеки LG pythinqconnect и интеграции Home Assistant; на приборах проверен только
// профиль стиральной машины.

func init() {
	for lgType, p := range lgBuiltin {
		builtin[lgType] = p
	}
}

// LGType сообщает, что тип - тип прибора LG: в ThinQ Connect все типы начинаются с DEVICE_.
func LGType(t string) bool {
	return strings.HasPrefix(t, "DEVICE_")
}

const (
	percent = "unit.percent"
	density = "unit.density.mcg_m3"
	// lgRemote - разрешено ли удалённое управление: без него прибор не запускается.
	lgRemote = "remoteControlEnable.remoteControlEnabled"
	lgState  = "runState.currentState"
)

// lgIdle - состояния, в которых прибор не работает. Остальные - работа, в том числе пауза
// и отложенный старт: полного списка состояний LG не публикует.
var lgIdle = []string{"POWER_OFF", "INITIAL", "END", "ERROR", "SLEEP", "STANDBY"}

// lgOvenIdle - у духовки конец готовки и самоочистки - свои состояния.
var lgOvenIdle = slices.Concat(lgIdle, []string{"DONE", "CLEANING_IS_DONE"})

// lgStylerIdle - у стайлера конец работы - COMPLETE и RUNNING_END, а не END.
var lgStylerIdle = slices.Concat(lgIdle, []string{"COMPLETE", "RUNNING_END"})

// lgEvents - уведомления LG кнопкой прибора: окончание работы, например CLEANING_IS_COMPLETED или
// FILTER_RESET_COMPLETE, - click, разогрев духовки - double_click, остальные коды - long_press.
var lgEvents = &Events{Button: map[string]string{
	"*_COMPLETE":             "click",
	"*_COMPLETED":            "click",
	"PREHEATING_IS_COMPLETE": "double_click",
	"*":                      "long_press",
}}

func lgPower(setting string) *Power {
	return &Power{Setting: setting, On: "POWER_ON", Off: "POWER_OFF"}
}

// lgRun - запуск и остановка работы: прибор включён, пока идёт работа.
func lgRun(setting, off string, idle []string) *Power {
	return &Power{Setting: setting, On: "START", Off: off, State: lgState, Idle: idle, Allowed: lgRemote}
}

func lgFan(option string) Mode {
	return Mode{Instance: "fan_speed", Option: option, Values: map[string]string{
		"LOW": "low", "MID": "medium", "HIGH": "high", "AUTO": "auto", "POWER": "turbo",
	}}
}

func lgFloat(instance, status, unit string) Property {
	return Property{Type: "float", Instance: instance, Status: status, Unit: unit}
}

// lgAir - датчики качества воздуха.
var lgAir = []Property{
	lgFloat("humidity", "airQualitySensor.humidity", percent),
	lgFloat("pm1_density", "airQualitySensor.PM1", density),
	lgFloat("pm2.5_density", "airQualitySensor.PM2", density),
	lgFloat("pm10_density", "airQualitySensor.PM10", density),
}

var lgDoor = Property{Type: "event", Instance: "open", Status: "doorStatus.doorState",
	Values: map[string]string{"OPEN": "opened", "CLOSE": "closed"}}

// lgLaundry - стиральные и сушильные машины и стайлер. Окончание работы - переход в END, а у
// стайлера в свои состояния: уведомление об окончании второго click не даёт, поэтому кнопкой
// идут только ошибки.
func lgLaundry(operation string, idle []string, end ...string) *Profile {
	button := map[string]string{
		"ERROR_*":       "long_press",
		"DRYING_FAILED": "long_press",
	}
	for _, state := range end {
		button[lgState+"="+state] = "click"
	}
	return &Profile{
		YandexType: "washing_machine",
		OnOff:      "power",
		Power:      lgRun("operation."+operation, "STOP", idle),
		Events:     &Events{Button: button},
	}
}

// lgWashing - стиральная или сушильная машина: работа кончается переходом в END.
func lgWashing(operation string) *Profile {
	return lgLaundry(operation, lgIdle, "END")
}

// lgHoodFan - скорость вытяжки: число от 0, а режим - только от 1. lgLamp - подсветка вытяжки по
// свойству шлюза lamp.lampEnabled: горит при любой яркости, кроме 0.
var (
	lgHoodFan = Mode{Instance: "fan_speed", Option: "ventilation.fanSpeed", Values: map[string]string{
		"1": "one", "2": "two", "3": "three", "4": "four", "5": "five",
	}}
	lgLamp = Toggle{Instance: "backlight", Setting: "lamp.lampEnabled", Optional: true}
)

var lgBuiltin = map[string]*Profile{
	"DEVICE_WASHER":           lgWashing("washerOperationMode"),
	"DEVICE_WASHTOWER":        lgWashing("washerOperationMode"),
	"DEVICE_WASHTOWER_WASHER": lgWashing("washerOperationMode"),
	"DEVICE_WASHCOMBO_MAIN":   lgWashing("washerOperationMode"),
	"DEVICE_WASHCOMBO_MINI":   lgWashing("washerOperationMode"),
	"DEVICE_DRYER":            lgWashing("dryerOperationMode"),
	"DEVICE_WASHTOWER_DRYER":  lgWashing("dryerOperationMode"),
	"DEVICE_STYLER":           lgLaundry("stylerOperationMode", lgStylerIdle, "COMPLETE", "RUNNING_END"),
	"DEVICE_DISH_WASHER": {
		YandexType: "dishwasher",
		OnOff:      "power",
		Power:      lgRun("operation.dishWasherOperationMode", "STOP", lgIdle),
		Properties: []Property{lgDoor, {Type: "event", Instance: "water_level", Status: "dishWashingStatus.rinseRefill",
			Values: map[string]string{"true": "low", "false": "normal"}}},
		Events: lgEvents,
	},
	"DEVICE_AIR_CONDITIONER": {
		YandexType: "thermostat.ac",
		OnOff:      "power",
		Power:      lgPower("operation.airConOperationMode"),
		Modes: []Mode{{Instance: "thermostat", Option: "airConJobMode.currentJobMode", Values: map[string]string{
			"COOL": "cool", "HEAT": "heat", "AIR_DRY": "dry", "FAN": "fan_only", "AUTO": "auto",
		}}, lgFan("airFlow.windStrength")},
		Ranges:     []Range{{Instance: "temperature", Option: "temperatureInUnits.targetTemperatureC", Unit: celsius}},
		Toggles:    []Toggle{{Instance: "oscillation", Setting: "windDirection.rotateUpDown", Optional: true}},
		Properties: append([]Property{lgFloat("temperature", "temperatureInUnits.currentTemperatureC", celsius)}, lgAir...),
		Events:     lgEvents,
	},
	"DEVICE_AIR_PURIFIER": {
		YandexType: "purifier",
		OnOff:      "power",
		Power:      lgPower("operation.airPurifierOperationMode"),
		Modes: []Mode{{Instance: "program", Option: "airPurifierJobMode.currentJobMode", Values: map[string]string{
			"AUTO": "auto", "CLEAN": "normal", "SLEEP": "quiet",
		}}, lgFan("airFlow.windStrength")},
		Properties: lgAir,
		Events:     lgEvents,
	},
	"DEVICE_AIR_PURIFIER_FAN": {
		YandexType: "purifier",
		OnOff:      "power",
		Power:      lgPower("operation.airFanOperationMode"),
		// Режимы сопоставлены приблизительно: точных значений у Алисы для них нет.
		Modes: []Mode{{Instance: "program", Option: "airFanJobMode.currentJobMode", Values: map[string]string{
			"SPACE_CLEAN": "normal", "SPOT_CLEAN": "express", "DIRECT_CLEAN": "turbo", "NATURE_CLEAN": "auto",
		}}, lgFan("airFlow.windStrength")},
		Properties: append([]Property{lgFloat("temperature", "airQualitySensor.temperature", celsius)}, lgAir...),
		Events:     lgEvents,
	},
	"DEVICE_HUMIDIFIER": {
		YandexType: "humidifier",
		OnOff:      "power",
		Power:      lgPower("operation.humidifierOperationMode"),
		Modes:      []Mode{lgFan("airFlow.windStrength")},
		Ranges:     []Range{{Instance: "humidity", Option: "humidity.targetHumidity", Unit: percent}},
		Toggles:    []Toggle{{Instance: "backlight", Setting: "moodLamp.moodLampState", On: "ON", Off: "OFF", Optional: true}},
		Properties: []Property{lgFloat("humidity", "airQualitySensor.humidity", percent),
			lgFloat("temperature", "airQualitySensor.temperature", celsius)},
		Events: lgEvents,
	},
	"DEVICE_DEHUMIDIFIER": {
		YandexType: "humidifier",
		OnOff:      "power",
		Power:      lgPower("operation.dehumidifierOperationMode"),
		Modes:      []Mode{lgFan("airFlow.windStrength")},
		Ranges:     []Range{{Instance: "humidity", Option: "humidity.targetHumidity", Unit: percent}},
		Properties: []Property{lgFloat("humidity", "humidity.currentHumidity", percent)},
		Events:     lgEvents,
	},
	"DEVICE_VENTILATOR": {
		YandexType: "ventilation",
		OnOff:      "power",
		Power:      lgPower("operation.ventOperationMode"),
		Modes: []Mode{{Instance: "program", Option: "ventJobMode.currentJobMode", Values: map[string]string{
			"VENT_AUTO": "auto", "VENT_NATURE": "normal", "VENT_HEAT_EXCHANGE": "heat",
		}}, lgFan("airFlow.windStrength")},
		// CO2 прибор сообщает уровнем, а не числом ppm: свойства Алисы для него нет.
		Properties: []Property{
			lgFloat("pm1_density", "airQualitySensor.PM1", density), lgFloat("pm2.5_density", "airQualitySensor.PM2", density),
			lgFloat("pm10_density", "airQualitySensor.PM10", density),
			lgFloat("temperature", "temperature.currentTemperature", celsius)},
		Events: lgEvents,
	},
	"DEVICE_CEILING_FAN": {
		YandexType: "ventilation.fan",
		OnOff:      "power",
		Power:      lgPower("operation.ceilingfanOperationMode"),
		Modes: []Mode{{Instance: "fan_speed", Option: "airFlow.windStrength", Values: map[string]string{
			"LOW": "low", "MID": "medium", "HIGH": "high", "TURBO": "turbo",
		}}},
	},
	"DEVICE_HOOD": {
		YandexType: "ventilation",
		Modes:      []Mode{lgHoodFan},
		Toggles:    []Toggle{lgLamp},
		Events:     lgEvents,
	},
	"DEVICE_MICROWAVE_OVEN": {
		YandexType: "cooking",
		Modes:      []Mode{lgHoodFan},
		Toggles:    []Toggle{lgLamp},
		Events:     lgEvents,
	},
	"DEVICE_OVEN": {
		YandexType: "cooking",
		OnOff:      "power",
		Power:      lgRun("operation.ovenOperationMode", "STOP", lgOvenIdle),
		// Температура уходит только настройкой, без START: голос не запускает нагрев сам.
		Ranges: []Range{{Instance: "temperature", Option: "temperature.targetTemperatureC", Unit: celsius}},
		Events: lgEvents,
	},
	"DEVICE_REFRIGERATOR": {
		YandexType: "cooking",
		Ranges:     []Range{{Instance: "temperature", Option: "temperatureInUnits.targetTemperatureC", Unit: celsius}},
		Properties: []Property{lgDoor},
		Events:     lgEvents,
	},
	// Температуру камеры кимчи прибор только сообщает, и это заданная, а не измеренная температура.
	// У камеры с текстовым режимом, например KIMCHI, значения нет.
	"DEVICE_KIMCHI_REFRIGERATOR": {
		YandexType: "cooking",
		Properties: []Property{lgFloat("temperature", "temperature.targetTemperature", celsius)},
		Events:     lgEvents,
	},
	"DEVICE_WINE_CELLAR": {
		YandexType: "cooking",
		Ranges: []Range{{Instance: "temperature", Option: "temperatureInUnits.targetTemperatureC", Unit: celsius},
			{Instance: "brightness", Option: "operation.lightStatus", Unit: percent}},
		Events: lgEvents,
	},
	"DEVICE_WATER_HEATER": {
		YandexType: "thermostat",
		OnOff:      "power",
		Power:      lgPower("operation.waterHeaterOperationMode"),
		Modes: []Mode{{Instance: "heat", Option: "waterHeaterJobMode.currentJobMode", Values: map[string]string{
			"AUTO": "auto", "TURBO": "turbo",
		}}},
		Ranges:     []Range{{Instance: "temperature", Option: "temperatureInUnits.targetTemperatureC", Unit: celsius}},
		Properties: []Property{lgFloat("temperature", "temperatureInUnits.currentTemperatureC", celsius)},
		Events:     lgEvents,
	},
	"DEVICE_SYSTEM_BOILER": {
		YandexType: "thermostat",
		OnOff:      "power",
		Power:      lgPower("operation.boilerOperationMode"),
		Modes: []Mode{{Instance: "thermostat", Option: "boilerJobMode.currentJobMode", Values: map[string]string{
			"HEAT": "heat", "COOL": "cool", "AUTO": "auto",
		}}},
		Ranges:     []Range{{Instance: "temperature", Option: "roomTemperatureInUnits.targetTemperatureC", Unit: celsius}},
		Properties: []Property{lgFloat("temperature", "roomTemperatureInUnits.currentTemperatureC", celsius)},
		Events:     lgEvents,
	},
	"DEVICE_ROBOT_CLEANER": {
		YandexType: "vacuum_cleaner",
		OnOff:      "power",
		Power: &Power{Setting: "operation.cleanOperationMode", On: "START", Off: "HOMING", State: lgState,
			Idle: []string{"CHARGING", "HOMING", "SLEEP", "STANDBY", "INITIALIZING", "DIAGNOSIS", "ERROR",
				"MACROSECTOR", "MONITORING_DETECTING", "MONITORING_MOVING", "MONITORING_POSITIONING", "RESERVATION", "SETDATE"}},
		Properties: []Property{lgFloat("battery_level", "battery.percent", percent)},
		Events:     lgEvents,
	},
	"DEVICE_STICK_CLEANER": {
		YandexType: "vacuum_cleaner",
		Properties: []Property{lgFloat("battery_level", "battery.percent", percent)},
		Events:     lgEvents,
	},
}
