package profile

import "strings"

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

// lgEvents - уведомления LG кнопкой датчика: окончание работы - click, разогрев духовки -
// double_click, остальные коды - long_press.
var lgEvents = &Events{Button: map[string]string{
	"*_IS_COMPLETE":          "click",
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

// lgLaundry - стиральные и сушильные машины и стайлер. Окончание работы - переход в END:
// уведомление об окончании второго click не даёт, поэтому кнопкой идут только ошибки.
func lgLaundry(operation string) *Profile {
	return &Profile{
		YandexType: "washing_machine",
		OnOff:      "power",
		Power:      lgRun("operation."+operation, "STOP", lgIdle),
		Events: &Events{Button: map[string]string{
			lgState + "=END":       "click",
			"ERROR_DURING_WASHING": "long_press",
			"DRYING_FAILED":        "long_press",
			"ERROR_HAS_OCCURRED":   "long_press",
		}},
	}
}

func lgCompartment(temperature string) *Profile {
	return &Profile{
		YandexType: "cooking",
		Ranges:     []Range{{Instance: "temperature", Option: temperature, Unit: celsius}},
		Properties: []Property{lgDoor},
		Events:     lgEvents,
	}
}

var lgBuiltin = map[string]*Profile{
	"DEVICE_WASHER":           lgLaundry("washerOperationMode"),
	"DEVICE_WASHTOWER":        lgLaundry("washerOperationMode"),
	"DEVICE_WASHTOWER_WASHER": lgLaundry("washerOperationMode"),
	"DEVICE_WASHCOMBO_MAIN":   lgLaundry("washerOperationMode"),
	"DEVICE_WASHCOMBO_MINI":   lgLaundry("washerOperationMode"),
	"DEVICE_DRYER":            lgLaundry("dryerOperationMode"),
	"DEVICE_WASHTOWER_DRYER":  lgLaundry("dryerOperationMode"),
	"DEVICE_STYLER":           lgLaundry("stylerOperationMode"),
	"DEVICE_DISH_WASHER": {
		YandexType: "dishwasher",
		OnOff:      "power",
		Power:      lgRun("operation.dishWasherOperationMode", "STOP", lgIdle),
		Properties: []Property{lgDoor, {Type: "event", Instance: "food_level", Status: "dishWashingStatus.rinseRefill",
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
		Toggles:    []Toggle{{Instance: "oscillation", Setting: "windDirection.rotateUpDown"}},
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
		Modes: []Mode{{Instance: "program", Option: "airFanJobMode.currentJobMode", Values: map[string]string{
			"AUTO": "auto", "CLEAN": "normal", "SLEEP": "quiet",
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
		Toggles:    []Toggle{{Instance: "backlight", Setting: "moodLamp.moodLampState"}},
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
		Power:      lgPower("operation.ventilatorOperationMode"),
		Modes: []Mode{{Instance: "program", Option: "ventilatorJobMode.currentJobMode", Values: map[string]string{
			"AUTO": "auto",
		}}, lgFan("airFlow.windStrength")},
		Properties: []Property{lgFloat("co2_level", "airQualitySensor.co2", "unit.ppm"),
			lgFloat("pm1_density", "airQualitySensor.PM1", density), lgFloat("pm2.5_density", "airQualitySensor.PM2", density),
			lgFloat("pm10_density", "airQualitySensor.PM10", density),
			lgFloat("temperature", "temperature.currentTemperature", celsius)},
		Events: lgEvents,
	},
	"DEVICE_CEILING_FAN": {
		YandexType: "ventilation.fan",
		OnOff:      "power",
		Power:      lgPower("operation.ceilingFanOperationMode"),
		Modes:      []Mode{lgFan("airFlow.windStrength")},
	},
	"DEVICE_HOOD": {
		YandexType: "ventilation",
		Modes: []Mode{{Instance: "fan_speed", Option: "ventilation.fanSpeed", Values: map[string]string{
			"1": "one", "2": "two", "3": "three", "4": "four", "5": "five",
		}}},
		Toggles: []Toggle{{Instance: "backlight", Setting: "lamp.lampBrightness", On: "1", Off: "0"}},
		Events:  lgEvents,
	},
	"DEVICE_MICROWAVE_OVEN": {
		YandexType: "cooking",
		Modes: []Mode{{Instance: "fan_speed", Option: "ventilation.fanSpeed", Values: map[string]string{
			"1": "one", "2": "two", "3": "three", "4": "four", "5": "five",
		}}},
		Toggles: []Toggle{{Instance: "backlight", Setting: "lamp.lampBrightness", On: "1", Off: "0"}},
		Events:  lgEvents,
	},
	"DEVICE_OVEN": {
		YandexType: "cooking",
		OnOff:      "power",
		Power:      lgRun("operation.ovenOperationMode", "STOP", lgIdle),
		Ranges:     []Range{{Instance: "temperature", Option: "temperatureInUnits.targetTemperatureC", Unit: celsius}},
		Events:     lgEvents,
	},
	"DEVICE_REFRIGERATOR":        lgCompartment("temperatureInUnits.targetTemperatureC"),
	"DEVICE_KIMCHI_REFRIGERATOR": lgCompartment("temperature.targetTemperature"),
	"DEVICE_WINE_CELLAR": func() *Profile {
		p := lgCompartment("temperatureInUnits.targetTemperatureC")
		p.Toggles = []Toggle{{Instance: "backlight", Setting: "operation.lightStatus", On: "ON", Off: "OFF"}}
		return p
	}(),
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
		Ranges:     []Range{{Instance: "temperature", Option: "roomTemperatureInUnits.roomTargetTemperatureC", Unit: celsius}},
		Properties: []Property{lgFloat("temperature", "roomTemperatureInUnits.roomCurrentTemperatureC", celsius)},
		Events:     lgEvents,
	},
	"DEVICE_ROBOT_CLEANER": {
		YandexType: "vacuum_cleaner",
		OnOff:      "power",
		Power: &Power{Setting: "operation.cleanOperationMode", On: "START", Off: "HOMING", State: lgState,
			Idle: []string{"CHARGING", "HOMING", "SLEEP", "STANDBY", "INITIALIZING", "DIAGNOSIS", "ERROR"}},
		Properties: []Property{lgFloat("battery_level", "battery.batteryPercent", percent)},
		Events:     lgEvents,
	},
	"DEVICE_STICK_CLEANER": {
		YandexType: "vacuum_cleaner",
		Properties: []Property{lgFloat("battery_level", "battery.batteryPercent", percent)},
		Events:     lgEvents,
	},
}
