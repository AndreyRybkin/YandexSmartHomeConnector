package profile

import "strings"

// Встроенные профили устройств SmartThings по категории устройства. Тип - приставка
// SmartThings. и категория, имя функции - «capability.атрибут», например switch.switch.
// Capability и значения взяты из справочника SmartThings и интеграции Home Assistant; на
// устройствах профили не проверены. Свойства и переключатели, которых устройство не
// сообщает, в Алису не попадают, поэтому одни и те же свойства подходят разным датчикам.

// SmartThingsPrefix - приставка типа устройства SmartThings.
const SmartThingsPrefix = "SmartThings."

// stThermostatSetpoint - уставка термостата по его режиму: при cool - уставка охлаждения, при
// остальных - нагрева. Значение считает сам прибор SmartThings, у устройства такого атрибута нет.
const stThermostatSetpoint = "hcgateway.thermostatSetpoint"

// SmartThingsType сообщает, что тип - тип устройства SmartThings.
func SmartThingsType(t string) bool {
	return strings.HasPrefix(t, SmartThingsPrefix)
}

func init() {
	for t, p := range stBuiltin {
		builtin[SmartThingsPrefix+t] = p
	}
}

func stPower() *Power {
	return &Power{Setting: "switch.switch", On: "on", Off: "off"}
}

func stEvent(instance, status string, values map[string]string) Property {
	return Property{Type: "event", Instance: instance, Status: status, Values: values}
}

// stSensors - показания и события датчиков, счётчиков и климатической техники.
var stSensors = []Property{
	stEvent("open", "contactSensor.contact", map[string]string{"open": "opened", "closed": "closed"}),
	stEvent("motion", "motionSensor.motion", map[string]string{"active": "detected", "inactive": "not_detected"}),
	stEvent("water_leak", "waterSensor.water", map[string]string{"wet": "leak", "dry": "dry"}),
	stEvent("smoke", "smokeDetector.smoke", map[string]string{"detected": "detected", "clear": "not_detected"}),
	lgFloat("temperature", "temperatureMeasurement.temperature", celsius),
	lgFloat("humidity", "relativeHumidityMeasurement.humidity", percent),
	lgFloat("illumination", "illuminanceMeasurement.illuminance", "unit.illumination.lux"),
	lgFloat("battery_level", "battery.battery", percent),
	lgFloat("co2_level", "carbonDioxideMeasurement.carbonDioxide", "unit.ppm"),
	lgFloat("pm1_density", "veryFineDustSensor.veryFineDustLevel", density),
	lgFloat("pm2.5_density", "dustSensor.fineDustLevel", density),
	lgFloat("pm10_density", "dustSensor.dustLevel", density),
	lgFloat("power", "powerMeter.power", "unit.watt"),
	lgFloat("electricity_meter", "energyMeter.energy", "unit.kilowatt_hour"),
	lgFloat("voltage", "voltageMeasurement.voltage", "unit.volt"),
}

func stSensor(yandexType string) *Profile {
	return &Profile{YandexType: yandexType, Properties: stSensors}
}

// stMedia - телевизор, колонка или ресивер: питание, громкость, звук и пауза.
func stMedia(yandexType string) *Profile {
	return &Profile{
		YandexType: yandexType,
		OnOff:      "power",
		Power:      stPower(),
		Ranges:     []Range{{Instance: "volume", Option: "audioVolume.volume", Unit: percent}},
		Toggles: []Toggle{{Instance: "mute", Setting: "audioMute.mute", On: "muted", Off: "unmuted"},
			{Instance: "pause", Setting: "mediaPlayback.playbackStatus", On: "paused", Off: "playing"}},
	}
}

// stLaundry - стиральная, сушильная или посудомоечная машина: запуск и остановка через
// состояние машины, пауза и окончание работы - кнопка: job перешёл в конечное значение.
// Кнопка - свойством: так машина без этого значения её не получит.
func stLaundry(yandexType, capability, job, end string) *Profile {
	state := capability + ".machineState"
	return &Profile{
		YandexType: yandexType,
		OnOff:      "power",
		Power: &Power{Setting: state, On: "run", Off: "stop", State: state, Idle: []string{"stop"},
			Allowed: "remoteControlStatus.remoteControlEnabled"},
		Toggles:    []Toggle{{Instance: "pause", Setting: state, On: "pause", Off: "run"}},
		Properties: []Property{stEvent("button", capability+"."+job, map[string]string{end: "click"})},
	}
}

func stFridge() *Profile {
	return &Profile{
		YandexType: "cooking",
		Ranges:     []Range{{Instance: "temperature", Option: "thermostatCoolingSetpoint.coolingSetpoint", Unit: celsius}},
		Properties: []Property{stSensors[0], stSensors[4]},
	}
}

func stOven() *Profile {
	return &Profile{
		YandexType: "cooking",
		Ranges:     []Range{{Instance: "temperature", Option: "ovenSetpoint.ovenSetpoint", Unit: celsius}},
		Properties: []Property{stSensors[4]},
	}
}

// stShade - штора или окно: «включи» открывает, «выключи» закрывает; включено, пока не закрыто.
func stShade() *Profile {
	return &Profile{
		YandexType: "openable.curtain",
		OnOff:      "power",
		Power: &Power{Setting: "windowShade.windowShade", On: "open", Off: "close", State: "windowShade.windowShade",
			Idle: []string{"closed"}},
		Ranges: []Range{{Instance: "open", Option: "windowShadeLevel.shadeLevel", Unit: percent}},
	}
}

var channelMin, channelMax = 1.0, 999.0

var stBuiltin = map[string]*Profile{
	"Light": {
		YandexType: "light",
		OnOff:      "power",
		Power:      stPower(),
		Ranges:     []Range{{Instance: "brightness", Option: "switchLevel.level", Unit: percent}},
		Color: &Color{Setting: "colorControl.setColor", Hue: "colorControl.hue", Saturation: "colorControl.saturation",
			HueMax: ptr(100.0), Temperature: "colorTemperature.colorTemperature"},
	},
	"Switch":    {YandexType: "switch", OnOff: "power", Power: stPower(), Properties: stSensors},
	"SmartPlug": {YandexType: "socket", OnOff: "power", Power: stPower(), Properties: stSensors},

	"ContactSensor":          stSensor("sensor.open"),
	"MotionSensor":           stSensor("sensor.motion"),
	"LeakSensor":             stSensor("sensor.water_leak"),
	"SmokeDetector":          stSensor("sensor.smoke"),
	"TempSensor":             stSensor("sensor.climate"),
	"TempHumiditySensor":     stSensor("sensor.climate"),
	"HumiditySensor":         stSensor("sensor.climate"),
	"EnvironmentSensor":      stSensor("sensor.climate"),
	"AirQualityDetector":     stSensor("sensor.climate"),
	"WeatherStation":         stSensor("sensor.climate"),
	"LightSensor":            stSensor("sensor.illumination"),
	"MultiFunctionalSensor":  stSensor("sensor"),
	"GenericSensor":          stSensor("sensor"),
	"PowerMeasurementSensor": stSensor("smart_meter.electricity"),
	"CurbPowerMeter":         stSensor("smart_meter.electricity"),
	"Button": {
		YandexType: "sensor.button",
		Properties: []Property{stEvent("button", "button.button", map[string]string{"pushed": "click", "double": "double_click",
			"pushed_2x": "double_click", "held": "long_press", "down_hold": "long_press"}), lgFloat("battery_level", "battery.battery", percent)},
	},

	"Thermostat": {
		YandexType: "thermostat",
		OnOff:      "power",
		Power: &Power{Setting: "thermostatMode.thermostatMode", On: "heat", Off: "off", State: "thermostatMode.thermostatMode",
			Idle: []string{"off"}},
		Modes: []Mode{{Instance: "thermostat", Option: "thermostatMode.thermostatMode", Values: map[string]string{
			"auto": "auto", "cool": "cool", "heat": "heat", "eco": "eco",
		}}},
		Ranges:     []Range{{Instance: "temperature", Option: stThermostatSetpoint, Unit: celsius}},
		Properties: stSensors,
	},
	"AirConditioner": {
		YandexType: "thermostat.ac",
		OnOff:      "power",
		Power:      stPower(),
		Modes: []Mode{{Instance: "thermostat", Option: "airConditionerMode.airConditionerMode", Values: map[string]string{
			"auto": "auto", "cool": "cool", "dry": "dry", "wind": "fan_only", "heat": "heat",
		}, AlsoValues: map[string]string{"fan": "fan_only", "fanOnly": "fan_only", "aIComfort": "auto"}},
			{Instance: "fan_speed", Option: "airConditionerFanMode.fanMode", Values: map[string]string{
				"auto": "auto", "low": "low", "medium": "medium", "high": "high", "turbo": "turbo",
			}},
			{Instance: "swing", Option: "fanOscillationMode.fanOscillationMode", Values: map[string]string{
				"fixed": "stationary", "vertical": "vertical", "horizontal": "horizontal", "all": "auto",
			}}},
		Ranges:     []Range{{Instance: "temperature", Option: "thermostatCoolingSetpoint.coolingSetpoint", Unit: celsius}},
		Properties: stSensors,
	},
	"AirPurifier": {
		YandexType: "purifier",
		OnOff:      "power",
		Power:      stPower(),
		Modes: []Mode{{Instance: "fan_speed", Option: "airPurifierFanMode.airPurifierFanMode",
			FallbackOption: "airConditionerFanMode.fanMode", Values: map[string]string{
				"auto": "auto", "low": "low", "medium": "medium", "high": "high", "sleep": "quiet",
			}}},
		Properties: stSensors,
	},
	"Humidifier":          {YandexType: "humidifier", OnOff: "power", Power: stPower(), Properties: stSensors},
	"Dehumidifier":        {YandexType: "humidifier", OnOff: "power", Power: stPower(), Properties: stSensors},
	"AirpurifyHumidifier": {YandexType: "humidifier", OnOff: "power", Power: stPower(), Properties: stSensors},
	"Fan": {
		YandexType: "ventilation.fan",
		OnOff:      "power",
		Power:      stPower(),
		Modes: []Mode{{Instance: "fan_speed", Option: "fanSpeed.fanSpeed", Values: map[string]string{
			"1": "low", "2": "medium", "3": "high", "4": "turbo",
		}}},
	},
	"Blind":        stShade(),
	"Shade":        stShade(),
	"WindowOpener": stShade(),
	"WaterValve": {
		YandexType: "openable.valve",
		OnOff:      "power",
		Power:      &Power{Setting: "valve.valve", On: "open", Off: "close", State: "valve.valve", Idle: []string{"closed"}},
	},

	"Television": func() *Profile {
		p := stMedia("media_device.tv")
		p.Modes = []Mode{{Instance: "input_source", Option: "samsungvd.mediaInputSource.inputSource", Values: map[string]string{
			"HDMI1": "one", "HDMI2": "two", "HDMI3": "three", "HDMI4": "four", "dtv": "five",
		}}}
		p.Ranges = append(p.Ranges, Range{Instance: "channel", Option: "tvChannel.tvChannel", Min: &channelMin, Max: &channelMax})
		return p
	}(),
	"Speaker":      stMedia("media_device"),
	"NetworkAudio": stMedia("media_device"),
	"SoundBar":     stMedia("media_device"),
	"MusicSystem":  stMedia("media_device"),
	"HomeTheater":  stMedia("media_device.receiver"),
	"Receiver":     stMedia("media_device.receiver"),
	"SetTop":       stMedia("media_device.tv_box"),

	"Washer":     stLaundry("washing_machine", "washerOperatingState", "washerJobState", "finish"),
	"Dryer":      stLaundry("washing_machine", "dryerOperatingState", "dryerJobState", "finished"),
	"Dishwasher": stLaundry("dishwasher", "dishwasherOperatingState", "dishwasherJobState", "finish"),

	"Refrigerator":       stFridge(),
	"KimchiRefrigerator": stFridge(),
	"CubeRefrigerator":   stFridge(),
	"WineCellar":         stFridge(),
	"Oven":               stOven(),
	"Range":              stOven(),
	"Microwave":          stOven(),

	"RobotCleaner": {
		YandexType: "vacuum_cleaner",
		OnOff:      "power",
		Power: &Power{Setting: "robotCleanerMovement.robotCleanerMovement", On: "cleaning", Off: "homing",
			State: "robotCleanerMovement.robotCleanerMovement",
			Idle:  []string{"homing", "idle", "charging", "powerOff", "alarm", "reserve", "after"}},
		Toggles:    []Toggle{{Instance: "pause", Setting: "robotCleanerMovement.robotCleanerMovement", On: "pause", Off: "cleaning"}},
		Properties: []Property{lgFloat("battery_level", "battery.battery", percent)},
	},
}

func ptr[T any](v T) *T { return &v }
