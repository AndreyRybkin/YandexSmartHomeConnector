package profile

// Встроенные профили телевизоров в домашней сети: LG webOS и Samsung на Tizen. Функции у
// обоих одинаковые - TV.* из пакета tv; отличаются только имена входов.

// Типы телевизоров в домашней сети.
const (
	LGTVType      = "LGTV"
	SamsungTVType = "SamsungTV"
)

func init() {
	builtin[LGTVType] = tvProfile(map[string]string{"HDMI_1": "one", "HDMI_2": "two", "HDMI_3": "three",
		"HDMI_4": "four", "LIVE_TV": "five"})
	builtin[SamsungTVType] = tvProfile(map[string]string{"HDMI1": "one", "HDMI2": "two", "HDMI3": "three",
		"HDMI4": "four", "LIVE_TV": "five"})
}

// tvProfile - телевизор: питание, громкость, канал, звук, пауза и источник сигнала. Входы
// HDMI - режимы «первый»-«четвёртый», эфир - «пятый».
func tvProfile(inputs map[string]string) *Profile {
	return &Profile{
		YandexType: "media_device.tv",
		OnOff:      "power",
		Power:      &Power{Setting: "TV.Power", On: "on", Off: "off"},
		Modes:      []Mode{{Instance: "input_source", Option: "TV.Input", Values: inputs}},
		Ranges:     []Range{{Instance: "volume", Option: "TV.Volume", Unit: percent}, {Instance: "channel", Option: "TV.Channel"}},
		Toggles: []Toggle{{Instance: "mute", Setting: "TV.Mute"},
			{Instance: "pause", Setting: "TV.Playback", On: "paused", Off: "playing"}},
	}
}
