package web

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/config"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/polaris"
	"hcgateway/internal/profile"
)

// Подписи по-русски: источники, типы приборов, состояния и имена Яндекса. Названия типов
// устройств, умений, режимов, сцен и единиц взяты из документации умного дома Яндекса.
// Рядом с подписью страница показывает и само имя: его пишут в настройках и видно в журнале.

// sourceNames - источники приборов коротко: в списке приборов и на странице прибора.
var sourceNames = map[string]string{
	config.SourceDemo:             "демо-прибор",
	config.SourceHomeConnectCloud: "Home Connect, облако",
	config.SourceHomeConnectLocal: "Home Connect, домашняя сеть",
	config.SourceWLED:             "лента WLED",
	config.SourceThinQ:            "LG ThinQ",
	config.SourceDeLonghi:         "De'Longhi",
	config.SourceWebOS:            "телевизор LG",
	config.SourceSamsungTV:        "телевизор Samsung",
	config.SourceSmartThings:      "SmartThings",
	config.SourcePolaris:          "Polaris",
}

// typeNames - типы приборов Home Connect, LG ThinQ и De'Longhi.
var typeNames = map[string]string{
	"Oven": "Духовой шкаф", "Dishwasher": "Посудомоечная машина", "Washer": "Стиральная машина",
	"Dryer": "Сушильная машина", "WasherDryer": "Стирально-сушильная машина", "Refrigerator": "Холодильник",
	"Freezer": "Морозильник", "FridgeFreezer": "Холодильник с морозильником", "WineCooler": "Винный шкаф",
	"CoffeeMaker": "Кофемашина", "Hob": "Варочная панель", "Hood": "Вытяжка", "CleaningRobot": "Робот-пылесос",
	"CookProcessor": "Кухонный процессор", "WarmingDrawer": "Шкаф для подогрева",
	"AirConditioner": "Кондиционер", "Microwave": "Микроволновая печь",
	profile.AllInOneType: "Все свойства в одном приборе", profile.PolarisKettleType: "Чайник Polaris", profile.DeLonghiType: "Кофемашина De'Longhi", profile.LGTVType: "Телевизор LG", profile.SamsungTVType: "Телевизор Samsung",

	"DEVICE_AIR_CONDITIONER": "Кондиционер LG", "DEVICE_AIR_PURIFIER": "Очиститель воздуха LG",
	"DEVICE_AIR_PURIFIER_FAN": "Очиститель-вентилятор LG", "DEVICE_CEILING_FAN": "Потолочный вентилятор LG",
	"DEVICE_DEHUMIDIFIER": "Осушитель воздуха LG", "DEVICE_DISH_WASHER": "Посудомоечная машина LG",
	"DEVICE_DRYER": "Сушильная машина LG", "DEVICE_HOOD": "Вытяжка LG", "DEVICE_HUMIDIFIER": "Увлажнитель воздуха LG",
	"DEVICE_KIMCHI_REFRIGERATOR": "Холодильник для кимчи LG", "DEVICE_MICROWAVE_OVEN": "Микроволновая печь LG",
	"DEVICE_OVEN": "Духовой шкаф LG", "DEVICE_REFRIGERATOR": "Холодильник LG", "DEVICE_ROBOT_CLEANER": "Робот-пылесос LG",
	"DEVICE_STICK_CLEANER": "Вертикальный пылесос LG", "DEVICE_STYLER": "Стайлер LG", "DEVICE_SYSTEM_BOILER": "Котёл LG",
	"DEVICE_VENTILATOR": "Вентиляция LG", "DEVICE_WASHCOMBO_MAIN": "Стирально-сушильная машина LG",
	"DEVICE_WASHCOMBO_MINI": "Мини-стиральная машина LG", "DEVICE_WASHER": "Стиральная машина LG",
	"DEVICE_WASHTOWER": "Стирально-сушильная колонна LG", "DEVICE_WASHTOWER_DRYER": "Сушильная машина колонны LG",
	"DEVICE_WASHTOWER_WASHER": "Стиральная машина колонны LG", "DEVICE_WATER_HEATER": "Водонагреватель LG",
	"DEVICE_WINE_CELLAR": "Винный шкаф LG", "DEVICE_COOKTOP": "Варочная панель LG",
	"DEVICE_WATER_PURIFIER": "Фильтр для воды LG", "DEVICE_HOME_BREW": "Домашняя пивоварня LG",
	"DEVICE_PLANT_CULTIVATOR": "Гроубокс LG",
}

// yandexTypeNames - типы устройств Алисы без префикса devices.types.
var yandexTypeNames = map[string]string{
	"camera": "Видеокамера", "cooking": "Кухонная техника", "cooking.coffee_maker": "Кофеварка",
	"cooking.kettle": "Чайник", "cooking.multicooker": "Мультиварка", "dishwasher": "Посудомоечная машина",
	"humidifier": "Увлажнитель воздуха", "iron": "Утюг", "light": "Лампочка, светильник", "light.ceiling": "Люстра",
	"light.dimmable": "Умный диммер", "light.garland": "Гирлянда", "light.lamp": "Настольная лампа", "light.sconce": "Бра",
	"light.strip": "Диодная лента", "light.torchere": "Торшер", "media_device": "Медиаустройство",
	"media_device.receiver": "Ресивер", "media_device.tv": "Телевизор", "media_device.tv_box": "ТВ-приставка",
	"openable": "Дверь, ворота, окно, ставни", "openable.curtain": "Шторы, жалюзи", "openable.door_lock": "Замок",
	"openable.valve": "Шаровой кран", "other": "Остальные устройства", "pet_drinking_fountain": "Поилка",
	"pet_feeder": "Кормушка", "purifier": "Очиститель воздуха", "sensor": "Датчик", "sensor.button": "Умная кнопка",
	"sensor.climate": "Датчик климата", "sensor.gas": "Датчик газа", "sensor.illumination": "Датчик освещённости",
	"sensor.motion": "Датчик движения", "sensor.open": "Датчик открытия двери", "sensor.smoke": "Датчик дыма",
	"sensor.vibration": "Датчик вибрации", "sensor.water_leak": "Датчик протечки воды", "smart_meter": "Счётчик",
	"smart_meter.cold_water": "Счётчик холодной воды", "smart_meter.electricity": "Счётчик электроэнергии",
	"smart_meter.gas": "Счётчик газа", "smart_meter.heat": "Счётчик тепла", "smart_meter.hot_water": "Счётчик горячей воды",
	"socket": "Умная розетка", "switch": "Выключатель", "switch.relay": "Реле", "thermostat": "Термостат",
	"thermostat.ac": "Кондиционер", "vacuum_cleaner": "Робот-пылесос", "ventilation": "Вентиляция",
	"ventilation.fan": "Вентилятор", "washing_machine": "Стиральная машина",
}

// kindNames - виды умений и свойств Алисы.
var kindNames = map[string]string{
	alice.CapOnOff: "Включение", alice.CapMode: "Режим", alice.CapRange: "Число", alice.CapToggle: "Переключатель",
	alice.CapColor: "Цвет", alice.PropFloat: "Показание", alice.PropEvent: "Событие",
}

// Instance умений и свойств по видам.
var (
	modeInstanceNames = map[string]string{
		"cleanup_mode": "Режим уборки", "coffee_mode": "Режим кофеварки", "dishwashing": "Режим мытья посуды",
		"fan_speed": "Скорость вентиляции", "heat": "Режим нагрева", "input_source": "Источник сигнала",
		"program": "Программа", "swing": "Направление воздуха", "tea_mode": "Режим приготовления чая",
		"thermostat": "Температурный режим", "ventilation_mode": "Режим вентиляции", "work_speed": "Скорость работы",
	}
	rangeInstanceNames = map[string]string{
		"brightness": "Яркость", "channel": "Канал", "humidity": "Влажность", "open": "Открытие",
		"temperature": "Температура", "volume": "Громкость",
	}
	toggleInstanceNames = map[string]string{
		"backlight": "Подсветка", "controls_locked": "Блокировка управления", "ionization": "Ионизация",
		"keep_warm": "Поддержание тепла", "mute": "Без звука", "oscillation": "Вращение", "pause": "Пауза",
	}
	floatInstanceNames = map[string]string{
		"amperage": "Ток", "battery_level": "Заряд батареи", "co2_level": "Углекислый газ",
		"electricity_meter": "Счётчик электроэнергии", "food_level": "Уровень корма", "gas_meter": "Счётчик газа",
		"heat_meter": "Счётчик тепла", "humidity": "Влажность", "illumination": "Освещённость", "meter": "Счётчик",
		"pm1_density": "Частицы PM1", "pm2.5_density": "Частицы PM2.5", "pm10_density": "Частицы PM10",
		"power": "Мощность", "pressure": "Давление", "remaining_time": "Оставшееся время", "temperature": "Температура",
		"tvoc": "Органические вещества", "voltage": "Напряжение", "water_level": "Уровень воды",
		"water_meter": "Счётчик воды",
	}
	eventInstanceNames = map[string]string{
		"battery_level": "Заряд батареи", "button": "Кнопка", "food_level": "Уровень корма", "gas": "Газ",
		"motion": "Движение", "open": "Открытие", "smoke": "Дым", "vibration": "Вибрация",
		"water_leak": "Протечка", "water_level": "Уровень воды",
	}
	colorInstanceNames = map[string]string{"rgb": "Цвет", "hsv": "Цвет", "scene": "Сцена", "temperature_k": "Цветовая температура"}
)

// modeValueNames - режимы умения mode.
var modeValueNames = map[string]string{
	"wet_cleaning": "Влажная уборка", "dry_cleaning": "Сухая уборка", "mixed_cleaning": "Смешанная уборка",
	"auto": "Автоматический", "eco": "Экономичный", "smart": "Умный", "turbo": "Турбо", "cool": "Охлаждение",
	"dry": "Осушение", "fan_only": "Вентиляция", "heat": "Обогрев", "preheat": "Подогрев",
	"high": "Высокая скорость", "low": "Низкая скорость", "medium": "Средняя скорость", "max": "Максимальный",
	"min": "Минимальный", "fast": "Быстрый", "slow": "Медленный", "express": "Экспресс", "normal": "Нормальный",
	"quiet": "Тихий", "horizontal": "Горизонтальный", "stationary": "Неподвижный", "vertical": "Вертикальный",
	"supply_air": "Приток воздуха", "extraction_air": "Отток воздуха",
	"one": "Первый", "two": "Второй", "three": "Третий", "four": "Четвёртый", "five": "Пятый",
	"six": "Шестой", "seven": "Седьмой", "eight": "Восьмой", "nine": "Девятый", "ten": "Десятый",
	"americano": "Американо", "cappuccino": "Капучино", "double_espresso": "Двойной эспрессо",
	"espresso": "Эспрессо", "latte": "Латте", "black_tea": "Чёрный чай", "flower_tea": "Цветочный чай",
	"green_tea": "Зелёный чай", "herbal_tea": "Травяной чай", "oolong_tea": "Чай улун", "puerh_tea": "Чай пуэр",
	"red_tea": "Красный чай", "white_tea": "Белый чай", "glass": "Мойка стекла", "intensive": "Интенсивный",
	"pre_rinse": "Ополаскивание", "aspic": "Холодец", "baby_food": "Детское питание", "baking": "Выпечка",
	"bread": "Хлеб", "boiling": "Варка", "cereals": "Крупы", "cheesecake": "Чизкейк", "deep_fryer": "Фритюр",
	"dessert": "Десерты", "fowl": "Дичь", "frying": "Жарка", "macaroni": "Макароны",
	"milk_porridge": "Молочная каша", "multicooker": "Мультиповар", "pasta": "Паста", "pilaf": "Плов",
	"pizza": "Пицца", "sauce": "Соус", "slow_cook": "Томление", "soup": "Суп", "steam": "Пар",
	"stewing": "Тушение", "vacuum": "Вакуум", "yogurt": "Йогурт",
}

// eventValueNames - значения свойств event.
var eventValueNames = map[string]string{
	"tilt": "переворачивание", "fall": "падение", "vibration": "вибрация", "opened": "открыто", "closed": "закрыто",
	"click": "одиночное нажатие", "double_click": "двойное нажатие", "long_press": "долгое нажатие",
	"detected": "обнаружено", "not_detected": "не обнаружено", "high": "высокий уровень", "low": "низкий",
	"normal": "нормальный", "empty": "пустой", "dry": "нет протечки", "leak": "протечка",
}

// sceneNames - сцены умения color_setting.
var sceneNames = map[string]string{
	"alarm": "Тревога", "alice": "Алиса", "candle": "Свеча", "dinner": "Ужин", "fantasy": "Фантазия",
	"garland": "Гирлянда", "jungle": "Джунгли", "movie": "Кино", "neon": "Неон", "night": "Ночь",
	"ocean": "Океан", "party": "Вечеринка", "reading": "Чтение", "rest": "Отдых", "romance": "Романтика",
	"siren": "Сирена",
}

// unitNames - единицы Яндекса значками.
var unitNames = map[string]string{
	"unit.percent": "%", "unit.temperature.celsius": "°C", "unit.temperature.kelvin": "K", "unit.ampere": "А",
	"unit.ppm": "ppm", "unit.kilowatt_hour": "кВт·ч", "unit.cubic_meter": "м³", "unit.gigacalorie": "Гкал",
	"unit.illumination.lux": "лк", "unit.density.mcg_m3": "мкг/м³", "unit.watt": "Вт", "unit.pressure.atm": "атм",
	"unit.pressure.pascal": "Па", "unit.pressure.bar": "бар", "unit.pressure.mmhg": "мм рт. ст.", "unit.volt": "В",
	"unit.time.seconds": "с",
}

// stateNames - состояние работы прибора одним словом. Перечисления Home Connect - по полному
// имени значения; режимы кофемашины - по имени и значению; значения LG - как есть.
var stateNames = map[string]string{
	"BSH.Common.EnumType.OperationState.Inactive":       "выключен",
	appliance.OperationReady:                            "готов",
	appliance.OperationDelayed:                          "ждёт отложенного старта",
	appliance.OperationRun:                              "работает",
	appliance.OperationPause:                            "на паузе",
	"BSH.Common.EnumType.OperationState.ActionRequired": "ждёт действия на приборе",
	appliance.OperationFinished:                         "программа закончена",
	"BSH.Common.EnumType.OperationState.Error":          "ошибка",
	"BSH.Common.EnumType.OperationState.Aborting":       "отменяет программу",

	delonghi.StatusMode + "=0":  "выключена",
	delonghi.StatusMode + "=1":  "включается",
	delonghi.StatusMode + "=2":  "выключается",
	delonghi.StatusMode + "=7":  "готова",
	delonghi.StatusMode + "=11": "наливает воду",

	"POWER_OFF": "выключен", "INITIAL": "ожидание", "DETECTING": "определяет загрузку", "RUNNING": "работает",
	"RINSING": "полоскание", "SPINNING": "отжим", "DRYING": "сушка", "COOLING": "охлаждение", "PAUSE": "пауза",
	polaris.KeyMode + "=0": "выключен", polaris.KeyMode + "=1": "кипячение", polaris.KeyMode + "=2": "кипячение с удержанием",
	polaris.KeyMode + "=3": "разогрев", polaris.KeyMode + "=4": "разогрев с удержанием", polaris.KeyMode + "=5": "IQ-кипячение",
	polaris.KeyMode + "=6": "чайная церемония",

	"RESERVED": "отложенный старт", "END": "работа закончена", "ERROR": "ошибка",
	"COOL_DOWN": "охлаждение", "RINSE_HOLD": "ожидание после полоскания", "REFRESHING": "освежение", "SLEEP": "спящий режим",
}

// alarmNames - что значит тревога кофемашины De'Longhi, в порядке на метке прибора: сначала сбои, затем
// то, что может сделать владелец. Описания протокола нет: смысл взят из имён и списка тревог старых
// машин. Чего здесь нет, метка показывает по имени.
var alarmNames = []kv{
	{"BrokenGrinder", "сбой кофемолки"},
	{"HeaterNTC", "сбой датчика нагревателя"},
	{"Fan", "сбой вентилятора"},
	{"General", "общая ошибка машины"},
	{"Infuser", "проверьте заварочный блок"},
	{"Bubble", "воздух в контуре воды"},
	{"Waste", "очистите контейнер гущи"},
	{"Drain", "опорожните поддон"},
	{"Descale", "удалите накипь"},
	{"Filter", "замените фильтр"},
	{"GroundFine", "помол слишком мелкий"},
	{"BrewUnitClean", "очистите заварочный блок"},
	{"Clean", "нужна очистка"},
	{"NoCoffee", "нет зёрен"},
	{"TankLevel", "мало воды"},
	{"TankPresence", "бак снят"},
}

// alarmName - что значит тревога кофемашины; пусто - шлюз её не знает.
func alarmName(alarm string) string {
	for _, a := range alarmNames {
		if a.Key == alarm {
			return a.Value
		}
	}
	return ""
}

// nameOr - подпись из names или само имя, если подписи нет.
func nameOr(names map[string]string, id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id
}

func sourceName(source string) string { return nameOr(sourceNames, source) }

// stCategoryNames - категории устройств SmartThings со встроенным профилем.
var stCategoryNames = map[string]string{
	"Light": "Лампа", "Switch": "Выключатель", "SmartPlug": "Розетка", "ContactSensor": "Датчик открытия",
	"MotionSensor": "Датчик движения", "LeakSensor": "Датчик протечки", "SmokeDetector": "Датчик дыма",
	"TempSensor": "Датчик температуры", "TempHumiditySensor": "Датчик температуры и влажности",
	"HumiditySensor": "Датчик влажности", "EnvironmentSensor": "Датчик климата", "AirQualityDetector": "Датчик качества воздуха",
	"WeatherStation": "Метеостанция", "LightSensor": "Датчик освещённости", "MultiFunctionalSensor": "Многофункциональный датчик",
	"GenericSensor": "Датчик", "PowerMeasurementSensor": "Датчик мощности", "CurbPowerMeter": "Счётчик электроэнергии",
	"Button": "Кнопка", "Thermostat": "Термостат", "AirConditioner": "Кондиционер", "AirPurifier": "Очиститель воздуха",
	"Humidifier": "Увлажнитель", "Dehumidifier": "Осушитель", "AirpurifyHumidifier": "Очиститель-увлажнитель",
	"Fan": "Вентилятор", "Blind": "Жалюзи", "Shade": "Штора", "WindowOpener": "Окно", "WaterValve": "Кран",
	"Television": "Телевизор", "Speaker": "Колонка", "NetworkAudio": "Сетевое аудио", "SoundBar": "Саундбар",
	"MusicSystem": "Музыкальный центр", "HomeTheater": "Домашний кинотеатр", "Receiver": "Ресивер", "SetTop": "ТВ-приставка",
	"Washer": "Стиральная машина", "Dryer": "Сушильная машина", "Dishwasher": "Посудомоечная машина",
	"Refrigerator": "Холодильник", "KimchiRefrigerator": "Холодильник для кимчи", "CubeRefrigerator": "Мини-холодильник",
	"WineCellar": "Винный шкаф", "Oven": "Духовой шкаф", "Range": "Плита", "Microwave": "Микроволновая печь",
	"RobotCleaner": "Робот-пылесос",
}

// typeName - тип прибора по-русски; у устройства SmartThings - категория с пометкой SmartThings.
func typeName(haType string) string {
	if cat, ok := strings.CutPrefix(haType, profile.SmartThingsPrefix); ok {
		return nameOr(stCategoryNames, cat) + " (SmartThings)"
	}
	return nameOr(typeNames, haType)
}

func yandexTypeName(t string) string {
	return nameOr(yandexTypeNames, strings.TrimPrefix(t, "devices.types."))
}

// instanceNames - подписи instance для вида умения или свойства.
func instanceNames(kind string) map[string]string {
	switch kind {
	case alice.CapMode:
		return modeInstanceNames
	case alice.CapRange:
		return rangeInstanceNames
	case alice.CapToggle:
		return toggleInstanceNames
	case alice.CapColor:
		return colorInstanceNames
	case alice.PropFloat:
		return floatInstanceNames
	case alice.PropEvent:
		return eventInstanceNames
	}
	return nil
}

// plain - варианты выбора только с подписями по-русски, без имён: «Духовой шкаф». blank -
// подпись пустого варианта; пустая подпись - пустого варианта нет.
func plain(list []string, cur, blank string, names map[string]string) []choice {
	out := choices(list, cur, blank, nil)
	for i, c := range out {
		if c.Value != "" {
			out[i].Label = nameOr(names, c.Value)
		}
	}
	return out
}

// programShort - программа прибора коротко: последняя часть имени, у кофемашины - «рецепт N».
func programShort(program string) string {
	if n, ok := strings.CutPrefix(program, delonghi.ProgramPrefix); ok {
		return "рецепт " + n
	}
	return short(program)
}

// programName - программа для выбора: у кофемашины - напиток Алисы, которому сопоставлен
// рецепт, иначе как programShort.
func programName(program string, p *profile.Profile) string {
	if pm := p.ProgramMode(); pm != nil && strings.HasPrefix(program, delonghi.ProgramPrefix) {
		if v, ok := pm.Programs[program]; ok {
			return nameOr(modeValueNames, v)
		}
	}
	return programShort(program)
}

// stateLabel - состояние работы по-русски; неизвестное значение - последняя часть имени.
func stateLabel(key, value string) string {
	if n, ok := stateNames[key+"="+value]; ok {
		return n
	}
	if n, ok := stateNames[value]; ok {
		return n
	}
	return short(value)
}

// programLabel - программа по-русски: режим Алисы, которому она сопоставлена, иначе
// последняя часть имени.
func programLabel(p *profile.Profile, program string) string {
	if pm := p.ProgramMode(); pm != nil {
		if v, ok := pm.Programs[program]; ok {
			return nameOr(modeValueNames, v)
		}
	}
	return short(program)
}

// withUnit - число с единицей Яндекса значком. Дробь страница показывает до сотых, чтобы
// значение вроде 79,933333 не занимало строку; Алиса получает его как есть.
func withUnit(v any, unit string) string {
	if f, ok := v.(float64); ok {
		v = math.Round(f*100) / 100
	}
	if u := nameOr(unitNames, unit); u != "" {
		return formatValue(v) + " " + u
	}
	return formatValue(v)
}

// shortKind - вид умения или свойства без префикса Яндекса: mode, float.
func shortKind(kind string) string {
	return strings.TrimPrefix(strings.TrimPrefix(kind, "devices.capabilities."), "devices.properties.")
}

// aliceItem - умение или свойство устройства Алисы: подпись и значение по-русски, а мелко
// рядом - имя Яндекса.
type aliceItem struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Tech  string `json:"tech"`
}

// itemLabel - подпись умения или свойства: вид и instance, «Режим «Программа»».
func itemLabel(kind, instance string) string {
	switch kind {
	case alice.CapOnOff:
		return kindNames[kind]
	case alice.CapColor:
		return nameOr(colorInstanceNames, instance)
	}
	return nameOr(kindNames, kind) + " «" + nameOr(instanceNames(kind), instance) + "»"
}

// stateItems - состояние устройства Алисы по-русски. units - единицы чисел из описания
// устройства по виду и instance.
func stateItems(st alice.DeviceState, units map[string]string) []aliceItem {
	var out []aliceItem
	for _, c := range append(append([]alice.CapState(nil), st.Capabilities...), st.Properties...) {
		sv, ok := c.State.(alice.StateValue)
		if !ok {
			continue
		}
		out = append(out, aliceItem{Label: itemLabel(c.Type, sv.Instance), Value: itemValue(c.Type, sv, units[c.Type+"/"+sv.Instance]),
			Tech: shortKind(c.Type) + " " + sv.Instance + ": " + formatValue(sv.Value)})
	}
	return out
}

// itemValue - значение умения или свойства вида kind по-русски; unit - единица числа.
func itemValue(kind string, sv alice.StateValue, unit string) string {
	switch kind {
	case alice.CapOnOff, alice.CapToggle:
		if sv.Value == true {
			return "включено"
		}
		return "выключено"
	case alice.CapMode:
		return nameOr(modeValueNames, formatValue(sv.Value))
	case alice.CapRange, alice.PropFloat:
		if f, ok := sv.Value.(float64); ok && sv.Instance == hcdevice.RemainingTime {
			return hcdevice.Duration(f)
		}
		return withUnit(sv.Value, unit)
	case alice.CapColor:
		switch v := sv.Value.(type) {
		case int:
			if sv.Instance == "temperature_k" {
				return fmt.Sprintf("%d K", v)
			}
			return fmt.Sprintf("#%06x", v)
		case alice.HSV:
			return fmt.Sprintf("оттенок %d, насыщенность %d %%", v.H, v.S)
		case string:
			return nameOr(sceneNames, v)
		}
	case alice.PropEvent:
		return nameOr(eventValueNames, formatValue(sv.Value))
	}
	return formatValue(sv.Value)
}

// aliceValues - что Алиса видит сейчас у прибора: значение по-русски по виду
// и instance, «devices.capabilities.range/temperature».
func aliceValues(dev alice.Device) map[string]string {
	u := map[string]string{}
	for _, info := range dev.Info() {
		for k, v := range units(info) {
			u[k] = v
		}
	}
	out := map[string]string{}
	for _, st := range dev.States() {
		for _, c := range append(slices.Clone(st.Capabilities), st.Properties...) {
			if sv, ok := c.State.(alice.StateValue); ok {
				out[c.Type+"/"+sv.Instance] = itemValue(c.Type, sv, u[c.Type+"/"+sv.Instance])
			}
		}
	}
	return out
}

// units - единицы чисел устройства Алисы по виду и instance: «devices.capabilities.range/temperature».
func units(info alice.DeviceInfo) map[string]string {
	out := map[string]string{}
	for _, c := range append(append([]alice.Capability(nil), info.Capabilities...), info.Properties...) {
		switch p := c.Parameters.(type) {
		case alice.RangeParams:
			out[c.Type+"/"+p.Instance] = p.Unit
		case alice.FloatParams:
			out[c.Type+"/"+p.Instance] = p.Unit
		}
	}
	return out
}

// describe - умение или свойство из описания устройства Алисы по-русски: что это и какие
// значения оно принимает.
func describe(c alice.Capability) aliceItem {
	it := aliceItem{Label: kindNames[c.Type], Tech: shortKind(c.Type)}
	names := func(list []alice.ValueItem, labels map[string]string) string {
		var out []string
		for _, v := range list {
			out = append(out, nameOr(labels, v.Value))
		}
		return strings.Join(out, ", ")
	}
	switch p := c.Parameters.(type) {
	case alice.ModeParams:
		it.Label, it.Tech = itemLabel(c.Type, p.Instance), it.Tech+" "+p.Instance
		it.Value = names(p.Modes, modeValueNames)
	case alice.RangeParams:
		it.Label, it.Tech = itemLabel(c.Type, p.Instance), it.Tech+" "+p.Instance
		it.Value = fmt.Sprintf("от %s до %s, шаг %s", formatValue(p.Range.Min), withUnit(p.Range.Max, p.Unit), formatValue(p.Range.Precision))
	case alice.InstanceParams:
		it.Label, it.Tech = itemLabel(c.Type, p.Instance), it.Tech+" "+p.Instance
		it.Value = "включено или выключено"
	case alice.FloatParams:
		it.Label, it.Tech = itemLabel(c.Type, p.Instance), it.Tech+" "+p.Instance
		it.Value = "число"
		if u := nameOr(unitNames, p.Unit); u != "" {
			it.Value += ", " + u
		}
		if p.Instance == hcdevice.RemainingTime {
			it.Value = "секунды до конца программы; нет в документации Яндекса"
		}
	case alice.EventParams:
		it.Label, it.Tech = itemLabel(c.Type, p.Instance), it.Tech+" "+p.Instance
		it.Value = names(p.Events, eventValueNames)
	case alice.ColorParams:
		var parts []string
		if p.ColorModel != "" {
			parts = append(parts, "любой цвет, модель "+p.ColorModel)
		}
		if p.ColorScene != nil {
			var scenes []string
			for _, sc := range p.ColorScene.Scenes {
				scenes = append(scenes, nameOr(sceneNames, sc.ID))
			}
			parts = append(parts, "сцены: "+strings.Join(scenes, ", "))
		}
		it.Value = strings.Join(parts, "; ")
	default:
		if c.Type == alice.CapOnOff {
			it.Value = "включить и выключить"
		}
	}
	return it
}

// named - варианты выбора с подписями по-русски: «Выпечка (baking)». blank - подпись пустого
// варианта; пустая подпись - пустого варианта нет.
func named(list []string, cur, blank string, names map[string]string) []choice {
	out := choices(list, cur, blank, nil)
	for i, c := range out {
		if n, ok := names[c.Value]; ok && c.Value != "" {
			out[i].Label = n + " (" + c.Value + ")"
		}
	}
	return out
}

// typeIcons - значок прибора по типу Home Connect, LG или De'Longhi.
var typeIcons = map[string]string{
	"Oven": "oven", "Dishwasher": "dishwasher", "Washer": "washer", "WasherDryer": "washer", "Dryer": "dryer",
	"CoffeeMaker": "coffee", profile.DeLonghiType: "coffee", profile.PolarisKettleType: "coffee", "Hood": "hood", "Refrigerator": "fridge",
	"FridgeFreezer": "fridge", "Freezer": "fridge", "WineCooler": "fridge", "CleaningRobot": "robot",
	"Microwave": "oven", "WarmingDrawer": "oven", "AirConditioner": "ac",
	"DEVICE_WASHER": "washer", "DEVICE_WASHTOWER": "washer", "DEVICE_WASHTOWER_WASHER": "washer",
	"DEVICE_WASHCOMBO_MAIN": "washer", "DEVICE_WASHCOMBO_MINI": "washer", "DEVICE_DRYER": "dryer",
	"DEVICE_WASHTOWER_DRYER": "dryer", "DEVICE_DISH_WASHER": "dishwasher", "DEVICE_OVEN": "oven",
	"DEVICE_MICROWAVE_OVEN": "oven", "DEVICE_HOOD": "hood", "DEVICE_REFRIGERATOR": "fridge",
	"DEVICE_KIMCHI_REFRIGERATOR": "fridge", "DEVICE_WINE_CELLAR": "fridge", "DEVICE_ROBOT_CLEANER": "robot",
	"DEVICE_STICK_CLEANER": "robot", "DEVICE_AIR_CONDITIONER": "ac", profile.LGTVType: "tv", profile.SamsungTVType: "tv",
}

// iconNames - значки приборов по-русски, для выбора своего значка.
var iconNames = map[string]string{"oven": "духовка", "dishwasher": "посудомойка", "washer": "стиральная машина",
	"dryer": "сушильная машина", "coffee": "кофемашина", "hood": "вытяжка", "fridge": "холодильник",
	"robot": "робот-пылесос", "light": "лампа", "ac": "кондиционер", "air": "очиститель воздуха", "tv": "телевизор",
	"sensor": "датчик", "socket": "розетка", "device": "общий значок"}

// yandexIcons - значок прибора по типу устройства Алисы, если по типу прибора его нет.
var yandexIcons = map[string]string{
	"cooking": "oven", "cooking.coffee_maker": "coffee", "cooking.kettle": "coffee", "dishwasher": "dishwasher",
	"washing_machine": "washer", "vacuum_cleaner": "robot", "thermostat": "ac", "thermostat.ac": "ac",
	"purifier": "air", "humidifier": "air", "ventilation": "air", "ventilation.fan": "air",
	"media_device": "tv", "media_device.tv": "tv", "media_device.tv_box": "tv", "media_device.receiver": "tv",
	"socket": "socket", "switch": "socket", "sensor": "sensor", "sensor.button": "sensor", "sensor.climate": "sensor",
	"sensor.illumination": "sensor", "sensor.motion": "sensor", "sensor.open": "sensor", "sensor.smoke": "sensor",
	"sensor.water_leak": "sensor", "smart_meter.electricity": "socket",
}

// deviceIcon - значок прибора: по типу прибора, по типу устройства Алисы, у света - лампа,
// иначе - общий значок.
func deviceIcon(haType, yandexType string) string {
	t := strings.TrimPrefix(yandexType, "devices.types.")
	if i, ok := typeIcons[haType]; ok {
		return i
	}
	if i, ok := yandexIcons[t]; ok {
		return i
	}
	if strings.HasPrefix(t, "light") {
		return "light"
	}
	return "device"
}
