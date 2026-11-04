package web

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/profile"
)

// Сводка сопоставлений на странице прибора: что Алиса берёт у прибора, как пересчитывает и
// что видит сейчас.

// mapRow - строка сводки. Key и Now - функция прибора и её значение сейчас, Label - имя
// функции коротко, Program - вместо функции программы прибора; Convert - пересчёт словами,
// пусто - без пересчёта; Alice и Value - умение Алисы и что Алиса видит сейчас; Section -
// раздел редактора для «Изменить».
type mapRow struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Note - что значит функция словами: у тревоги кофемашины.
	Note    string `json:"note"`
	Program bool   `json:"program"`
	Now     string `json:"now"`
	Convert string `json:"convert"`
	Alice   string `json:"alice"`
	Value   string `json:"value"`
	Section string `json:"section"`
}

// mapRows - сводка сопоставлений прибора по действующему профилю p, в порядке разделов
// редактора.
func mapRows(p *profile.Profile, st *appliance.State, dev alice.Device) []mapRow {
	seen := aliceValues(dev)
	var out []mapRow
	add := func(key, convert, kind, instance, section string) {
		if key == "" {
			return
		}
		r := mapRow{Key: key, Label: short(key), Convert: convert, Alice: itemLabel(kind, instance), Value: seen[kind+"/"+instance],
			Section: section}
		if v, ok := profile.StatusValue(st.Values, key); ok {
			r.Now = shortValue(v)
		}
		out = append(out, r)
	}
	switch pw := p.Power; {
	case pw != nil:
		add(pw.Setting, valuePair(pw.On, pw.Off), alice.CapOnOff, "on", "on_off")
		if pw.State != "" {
			add(pw.State, "включён, кроме: "+strings.Join(pw.Idle, ", "), alice.CapOnOff, "on", "on_off")
		}
		add(pw.Allowed, "false - «включи» не выполняется", alice.CapOnOff, "on", "on_off")
	case p.OnOff == "power":
		add(appliance.SettingPowerState, "", alice.CapOnOff, "on", "on_off")
	}
	if p.OnOff == "program" {
		add(appliance.StatusOperationState, "включён, пока идёт программа", alice.CapOnOff, "on", "on_off")
	}
	for _, m := range p.Modes {
		if m.Programs == nil {
			add(m.Option, "таблица: "+count(len(m.Values), "значение", "значения", "значений"), alice.CapMode, m.Instance, "modes")
			continue
		}
		r := mapRow{Program: true, Now: "не идёт", Convert: "таблица: " + count(len(m.Programs), "программа", "программы", "программ"),
			Alice: itemLabel(alice.CapMode, m.Instance), Value: seen[alice.CapMode+"/"+m.Instance], Section: "modes"}
		if st.ActiveProgram != "" {
			r.Now = programShort(st.ActiveProgram)
		}
		out = append(out, r)
	}
	for _, r := range p.Ranges {
		add(r.Option, "", alice.CapRange, r.Instance, "ranges")
	}
	for _, t := range p.Toggles {
		if t.Setting == "" && t.Instance == "pause" {
			add(appliance.StatusOperationState, "включено, пока программа на паузе", alice.CapToggle, t.Instance, "toggles")
			continue
		}
		if _, ok := st.Values[t.Setting]; t.Optional && !ok {
			// Такой настройки у этого прибора нет.
			continue
		}
		add(t.Setting, valuePair(t.On, t.Off), alice.CapToggle, t.Instance, "toggles")
	}
	if c := p.Color; c != nil {
		hue := ""
		if c.HueScale() != 360 {
			hue = "0-" + formatValue(c.HueScale()) + " → 0-360"
		}
		add(c.Setting, "оттенок и насыщенность", alice.CapColor, "hsv", "color")
		add(c.Hue, hue, alice.CapColor, "hsv", "color")
		add(c.Saturation, "", alice.CapColor, "hsv", "color")
		add(c.Temperature, "", alice.CapColor, "temperature_k", "color")
	}
	for i := range p.Properties {
		pr := &p.Properties[i]
		kind, convert := alice.PropFloat, conversionLabel(pr)
		if pr.Type == "event" {
			kind, convert = alice.PropEvent, "таблица: "+count(len(pr.Values), "значение", "значения", "значений")
		}
		add(pr.Status, convert, kind, pr.Instance, "properties")
	}
	if e := p.Events; e != nil {
		for _, k := range slices.Sorted(maps.Keys(e.Button)) {
			label, note := eventLabel(k)
			out = append(out, mapRow{Key: k, Label: label, Note: note, Alice: itemLabel(alice.PropEvent, "button"),
				Value: nameOr(eventValueNames, e.Button[k]), Section: "events"})
		}
		level := func(instance string, always, ifKnown []string) {
			row := func(k string) {
				value := "низкий"
				if slices.Contains(e.Empty, k) {
					value = "пусто"
				}
				label, note := eventLabel(k)
				out = append(out, mapRow{Key: k, Label: label, Note: note, Alice: itemLabel(alice.PropEvent, instance), Value: value, Section: "events"})
			}
			for _, k := range always {
				row(k)
			}
			for _, k := range ifKnown {
				if _, ok := st.Values[k]; ok {
					row(k)
				}
			}
		}
		level("food_level", e.FoodLevel, e.FoodLevelIfKnown)
		level("water_level", e.WaterLevel, e.WaterLevelIfKnown)
	}
	return out
}

// eventLabel - событие прибора коротко и что оно значит. У перехода - имя и значение: state=END, а у
// тревоги кофемашины только её имя: значение у неё всегда одно.
func eventLabel(key string) (label, note string) {
	state, value, transition := strings.Cut(key, "=")
	if alarm, ok := strings.CutPrefix(state, delonghi.AlarmPrefix); ok {
		return alarm, alarmName(alarm)
	}
	if transition {
		return short(state) + "=" + short(value), ""
	}
	return short(key), ""
}

// shortValue - значение прибора коротко: у перечисления Home Connect - последняя часть
// имени, Run из BSH.Common.EnumType.OperationState.Run.
func shortValue(v any) string {
	if s, ok := v.(string); ok && !strings.Contains(s, " ") {
		return short(s)
	}
	return formatValue(v)
}

// valuePair - значения прибора для «включено» и «выключено»; пусто - true и false.
func valuePair(on, off string) string {
	if on == "" && off == "" {
		return ""
	}
	return "включено - " + on + ", выключено - " + off
}

// count - число со словом в нужной форме: «1 значение», «3 значения», «5 значений».
func count(n int, one, few, many string) string {
	word := many
	switch n10, n100 := n%10, n%100; {
	case n10 == 1 && n100 != 11:
		word = one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		word = few
	}
	return strconv.Itoa(n) + " " + word
}
