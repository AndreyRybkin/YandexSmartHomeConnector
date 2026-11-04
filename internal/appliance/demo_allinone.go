package appliance

// DemoAllInOne - демо-прибор для проверки у Яндекса: все свойства из документации Яндекса и кнопка
// в одном устройстве. Значения меняются за программу allInOneSteps раз, чтобы уведомления
// Яндексу содержали каждое свойство.
const DemoAllInOne = "AllInOne"

const (
	allInOneSteps  = 4
	allInOneOption = "Demo.Option.Temperature"
	allInOneButton = "Demo.Status.Button"
	// allInOneNoPress - значение кнопки без нажатия: профиль его не знает, Алиса его не получает.
	allInOneNoPress = "none"
)

// allInOneFloats - числовые показания: значение в начале программы и прибавка за шаг.
var allInOneFloats = map[string]struct{ start, step float64 }{
	"Demo.Status.Temperature":      {22, 20},
	"Demo.Status.Humidity":         {40, 5},
	"Demo.Status.Illumination":     {100, 50},
	"Demo.Status.BatteryLevel":     {100, -5},
	"Demo.Status.CO2Level":         {400, 100},
	"Demo.Status.PM1Density":       {5, 1},
	"Demo.Status.PM25Density":      {8, 2},
	"Demo.Status.PM10Density":      {10, 3},
	"Demo.Status.Power":            {0, 100},
	"Demo.Status.ElectricityMeter": {1, 0.5},
	"Demo.Status.Voltage":          {220, 1},
	"Demo.Status.WaterLevelFloat":  {100, -20},
	"Demo.Status.Amperage":         {1, 0.5},
	"Demo.Status.FoodLevelFloat":   {100, -20},
	"Demo.Status.GasMeter":         {10, 0.5},
	"Demo.Status.HeatMeter":        {5, 0.5},
	"Demo.Status.Meter":            {100, 1},
	"Demo.Status.Pressure":         {740, 5},
	"Demo.Status.TVOC":             {50, 10},
	"Demo.Status.WaterMeter":       {20, 0.5},
}

// allInOneEvents - показания-события: значения идут по кругу, первое - в начале программы.
var allInOneEvents = map[string][]string{
	"Demo.Status.Open":       {"closed", "opened"},
	"Demo.Status.Motion":     {"not_detected", "detected"},
	"Demo.Status.WaterLeak":  {"dry", "leak"},
	"Demo.Status.Smoke":      {"not_detected", "detected", "high"},
	"Demo.Status.Gas":        {"not_detected", "detected", "high"},
	"Demo.Status.Vibration":  {"tilt", "fall", "vibration"},
	"Demo.Status.Battery":    {"normal", "low"},
	"Demo.Status.FoodLevel":  {"normal", "low", "empty"},
	"Demo.Status.WaterLevel": {"normal", "low", "empty"},
}

// allInOneExtra - остальные значения режима из списка Яндекса: программы Demo.Program.<значение>. Яндекс
// видит их на демо-приборе раньше, чем на настоящем: отвергнутое значение закрывает всё устройство.
var allInOneExtra = []string{
	"wet_cleaning", "dry_cleaning", "mixed_cleaning", "auto", "eco", "turbo", "cool", "dry", "fan_only",
	"preheat", "high", "low", "medium", "max", "min", "fast", "slow", "express", "normal", "quiet",
	"horizontal", "stationary", "vertical", "supply_air", "extraction_air", "three", "four", "five", "six",
	"seven", "eight", "nine", "ten", "americano", "cappuccino", "double_espresso", "espresso", "latte",
	"black_tea", "flower_tea", "green_tea", "herbal_tea", "oolong_tea", "puerh_tea", "red_tea", "white_tea",
	"glass", "intensive", "pre_rinse", "aspic", "baby_food", "baking", "bread", "cereals", "cheesecake",
	"deep_fryer", "dessert", "fowl", "frying", "macaroni", "milk_porridge", "multicooker", "pasta", "pilaf",
	"pizza", "sauce", "slow_cook", "soup", "steam", "stewing", "vacuum", "yogurt",
}

// allInOneRanges - настройки-числа для умений range: имя и предел сверху.
var allInOneRanges = map[string]float64{"Demo.Setting.Brightness": 100, "Demo.Setting.Humidity": 100, "Demo.Setting.Open": 100,
	"Demo.Setting.Volume": 100, "Demo.Setting.Channel": 999}

// allInOneToggles - настройки-флаги для умений toggle; allInOneModes - настройки со значениями для
// умений mode: значения равны значениям Алисы, первое - начальное.
var (
	allInOneToggles = []string{"Demo.Setting.ChildLock", "Demo.Setting.Mute", "Demo.Setting.Ionization", "Demo.Setting.KeepWarm", "Demo.Setting.Light", "Demo.Setting.Oscillation"}
	allInOneModes   = map[string][]string{
		"Demo.Setting.WorkSpeed": {"medium", "slow", "fast", "turbo", "max"},
		"Demo.Setting.Swing":     {"stationary", "vertical", "horizontal", "auto"},
		"Demo.Setting.Heat":      {"normal", "min", "low", "high", "max"},
		"Demo.Setting.Cleanup":   {"auto", "quiet", "normal", "express", "dry_cleaning", "wet_cleaning", "mixed_cleaning", "smart"},
		"Demo.Setting.FanSpeed":  {"auto", "low", "medium", "high", "turbo"},
		"Demo.Setting.Input":     {"one", "two", "three"},
	}
)

// allInOnePresses - нажатие кнопки на шаге программы. Шаг 4 - её конец.
var allInOnePresses = map[int]string{2: "double_click", 3: "long_press", 4: "click"}

func allInOneDescription() Description {
	options := map[string]Option{
		allInOneOption:    {Key: allInOneOption, Min: 30, Max: 100, Step: 5},
		SettingPowerState: {Key: SettingPowerState, Values: []string{PowerOn, PowerOff}},
	}
	for _, key := range allInOneToggles {
		options[key] = Option{Key: key, Setting: true}
	}
	for key, values := range allInOneModes {
		options[key] = Option{Key: key, Values: values, Setting: true}
	}
	for key, top := range allInOneRanges {
		options[key] = Option{Key: key, Min: 1, Max: top, Step: 1, Setting: true}
	}
	programs := []string{"Demo.Program.One", "Demo.Program.Two", "Demo.Program.Boil", "Demo.Program.Heat", "Demo.Program.Smart"}
	for _, value := range allInOneExtra {
		programs = append(programs, "Demo.Program."+value)
	}
	return Description{
		Type:     DemoAllInOne,
		Programs: programs,
		Options:  options,
		Commands: []string{CommandPause, CommandResume},
	}
}

// setAllInOneSettings записывает начальные настройки: новая программа их не сбрасывает.
func (d *Demo) setAllInOneSettings() {
	for _, key := range allInOneToggles {
		d.state.Values[key] = false
	}
	for key, values := range allInOneModes {
		d.state.Values[key] = values[0]
	}
	for key := range allInOneRanges {
		d.state.Values[key] = 50.0
	}
}

// setAllInOneStep записывает показания шага; блокировку держит вызывающий.
func (d *Demo) setAllInOneStep(step int) {
	d.step = step
	for status, f := range allInOneFloats {
		d.state.Values[status] = f.start + float64(step)*f.step
	}
	for status, values := range allInOneEvents {
		d.state.Values[status] = values[step%len(values)]
	}
	press, ok := allInOnePresses[step]
	if !ok {
		press = allInOneNoPress
	}
	d.state.Values[allInOneButton] = press
}
