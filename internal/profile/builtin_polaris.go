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
		// Режим program выбирает работу и сразу её запускает. У выключенного чайника режима нет.
		Modes: []Mode{{Instance: "tea_mode", Option: "tea", Values: map[string]string{
			"black": "black_tea", "green": "green_tea", "flower": "flower_tea", "red": "red_tea",
			"puerh": "puerh_tea", "oolong": "oolong_tea", "white": "white_tea", "herbal": "herbal_tea"}},
			{Instance: "program", Option: "mode", Values: map[string]string{
				"1": "boiling", "2": "one", "3": "heat", "4": "two", "5": "smart", "6": "three"}}},
		// Звук у чайника - настройка sound: mute включён, когда она false.
		Toggles: []Toggle{{Instance: "backlight", Setting: "backlight"}, {Instance: "controls_locked", Setting: "child_lock"},
			{Instance: "mute", Setting: "sound", On: "false", Off: "true"},
			// Удержание тепла - виртуальная функция шлюза: режим с удержанием и без него.
			{Instance: "keep_warm", Setting: "Polaris.KeepWarm"}},
		Properties: []Property{{Type: "float", Instance: "temperature", Status: "sensor/temperature", Unit: celsius},
			{Type: "float", Instance: "water_level", Status: "weight", Unit: percent, Convert: WeightToPercent, Min: &noWater, Max: &fullWater}},
		Events: &Events{
			// Polaris.Event.BoilFinished - чайник перестал работать сам: его не сняли с базы и не выключили
			// командой шлюза. Выключение кнопкой на чайнике от конца работы не отличить.
			// Коды 03 и 04 - сбой датчика температуры, 06 и 07 - пора сменить воду.
			Button: map[string]string{"Polaris.Event.BoilFinished": "click", "error/empty_base=true": "long_press",
				"error/code=03": "double_click", "error/code=04": "double_click", "error/code=06": "double_click", "error/code=07": "double_click"},
			// Код 1 - мало воды, код 2 - чайник пуст.
			WaterLevel: []string{"Polaris.Alarm.Water", "Polaris.Alarm.NoWater"},
			Empty:      []string{"Polaris.Alarm.NoWater"},
		},
	}
}
