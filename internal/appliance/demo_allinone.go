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

// allInOneToggles - настройки-флаги для умений toggle; allInOneModes - настройки со значениями для
// умений mode: значения равны значениям Алисы, первое - начальное.
var (
	allInOneToggles = []string{"Demo.Setting.ChildLock", "Demo.Setting.Mute", "Demo.Setting.Ionization", "Demo.Setting.KeepWarm", "Demo.Setting.Light"}
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
	return Description{
		Type:     DemoAllInOne,
		Programs: []string{"Demo.Program.One", "Demo.Program.Two"},
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
