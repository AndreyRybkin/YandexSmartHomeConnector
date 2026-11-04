package profile

// AllInOneType - демо-прибор для проверки у Яндекса: все свойства из документации Яндекса и кнопка
// в одном устройстве. Кнопка и уровни заданы свойствами, без раздела events.
// Значения свойств равны значениям Алисы; имена - как у прибора в пакете appliance.
const AllInOneType = "AllInOne"

func demoFloat(instance, status, unit string) Property {
	return Property{Type: "float", Instance: instance, Status: "Demo.Status." + status, Unit: unit}
}

func demoEvent(instance, status string, values ...string) Property {
	m := make(map[string]string, len(values))
	for _, v := range values {
		m[v] = v
	}
	return Property{Type: "event", Instance: instance, Status: "Demo.Status." + status, Values: m}
}

func demoMode(instance, setting string, values ...string) Mode {
	m := make(map[string]string, len(values))
	for _, v := range values {
		m[v] = v
	}
	return Mode{Instance: instance, Option: "Demo.Setting." + setting, Values: m}
}

func init() {
	builtin[AllInOneType] = &Profile{
		YandexType:     "cooking",
		OnOff:          "program",
		DefaultProgram: "Demo.Program.One",
		DefaultOptions: map[string]any{"Demo.Option.Temperature": 60.0},
		Modes: []Mode{{Instance: "program", Programs: map[string]string{"Demo.Program.One": "one", "Demo.Program.Two": "two"}},
			demoMode("work_speed", "WorkSpeed", "medium", "slow", "fast", "turbo", "max"),
			demoMode("swing", "Swing", "stationary", "vertical", "horizontal", "auto"),
			demoMode("heat", "Heat", "normal", "min", "low", "high", "max"),
			demoMode("cleanup_mode", "Cleanup", "auto", "quiet", "normal", "express", "dry_cleaning", "wet_cleaning", "mixed_cleaning", "smart"),
			demoMode("fan_speed", "FanSpeed", "auto", "low", "medium", "high", "turbo"),
			demoMode("input_source", "Input", "one", "two", "three")},
		Ranges: []Range{{Instance: "temperature", Option: "Demo.Option.Temperature", Unit: celsius}},
		Toggles: []Toggle{pause,
			{Instance: "controls_locked", Setting: "Demo.Setting.ChildLock"},
			{Instance: "mute", Setting: "Demo.Setting.Mute"},
			{Instance: "ionization", Setting: "Demo.Setting.Ionization"},
			{Instance: "keep_warm", Setting: "Demo.Setting.KeepWarm"},
			{Instance: "backlight", Setting: "Demo.Setting.Light"}},
		Properties: []Property{
			demoFloat("temperature", "Temperature", celsius),
			demoFloat("humidity", "Humidity", percent),
			demoFloat("illumination", "Illumination", "unit.illumination.lux"),
			demoFloat("battery_level", "BatteryLevel", percent),
			demoFloat("co2_level", "CO2Level", "unit.ppm"),
			demoFloat("pm1_density", "PM1Density", density),
			demoFloat("pm2.5_density", "PM25Density", density),
			demoFloat("pm10_density", "PM10Density", density),
			demoFloat("power", "Power", "unit.watt"),
			demoFloat("electricity_meter", "ElectricityMeter", "unit.kilowatt_hour"),
			demoFloat("voltage", "Voltage", "unit.volt"),
			// Главный вопрос проверки: water_level и числом, и событием в одном устройстве.
			demoFloat("water_level", "WaterLevelFloat", percent),
			demoFloat("amperage", "Amperage", "unit.ampere"),
			demoFloat("food_level", "FoodLevelFloat", percent),
			demoFloat("gas_meter", "GasMeter", "unit.cubic_meter"),
			demoFloat("heat_meter", "HeatMeter", "unit.gigacalorie"),
			demoFloat("meter", "Meter", ""),
			demoFloat("pressure", "Pressure", "unit.pressure.mmhg"),
			demoFloat("tvoc", "TVOC", density),
			demoFloat("water_meter", "WaterMeter", "unit.cubic_meter"),
			demoEvent("water_level", "WaterLevel", "empty", "low", "normal"),
			demoEvent("food_level", "FoodLevel", "empty", "low", "normal"),
			demoEvent("open", "Open", "opened", "closed"),
			demoEvent("motion", "Motion", "detected", "not_detected"),
			demoEvent("water_leak", "WaterLeak", "dry", "leak"),
			demoEvent("smoke", "Smoke", "detected", "not_detected", "high"),
			demoEvent("gas", "Gas", "detected", "not_detected", "high"),
			demoEvent("vibration", "Vibration", "tilt", "fall", "vibration"),
			demoEvent("battery_level", "Battery", "low", "normal"),
			demoEvent("button", "Button", "click", "double_click", "long_press"),
		},
	}
}
