package profile

// AllInOneType - демо-прибор для проверки у Яндекса: все свойства встроенных профилей и кнопка
// в одном устройстве. Раздела событий нет, поэтому отдельного датчика у прибора тоже нет.
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

func init() {
	builtin[AllInOneType] = &Profile{
		YandexType:     "cooking",
		OnOff:          "program",
		DefaultProgram: "Demo.Program.One",
		DefaultOptions: map[string]any{"Demo.Option.Temperature": 60.0},
		Modes:          []Mode{{Instance: "program", Programs: map[string]string{"Demo.Program.One": "one", "Demo.Program.Two": "two"}}},
		Ranges:         []Range{{Instance: "temperature", Option: "Demo.Option.Temperature", Unit: celsius}},
		Toggles:        []Toggle{pause},
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
			demoEvent("water_level", "WaterLevel", "empty", "low", "normal"),
			demoEvent("food_level", "FoodLevel", "empty", "low", "normal"),
			demoEvent("open", "Open", "opened", "closed"),
			demoEvent("motion", "Motion", "detected", "not_detected"),
			demoEvent("water_leak", "WaterLeak", "dry", "leak"),
			demoEvent("smoke", "Smoke", "detected", "not_detected"),
			demoEvent("button", "Button", "click", "double_click", "long_press"),
		},
	}
}
