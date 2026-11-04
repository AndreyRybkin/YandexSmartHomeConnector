package profile

import (
	"maps"
	"slices"
)

// Встроенные профили телевизоров в домашней сети: LG webOS и Samsung на Tizen. Функции у
// обоих - TV.* из пакета tv; отличаются имена входов, а экран и приложения есть только у LG.

// Типы телевизоров в домашней сети.
const (
	LGTVType      = "LGTV"
	SamsungTVType = "SamsungTV"
)

func init() {
	builtin[LGTVType] = tvProfile(map[string]string{"HDMI_1": "one", "HDMI_2": "two", "HDMI_3": "three",
		"HDMI_4": "four", "LIVE_TV": "five"})
	// Экран гасить умеет только LG; звук при этом продолжает играть.
	lg := builtin[LGTVType]
	lg.Toggles = append(lg.Toggles, Toggle{Instance: "backlight", Setting: "TV.Screen", On: "on", Off: "off"})
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

// TVAppsLayer - слой профиля телевизора LG из настроек прибора: режим program запускает приложения.
// apps - режим Алисы -> идентификатор приложения webOS; пусто - слоя нет и режима нет. Слой лежит под
// профилем прибора и поверх профиля типа, а список режимов заменяется целиком, поэтому в слое
// остаются режимы типа forType.
func TVAppsLayer(apps map[string]string, forType *Profile) *Profile {
	if len(apps) == 0 {
		return nil
	}
	modes := slices.DeleteFunc(slices.Clone(Merge(builtin[LGTVType], forType).Modes), func(m Mode) bool { return m.Instance == "program" })
	values := make(map[string]string, len(apps))
	for _, mode := range slices.Sorted(maps.Keys(apps)) {
		values[apps[mode]] = mode
	}
	return &Profile{Modes: append(modes, Mode{Instance: "program", Option: "TV.App", Values: values})}
}
