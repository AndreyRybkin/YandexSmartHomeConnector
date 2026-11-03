package profile

// PolarisKettleType - тип чайника Polaris. Имена функций - как в топиках прибора после
// polaris/<id>/state/; значения режимов - в пакете polaris.
const PolarisKettleType = "PolarisKettle"

func init() {
	celsius := "unit.temperature.celsius"
	minT, maxT, precision := 30.0, 100.0, 1.0
	noWater, fullWater := 0.0, 100.0
	builtin[PolarisKettleType] = &Profile{
		YandexType: "cooking.kettle",
		OnOff:      "power",
		// «Включи» - кипячение. Чайник работает, пока режим не 0.
		Power:  &Power{Setting: "mode", On: "1", Off: "0", State: "mode", Idle: []string{"0"}},
		Ranges: []Range{{Instance: "temperature", Option: "temperature", Unit: celsius, Min: &minT, Max: &maxT, Precision: &precision}},
		Modes: []Mode{{Instance: "tea_mode", Option: "tea", Values: map[string]string{
			"black": "black_tea", "green": "green_tea", "flower": "flower_tea", "red": "red_tea",
			"puerh": "puerh_tea", "oolong": "oolong_tea", "white": "white_tea", "herbal": "herbal_tea"}}},
		Toggles: []Toggle{{Instance: "backlight", Setting: "backlight"}, {Instance: "controls_locked", Setting: "child_lock"}},
		Properties: []Property{{Type: "float", Instance: "temperature", Status: "sensor/temperature", Unit: celsius},
			{Type: "float", Instance: "water_level", Status: "weight", Unit: percent, Convert: WeightToPercent, Min: &noWater, Max: &fullWater}},
		Events: &Events{
			// Переход в режим 0 - конец работы, и после выключения вручную: чайник их не различает.
			Button: map[string]string{"mode=0": "click", "error/empty_base=true": "long_press"},
			// Код 01 - мало воды; пустой ли чайник, прибор отдельно не сообщает.
			WaterLevel: []string{"Polaris.Alarm.Water"},
		},
	}
}
