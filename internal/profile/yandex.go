package profile

// Списки Яндекса, из которых профиль берёт значения: типы устройств, instance умений
// и свойств, их единицы и значения событий. Взяты из документации умного дома Яндекса.

// YandexTypes - типы устройств без префикса devices.types.
var YandexTypes = []string{
	"camera", "cooking", "cooking.coffee_maker", "cooking.kettle", "cooking.multicooker", "dishwasher",
	"humidifier", "iron", "light", "light.ceiling", "light.dimmable", "light.garland", "light.lamp",
	"light.sconce", "light.strip", "light.torchere", "media_device", "media_device.receiver",
	"media_device.tv", "media_device.tv_box", "openable", "openable.curtain", "openable.door_lock",
	"openable.valve", "other", "pet_drinking_fountain", "pet_feeder", "purifier", "sensor",
	"sensor.button", "sensor.climate", "sensor.gas", "sensor.illumination", "sensor.motion",
	"sensor.open", "sensor.smoke", "sensor.vibration", "sensor.water_leak", "smart_meter",
	"smart_meter.cold_water", "smart_meter.electricity", "smart_meter.gas", "smart_meter.heat",
	"smart_meter.hot_water", "socket", "switch", "switch.relay", "thermostat", "thermostat.ac",
	"vacuum_cleaner", "ventilation", "ventilation.fan", "washing_machine",
}

// Значения, которые шлюз проверяет в профилях.
var (
	// OnOffKinds - что делает «включи»: запуск программы, питание или ничего.
	OnOffKinds = []string{"program", "power", "none"}
	// ButtonValues - события датчика-кнопки.
	ButtonValues = []string{"click", "double_click", "long_press"}
	// PropertyTypes - виды свойств.
	PropertyTypes = []string{"event", "float"}
	// ModeInstances и ModeValues - instance и значения умения mode.
	ModeInstances = []string{
		"cleanup_mode", "coffee_mode", "dishwashing", "fan_speed", "heat", "input_source", "program",
		"swing", "tea_mode", "thermostat", "ventilation_mode", "work_speed",
	}
	ModeValues = []string{
		"wet_cleaning", "dry_cleaning", "mixed_cleaning", "auto", "eco", "smart", "turbo", "cool", "dry",
		"fan_only", "heat", "preheat", "high", "low", "medium", "max", "min", "fast", "slow", "express",
		"normal", "quiet", "horizontal", "stationary", "vertical", "supply_air", "extraction_air",
		"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"americano", "cappuccino", "double_espresso", "espresso", "latte", "black_tea", "flower_tea",
		"green_tea", "herbal_tea", "oolong_tea", "puerh_tea", "red_tea", "white_tea", "glass",
		"intensive", "pre_rinse", "aspic", "baby_food", "baking", "bread", "boiling", "cereals",
		"cheesecake", "deep_fryer", "dessert", "fowl", "frying", "macaroni", "milk_porridge",
		"multicooker", "pasta", "pilaf", "pizza", "sauce", "slow_cook", "soup", "steam", "stewing",
		"vacuum", "yogurt",
	}
)

// Undocumented - единицы, которые интерфейс знает, а документация Яндекса - нет: свойство с
// ними Яндекс может не принять.
var Undocumented = []string{"unit.time.seconds"}

// Значения, которые шлюз пока не проверяет, а веб-интерфейс предлагает на выбор.
var (
	// RangeUnits - instance умения range и их единицы; без единиц - пустой список.
	RangeUnits = map[string][]string{
		"brightness":  {"unit.percent"},
		"channel":     nil,
		"humidity":    {"unit.percent"},
		"open":        {"unit.percent"},
		"temperature": {"unit.temperature.celsius", "unit.temperature.kelvin"},
		"volume":      {"unit.percent"},
	}
	// ToggleInstances - instance умения toggle.
	ToggleInstances = []string{"backlight", "controls_locked", "ionization", "keep_warm", "mute", "oscillation", "pause"}
	// FloatUnits - instance свойства float и их единицы.
	FloatUnits = map[string][]string{
		"amperage":          {"unit.ampere"},
		"battery_level":     {"unit.percent"},
		"co2_level":         {"unit.ppm"},
		"electricity_meter": {"unit.kilowatt_hour"},
		"food_level":        {"unit.percent"},
		"gas_meter":         {"unit.cubic_meter"},
		"heat_meter":        {"unit.gigacalorie"},
		"humidity":          {"unit.percent"},
		"illumination":      {"unit.illumination.lux"},
		"meter":             nil,
		"pm1_density":       {"unit.density.mcg_m3"},
		"pm2.5_density":     {"unit.density.mcg_m3"},
		"pm10_density":      {"unit.density.mcg_m3"},
		"power":             {"unit.watt"},
		"pressure":          {"unit.pressure.atm", "unit.pressure.pascal", "unit.pressure.bar", "unit.pressure.mmhg"},
		"temperature":       {"unit.temperature.celsius", "unit.temperature.kelvin"},
		"tvoc":              {"unit.density.mcg_m3"},
		"voltage":           {"unit.volt"},
		"water_level":       {"unit.percent"},
		"water_meter":       {"unit.cubic_meter"},
	}
	// EventValues - instance свойства event и их значения.
	EventValues = map[string][]string{
		"battery_level": {"low", "normal"},
		"button":        {"click", "double_click", "long_press"},
		"food_level":    {"empty", "low", "normal"},
		"gas":           {"detected", "not_detected", "high"},
		"motion":        {"detected", "not_detected"},
		"open":          {"opened", "closed"},
		"smoke":         {"detected", "not_detected", "high"},
		"vibration":     {"tilt", "fall", "vibration"},
		"water_leak":    {"dry", "leak"},
		"water_level":   {"empty", "low", "normal"},
	}
)
