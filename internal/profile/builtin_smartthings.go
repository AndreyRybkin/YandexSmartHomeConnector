package profile

import (
	"slices"
	"strings"
)

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
		p.Properties = withSensors(p.Properties)
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
	stEvent("motion", "motionSensor.motion", map[string]string{"active": "detected", "inactive": "not_detected",
		"present": "detected", "not present": "not_detected"}),
	stEvent("water_leak", "waterSensor.water", map[string]string{"wet": "leak", "dry": "dry"}),
	stEvent("smoke", "smokeDetector.smoke", map[string]string{"detected": "detected", "clear": "not_detected"}),
	// Значение tested - проверка датчика, а не газ: Алиса его не получает.
	stEvent("gas", "gasDetector.gas", map[string]string{"detected": "detected", "clear": "not_detected"}),
	// Покоя у вибрации нет: inactive Алиса не получает.
	stEvent("vibration", "accelerationSensor.acceleration", map[string]string{"active": "vibration"}),
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
	// Устройство сообщает кПа, Алисе нужны мм рт. ст.
	{Type: "float", Instance: "pressure", Status: "atmosphericPressureMeasurement.atmosphericPressure", Unit: "unit.pressure.mmhg", Scale: ptr(7.50062)},
	// Заряд словами - у устройств без процентов.
	stEvent("battery_level", "batteryLevel.battery", map[string]string{"normal": "normal", "warning": "low", "critical": "low"}),
	// Устройство сообщает ppm, а единица Яндекса - мкг/м³. Пересчёт зависит от состава смеси: число уходит как есть.
	lgFloat("tvoc", "tvocMeasurement.tvocLevel", density),
	lgFloat("amperage", "currentMeasurement.current", "unit.ampere"),
	lgFloat("gas_meter", "gasMeter.gasMeterVolume", "unit.cubic_meter"),
}

// Кнопки-тревоги: у Яндекса нет таких свойств, поэтому о событии сообщает кнопка самого устройства.
var (
	stTamper       = stEvent("button", "tamperAlert.tamper", map[string]string{"detected": "double_click"})
	stWaterFilter  = stEvent("button", "custom.waterFilter.waterFilterStatus", map[string]string{"replace": "double_click"})
	stFilterStatus = stEvent("button", "filterStatus.filterStatus", map[string]string{"replace": "double_click"})
	// stWaterMeter - поле отчёта о расходе воды, которое источник уже перевёл в м³.
	stWaterMeter = lgFloat("water_meter", "samsungce.waterConsumptionReport.waterConsumption.cumulativeAmount", "unit.cubic_meter")
)

// withSensors - свойства профиля и все показания датчиков, которых в нём нет: Яндекс не ограничивает
// свойства типом устройства, а значения, которых устройство не сообщает, в Алису не попадают.
func withSensors(props []Property) []Property {
	out := slices.Clone(props)
	for _, s := range stSensors {
		if !slices.ContainsFunc(props, func(p Property) bool { return p.Type == s.Type && p.Instance == s.Instance }) {
			out = append(out, s)
		}
	}
	return out
}

func stSensor(yandexType string) *Profile {
	return &Profile{YandexType: yandexType, Properties: append(slices.Clip(stSensors), stTamper)}
}

// stButton - кнопка или пульт: нажатия и заряд.
func stButton() *Profile {
	return &Profile{
		YandexType: "sensor.button",
		Properties: []Property{stButtonEvent(), lgFloat("battery_level", "battery.battery", percent)},
	}
}

func stButtonEvent() Property {
	return stEvent("button", "button.button", map[string]string{"pushed": "click", "double": "double_click",
		"pushed_2x": "double_click", "held": "long_press", "down_hold": "long_press"})
}

// stOther - устройство с общей категорией: то, что оно само сообщает. Питание и свойства появятся, только
// если устройство их описывает.
func stOther() *Profile {
	return &Profile{YandexType: "other", OnOff: "power", Power: stPower(), Properties: []Property{stButtonEvent()}}
}

// stHumidifierMode - скорость увлажнителя. Режим появится, только если устройство назовёт эти значения.
func stHumidifierMode() []Mode {
	return []Mode{{Instance: "fan_speed", Option: "humidifierMode.humidifierMode", Values: map[string]string{
		"auto": "auto", "low": "low", "medium": "medium", "high": "high",
	}}}
}

// stLamp - свет в приборе Samsung: on - значение «включено» у этого типа приборов.
func stLamp(on string) Toggle {
	return Toggle{Instance: "backlight", Setting: "samsungce.lamp.brightnessLevel", On: on, Off: "off", Optional: true}
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
		Modes: []Mode{{Instance: "input_source", Option: "mediaInputSource.inputSource", Values: map[string]string{
			"HDMI1": "one", "HDMI2": "two", "digital": "three", "wifi": "four", "bluetooth": "five",
		}}},
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
		// Питание есть только у компонента с выключателем, например icemaker.
		OnOff:      "power",
		Power:      stPower(),
		Ranges:     []Range{{Instance: "temperature", Option: "thermostatCoolingSetpoint.coolingSetpoint", Unit: celsius}},
		Properties: []Property{stSensors[0], stSensors[4]},
	}
}

// stOven - духовка, плита или микроволновка. lampOn - значение «свет включён»: у плиты оно своё.
// Окончание работы - кнопка: ovenJobState перешёл в finished.
func stOven(lampOn string) *Profile {
	return &Profile{
		YandexType: "cooking",
		Ranges:     []Range{{Instance: "temperature", Option: "ovenSetpoint.ovenSetpoint", Unit: celsius}},
		Toggles:    []Toggle{stLamp(lampOn)},
		Properties: []Property{stSensors[4], stEvent("button", "ovenOperatingState.ovenJobState", map[string]string{"finished": "click"})},
	}
}

// stShade - штора или окно: «включи» открывает, «выключи» закрывает; включено, пока не закрыто.
func stShade() *Profile {
	return &Profile{
		YandexType: "openable.curtain",
		OnOff:      "power",
		Power: &Power{Setting: "windowShade.windowShade", On: "open", Off: "close", State: "windowShade.windowShade",
			Idle: []string{"closed"}},
		Ranges: []Range{{Instance: "open", Option: "windowShadeLevel.shadeLevel", FallbackOption: "switchLevel.level", Unit: percent}},
		// Команда pause: устройство сообщает paused или partially open. Off действует, только если
		// устройство сообщило paused: продолжить движение нечем, поэтому штора открывается.
		Toggles: []Toggle{{Instance: "pause", Setting: "windowShade.windowShade", On: "pause", Off: "open",
			State: "windowShade.windowShade", StateOn: "paused", Optional: true}},
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
	"Switch": {YandexType: "switch", OnOff: "power", Power: stPower(), Properties: stSensors,
		Ranges: []Range{{Instance: "brightness", Option: "switchLevel.level", Unit: percent}}},
	"SmartPlug": {YandexType: "socket", OnOff: "power", Power: stPower(), Properties: stSensors,
		Ranges: []Range{{Instance: "brightness", Option: "switchLevel.level", Unit: percent}}},

	"ContactSensor":         stSensor("sensor.open"),
	"MotionSensor":          stSensor("sensor.motion"),
	"PresenceSensor":        stSensor("sensor.motion"),
	"LeakSensor":            stSensor("sensor.water_leak"),
	"SmokeDetector":         stSensor("sensor.smoke"),
	"TempSensor":            stSensor("sensor.climate"),
	"TempHumiditySensor":    stSensor("sensor.climate"),
	"HumiditySensor":        stSensor("sensor.climate"),
	"EnvironmentSensor":     stSensor("sensor.climate"),
	"AirQualityDetector":    stSensor("sensor.climate"),
	"WeatherStation":        stSensor("sensor.climate"),
	"LightSensor":           stSensor("sensor.illumination"),
	"MultiFunctionalSensor": stSensor("sensor"),
	"GenericSensor":         stSensor("sensor"),
	// Датчик газа сообщает категорию Siren. Шлюз им не управляет, а только передаёт показания.
	"Siren":                  stSensor("sensor"),
	"PowerMeasurementSensor": stSensor("smart_meter.electricity"),
	"CurbPowerMeter":         stSensor("smart_meter.electricity"),
	"GasMeter":               stSensor("smart_meter.gas"),
	"Button":                 stButton(),
	"RemoteController":       stButton(),
	"PanicButton":            stButton(),
	"Other":                  stOther(),
	"Others":                 stOther(),

	"Thermostat": {
		YandexType: "thermostat",
		OnOff:      "power",
		Power: &Power{Setting: "thermostatMode.thermostatMode", On: "heat", Off: "off", State: "thermostatMode.thermostatMode",
			Idle: []string{"off"}},
		Modes: []Mode{{Instance: "thermostat", Option: "thermostatMode.thermostatMode", Values: map[string]string{
			"auto": "auto", "cool": "cool", "heat": "heat", "eco": "eco",
		}}, {Instance: "fan_speed", Option: "thermostatFanMode.thermostatFanMode", Values: map[string]string{
			"auto": "auto", "on": "high", "circulate": "low",
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
		Ranges: []Range{{Instance: "temperature", Option: "thermostatCoolingSetpoint.coolingSetpoint", Unit: celsius}},
		Toggles: []Toggle{{Instance: "ionization", Setting: "custom.spiMode.spiMode", On: "on", Off: "off", Optional: true},
			{Instance: "backlight", Setting: "samsungce.airConditionerLighting.lighting", On: "on", Off: "off"}},
		Properties: []Property{stFilterStatus},
	},
	"AirPurifier": {
		YandexType: "purifier",
		OnOff:      "power",
		Power:      stPower(),
		Modes: []Mode{{Instance: "fan_speed", Option: "airPurifierFanMode.airPurifierFanMode",
			FallbackOption: "airConditionerFanMode.fanMode", Values: map[string]string{
				"auto": "auto", "low": "low", "medium": "medium", "high": "high", "sleep": "quiet",
			}}},
		Toggles:    []Toggle{stLamp("high")},
		Properties: []Property{stFilterStatus},
	},
	"Humidifier":          {YandexType: "humidifier", OnOff: "power", Power: stPower(), Modes: stHumidifierMode(), Properties: stSensors},
	"Dehumidifier":        {YandexType: "humidifier", OnOff: "power", Power: stPower(), Properties: stSensors},
	"AirpurifyHumidifier": {YandexType: "humidifier", OnOff: "power", Power: stPower(), Modes: stHumidifierMode(), Properties: stSensors},
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

	"Washer": func() *Profile {
		p := stLaundry("washing_machine", "washerOperatingState", "washerJobState", "finish")
		p.Properties = append(p.Properties, stWaterMeter)
		return p
	}(),
	"Dryer": stLaundry("washing_machine", "dryerOperatingState", "dryerJobState", "finished"),
	"Dishwasher": func() *Profile {
		p := stLaundry("dishwasher", "dishwasherOperatingState", "dishwasherJobState", "finish")
		p.Properties = append(p.Properties, stWaterMeter)
		p.Modes = []Mode{{Instance: "dishwashing", Option: "samsungce.dishwasherWashingCourse.washingCourse", Values: map[string]string{
			"auto": "auto", "eco": "eco", "normal": "normal", "intensive": "intensive", "heavy": "max", "delicate": "glass",
			"express": "express", "express_0C": "fast", "preWash": "pre_rinse", "rinseOnly": "one", "extraSilence": "quiet",
		}}}
		return p
	}(),

	"Refrigerator": func() *Profile {
		p := stFridge()
		p.Properties = append(p.Properties, stWaterFilter)
		return p
	}(),
	"KimchiRefrigerator": stFridge(),
	"CubeRefrigerator":   stFridge(),
	"WineCellar":         stFridge(),
	"Oven":               stOven("high"),
	"Range":              stOven("extraHigh"),
	"Microwave":          stOven("high"),

	"RobotCleaner": {
		YandexType: "vacuum_cleaner",
		OnOff:      "power",
		Power: &Power{Setting: "robotCleanerMovement.robotCleanerMovement", On: "cleaning", Off: "homing",
			State: "robotCleanerMovement.robotCleanerMovement",
			Idle:  []string{"homing", "idle", "charging", "powerOff", "alarm", "reserve", "after"}},
		Modes: []Mode{{Instance: "work_speed", Option: "robotCleanerTurboMode.robotCleanerTurboMode", Values: map[string]string{
			"off": "auto", "on": "turbo", "silence": "normal", "extraSilence": "quiet",
		}}, {Instance: "cleanup_mode", Option: "samsungce.robotCleanerCleaningType.cleaningType", Values: map[string]string{
			"vacuum": "dry_cleaning", "mop": "wet_cleaning", "vacuumAndMopTogether": "mixed_cleaning", "mopAfterVacuum": "normal",
		}}},
		Toggles: []Toggle{{Instance: "pause", Setting: "robotCleanerMovement.robotCleanerMovement", On: "pause", Off: "cleaning"}},
		Properties: []Property{lgFloat("battery_level", "battery.battery", percent),
			stEvent("button", "robotCleanerMovement.robotCleanerMovement", map[string]string{"alarm": "double_click"})},
	},
}

func ptr[T any](v T) *T { return &v }
