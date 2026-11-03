package web

import (
	"maps"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"hcgateway/internal/alice"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/profile"
)

// Показания на плитке прибора: строка до profile.MaxTile значений под меткой состояния. Значения те же,
// что видит Алиса. Состав задаёт поле профиля tile, а без него - правила ниже.

const tileSeparator = " · "

// tileFloatOrder - порядок показаний: главное впереди. Остальные числа, например своё свойство, идут
// за ними в порядке профиля.
var tileFloatOrder = []string{"temperature", "humidity", "water_level", "battery_level", "co2_level", "pm2.5_density", "tvoc",
	"power", "electricity_meter", "illumination", "pressure", "water_meter", "gas_meter", "heat_meter", "meter",
	"pm1_density", "pm10_density", "voltage", "amperage"}

// tileSettingOrder - порядок настроек.
var tileSettingOrder = []string{"range/temperature", "range/humidity", "mode/thermostat", "mode/input_source", "mode/fan_speed",
	"mode/tea_mode", "mode/heat", "range/volume", "range/channel", "range/open", "range/brightness"}

// tileLabels - короткие подписи значений. Температура, мощность и счётчики понятны по единице.
var tileLabels = map[string]string{
	"float/humidity": "влажность", "float/water_level": "вода", "float/battery_level": "заряд", "float/co2_level": "CO₂",
	"float/pm1_density": "PM1", "float/pm2.5_density": "PM2.5", "float/pm10_density": "PM10", "float/tvoc": "TVOC",
	"float/temperature": "", "float/power": "", "float/electricity_meter": "", "float/illumination": "", "float/pressure": "",
	"float/water_meter": "", "float/gas_meter": "", "float/heat_meter": "", "float/meter": "", "float/voltage": "", "float/amperage": "",
	"range/temperature": "цель", "range/humidity": "цель", "range/volume": "громкость", "range/channel": "канал",
	"range/brightness": "яркость", "range/open": "открыто на",
	"mode/fan_speed": "скорость", "mode/input_source": "вход", "mode/heat": "нагрев", "mode/thermostat": "", "mode/tea_mode": "",
}

// tileCalm - значения события, когда ничего не случилось: на плитке прибора их нет, а датчику это
// главное показание.
var tileCalm = []string{"closed", "not_detected", "normal", "dry"}

// tileText - показания плитки одной строкой; пусто - показывать нечего. Прибор без связи показаний не
// имеет: они могли устареть.
func tileText(hc *hcdevice.Device, st *status) string {
	if !st.Online {
		return ""
	}
	states := hc.States()
	unitsByKey := map[string]string{}
	for _, info := range hc.Info() {
		maps.Copy(unitsByKey, units(info))
	}
	var parts []string
	if hc.Profile.Tile != nil {
		parts = chosenTile(hc, states, unitsByKey)
	} else {
		parts = autoTile(hc, states, unitsByKey, st)
	}
	return strings.Join(parts, tileSeparator)
}

// chosenTile - значения, которые владелец выбрал в профиле, в заданном порядке.
func chosenTile(hc *hcdevice.Device, states []alice.DeviceState, units map[string]string) []string {
	var parts []string
	values := hc.Appliance.State().Values
	for _, item := range hc.Profile.Tile {
		if item.Status != "" {
			if raw, ok := profile.StatusValue(values, item.Status); ok {
				parts = append(parts, tileJoin(item.Label, tileRaw(raw, item.Unit)))
			}
			continue
		}
		kind, instance, _ := strings.Cut(item.Item, "/")
		for _, ds := range states {
			for _, c := range append(slices.Clone(ds.Capabilities), ds.Properties...) {
				sv, ok := c.State.(alice.StateValue)
				if !ok || shortKind(c.Type) != kind || sv.Instance != instance {
					continue
				}
				text := tileValue(c.Type, sv, units[c.Type+"/"+sv.Instance], hc.Profile, true)
				if item.Label != "" {
					text = tileJoin(item.Label, tileBare(c.Type, sv, units[c.Type+"/"+sv.Instance]))
				}
				parts = append(parts, text)
			}
		}
	}
	return parts
}

// autoTile - значения по правилам: сначала сработавшие события, затем измерения, затем настройки.
// Настройки видны, только пока прибор включён, а также у приборов без кнопки питания: у выключенного
// кондиционера цель ничего не значит. Программа и оставшееся время уже есть в метке состояния.
func autoTile(hc *hcdevice.Device, states []alice.DeviceState, units map[string]string, st *status) []string {
	var events, floats, settings []tileEntry
	p := hc.Profile
	sensor := strings.HasPrefix(p.YandexType, "sensor")
	active := st.On || !st.Switch
	for _, ds := range states {
		if ds.ID != hc.ID {
			continue
		}
		for _, c := range ds.Properties {
			sv, ok := c.State.(alice.StateValue)
			if !ok {
				continue
			}
			switch c.Type {
			case alice.PropEvent:
				if sv.Instance == alice.InstanceButton || !sensor && slices.Contains(tileCalm, formatValue(sv.Value)) {
					continue
				}
				events = append(events, tileEntry{text: tileValue(c.Type, sv, "", p, sensor)})
			case alice.PropFloat:
				// Низкий заряд уже показан отдельной меткой, а оставшееся время - в метке состояния.
				if sv.Instance == hcdevice.RemainingTime || sv.Instance == "battery_level" && st.Problem == "battery" {
					continue
				}
				floats = append(floats, tileEntry{order: tileRank(tileFloatOrder, sv.Instance),
					text: tileValue(c.Type, sv, units[c.Type+"/"+sv.Instance], p, sensor)})
			}
		}
		if !active {
			continue
		}
		for _, c := range ds.Capabilities {
			sv, ok := c.State.(alice.StateValue)
			if !ok {
				continue
			}
			switch c.Type {
			case alice.CapRange:
			case alice.CapMode:
				if modeOfPrograms(p, sv.Instance) {
					continue
				}
			default:
				continue
			}
			settings = append(settings, tileEntry{order: tileRank(tileSettingOrder, shortKind(c.Type)+"/"+sv.Instance),
				text: tileValue(c.Type, sv, units[c.Type+"/"+sv.Instance], p, sensor)})
		}
	}
	var parts []string
	for _, group := range [][]tileEntry{events, floats, settings} {
		slices.SortStableFunc(group, func(a, b tileEntry) int { return a.order - b.order })
		for _, e := range group {
			parts = append(parts, e.text)
		}
	}
	return parts[:min(len(parts), profile.MaxTile)]
}

// tileEntry - значение плитки и его место в группе.
type tileEntry struct {
	order int
	text  string
}

// tileRank - место в списке; чего нет в списке - после всего списка.
func tileRank(order []string, key string) int {
	if i := slices.Index(order, key); i >= 0 {
		return i
	}
	return len(order)
}

// modeOfPrograms сообщает, что режим - программа прибора: её показывает метка состояния.
func modeOfPrograms(p *profile.Profile, instance string) bool {
	return slices.ContainsFunc(p.Modes, func(m profile.Mode) bool { return m.Instance == instance && m.Programs != nil })
}

// tileValue - значение с подписью для плитки.
func tileValue(kind string, sv alice.StateValue, unit string, p *profile.Profile, sensor bool) string {
	key := shortKind(kind) + "/" + sv.Instance
	if kind == alice.PropEvent {
		if sv.Instance == "open" && !sensor {
			if sv.Value == "opened" {
				return "дверца открыта"
			}
			return "дверца закрыта"
		}
		return lowerFirst(nameOr(eventValueNames, formatValue(sv.Value)))
	}
	label, known := tileLabels[key]
	switch {
	case key == "range/temperature" && p.OnOff == "none":
		label = "задано"
	case !known && kind == alice.CapToggle:
		label = lowerFirst(nameOr(toggleInstanceNames, sv.Instance))
	case !known && kind == alice.CapColor:
		label = lowerFirst(nameOr(colorInstanceNames, sv.Instance))
	case !known && kind == alice.CapMode:
		label = lowerFirst(nameOr(modeInstanceNames, sv.Instance))
	case !known:
		// Своё свойство: шлюз знает только его имя.
		label = nameOr(floatInstanceNames, sv.Instance)
		if label != sv.Instance {
			label = lowerFirst(label)
		}
	}
	return tileJoin(label, tileBare(kind, sv, unit))
}

// tileBare - значение без подписи.
func tileBare(kind string, sv alice.StateValue, unit string) string {
	switch kind {
	case alice.CapRange, alice.PropFloat:
		if f, ok := sv.Value.(float64); ok {
			return withUnit(tileRound(f, unit), unit)
		}
	case alice.CapMode:
		return lowerFirst(nameOr(modeValueNames, formatValue(sv.Value)))
	case alice.PropEvent:
		return lowerFirst(nameOr(eventValueNames, formatValue(sv.Value)))
	}
	return itemValue(kind, sv, unit)
}

// tileRaw - значение прибора: число с единицей или последняя часть имени.
func tileRaw(raw any, unit string) string {
	if f, ok := raw.(float64); ok {
		return withUnit(tileRound(f, unit), unit)
	}
	return short(formatValue(raw))
}

// tileRound - число для плитки: проценты целыми, остальное - до десятых.
func tileRound(f float64, unit string) float64 {
	if unit == "unit.percent" {
		return math.Round(f)
	}
	return math.Round(f*10) / 10
}

func tileJoin(label, value string) string {
	if label == "" {
		return value
	}
	return label + " " + value
}

// lowerFirst - строка со строчной первой буквой: подписи режимов и событий внутри строки показаний.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}
