package web

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/profile"
)

// Редактор профиля: каждое поле профиля - свой раздел со своей формой. Сохранённое поле
// заменяет унаследованное целиком, как в настройках (раздел «Профили» описания).

// profileFields - поля профиля в порядке разделов.
var profileFields = []struct{ name, title string }{
	{"yandex_type", "Тип устройства в Алисе"},
	{"on_off", "Что делает «включи»"},
	{"default_program", "Программа по умолчанию"},
	{"default_options", "Параметры программы при запуске"},
	{"modes", "Режимы"},
	{"ranges", "Числовые параметры"},
	{"toggles", "Переключатели"},
	{"color", "Цвет"},
	{"properties", "Свойства"},
	{"events", "Датчик событий"},
}

const (
	// sourcePrograms - режим выбирает программу, а не значение параметра.
	sourcePrograms = "programs"
	// customKind - своё свойство в поле вида: instance вписывается отдельно, например float/*.
	customKind = "*"
	// convertCustom - своя формула пересчёта: множитель и слагаемое из формы.
	convertCustom = "custom"
	// maxValue - предел длины одного поля формы.
	maxValue = 1024
	// blankRows - сколько пустых строк для новых значений показывает редактор.
	blankRows = 2
)

var onOffLabels = map[string]string{"program": "Запуск и остановка программы", "power": "Питание", "none": "Нет умения"}

func knownField(name string) bool {
	return slices.ContainsFunc(profileFields, func(f struct{ name, title string }) bool { return f.name == name })
}

// editTarget - чьё переопределение правит редактор: прибора или типа.
type editTarget struct {
	// action - адрес форм без имени поля.
	action  string
	builtin *profile.Profile
	forType *profile.Profile
	// forDevice и device - переопределение прибора, если редактор на странице прибора.
	forDevice *profile.Profile
	device    bool
	// desc - программы и параметры прибора или всех приборов типа.
	desc appliance.Description
	// values - имена состояний и событий, о которых сообщили приборы.
	values []string
	// now и program - значения прибора сейчас и его программа: только на странице прибора.
	now     map[string]any
	program string
	// lg - прибор или тип без программ: LG, SmartThings или телевизор. Подсказки - его свойства
	// и уведомления.
	lg bool
	// probe - адрес кнопки «Проверить» у режима; пусто - на странице типа, кнопки нет.
	probe string
}

// hint - подсказка к функции прибора: значение сейчас, пределы или значения из описания.
func (t *editTarget) hint(key string) string {
	var parts []string
	if v, ok := t.now[key]; ok {
		parts = append(parts, "сейчас "+formatValue(v))
	}
	if o, ok := t.desc.Options[key]; ok {
		switch {
		case len(o.Values) > 0:
			parts = append(parts, "значения: "+shortList(o.Values))
		case o.Max > o.Min:
			parts = append(parts, "от "+formatValue(o.Min)+" до "+formatValue(o.Max))
		}
	}
	return strings.Join(parts, " · ")
}

// shortList - значения перечисления коротко: последние части имён, не больше пяти.
func shortList(values []string) string {
	var out []string
	for i, v := range values {
		if i == 5 {
			out = append(out, "…")
			break
		}
		out = append(out, short(v))
	}
	return strings.Join(out, ", ")
}

// keyChoice - имя функции прибора в подсказках поля: Label - подсказка (hint), Now - число
// сейчас для пересчёта в скрипте.
type keyChoice struct {
	Value, Label, Now string
}

func (t *editTarget) keyChoices(list []string) []keyChoice {
	out := make([]keyChoice, 0, len(list))
	for _, k := range list {
		c := keyChoice{Value: k, Label: t.hint(k)}
		if f, ok := t.now[k].(float64); ok {
			c.Now = formatValue(f)
		}
		out = append(out, c)
	}
	return out
}

// valueKind - вид значений функции прибора: number, bool, list или пусто - неизвестно.
func (t *editTarget) valueKind(key string) string {
	o, known := t.desc.Options[key]
	if known && len(o.Values) > 0 {
		return "list"
	}
	switch t.now[key].(type) {
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "list"
	}
	if known && o.Max > o.Min {
		return "number"
	}
	return ""
}

// numberFit - предупреждение, если числу Алисы досталась функция не с числами; target -
// умение Алисы словами.
func (t *editTarget) numberFit(key, target string) string {
	switch t.valueKind(key) {
	case "bool":
		return "у функции значения да/нет: " + target + " она не подходит"
	case "list":
		return "у функции значения из списка: " + target + " она не подходит"
	}
	return ""
}

// toggleFit - предупреждение, если переключателю досталась функция, которой нельзя задать
// «включено» и «выключено» без значений прибора.
func (t *editTarget) toggleFit(tg *profile.Toggle) string {
	switch t.valueKind(tg.Setting) {
	case "number":
		return "у функции число: переключателю нужны два значения"
	case "list":
		if tg.On == "" || tg.Off == "" {
			return "у функции значения из списка: задайте значения «включено» и «выключено»"
		}
	}
	return ""
}

// users - какие умения Алисы меняют каждую функцию прибора: две записи в одну функцию
// перебивают друг друга.
func users(p *profile.Profile) map[string][]string {
	out := map[string][]string{}
	add := func(key, label string) {
		if key != "" && !slices.Contains(out[key], label) {
			out[key] = append(out[key], label)
		}
	}
	if p.Power != nil {
		add(p.Power.Setting, itemLabel(alice.CapOnOff, "on"))
	}
	for _, m := range p.Modes {
		add(m.Option, itemLabel(alice.CapMode, m.Instance))
	}
	for _, r := range p.Ranges {
		add(r.Option, itemLabel(alice.CapRange, r.Instance))
	}
	for _, tg := range p.Toggles {
		add(tg.Setting, itemLabel(alice.CapToggle, tg.Instance))
	}
	if c := p.Color; c != nil {
		add(c.Setting, itemLabel(alice.CapColor, "hsv"))
		add(c.Temperature, itemLabel(alice.CapColor, "temperature_k"))
	}
	return out
}

// taken - предупреждение, если функцию key меняет и другое умение, кроме self.
func taken(users map[string][]string, key, self string) string {
	var others []string
	for _, u := range users[key] {
		if u != self {
			others = append(others, "«"+u+"»")
		}
	}
	if len(others) == 0 {
		return ""
	}
	return "эту функцию уже меняет " + strings.Join(others, ", ")
}

// joinWarn - предупреждения через «; », без пустых.
func joinWarn(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "; ")
}

// override - переопределение, которое правит редактор.
func (t *editTarget) override() *profile.Profile {
	if t.device {
		return t.forDevice
	}
	return t.forType
}

// base - профиль, который наследует правка: на странице прибора - встроенный с профилем типа.
func (t *editTarget) base() *profile.Profile {
	if t.device {
		return profile.Merge(t.builtin, t.forType)
	}
	return t.builtin
}

func (t *editTarget) origin(field string) string {
	if o := t.overrideOrigin(field); o != "" {
		return o
	}
	if has(t.builtin, field) {
		return "встроенный профиль"
	}
	return "не задано"
}

// overrideOrigin - откуда поле, если оно переопределено для прибора или в профиле типа; иначе
// пусто. Редактор подписывает только такие разделы: пометка «встроенный профиль» стояла бы у
// каждого.
func (t *editTarget) overrideOrigin(field string) string {
	switch {
	case t.device && has(t.forDevice, field):
		return "задано для прибора"
	case has(t.forType, field):
		return "профиль типа"
	}
	return ""
}

func (t *editTarget) resetLabel(field string) string {
	if t.device && has(t.forType, field) {
		return "Вернуть значение типа"
	}
	return "Вернуть встроенное"
}

func has(p *profile.Profile, field string) bool {
	if p == nil {
		return false
	}
	switch field {
	case "yandex_type":
		return p.YandexType != ""
	case "on_off":
		return p.OnOff != "" || p.Power != nil
	case "default_program":
		return p.DefaultProgram != ""
	case "default_options":
		return p.DefaultOptions != nil
	case "modes":
		return p.Modes != nil
	case "ranges":
		return p.Ranges != nil
	case "toggles":
		return p.Toggles != nil
	case "color":
		return p.Color != nil
	case "properties":
		return p.Properties != nil
	case "events":
		return p.Events != nil
	}
	return false
}

// setField - копия o, где поле field взято из src. src == nil убирает переопределение поля.
// Пустое переопределение - nil.
func setField(o *profile.Profile, field string, src *profile.Profile) *profile.Profile {
	var n profile.Profile
	if o != nil {
		n = *o
	}
	if src == nil {
		src = &profile.Profile{}
	}
	switch field {
	case "yandex_type":
		n.YandexType = src.YandexType
	case "on_off":
		n.OnOff, n.Power = src.OnOff, src.Power
	case "default_program":
		n.DefaultProgram = src.DefaultProgram
	case "default_options":
		n.DefaultOptions = src.DefaultOptions
	case "modes":
		n.Modes = src.Modes
	case "ranges":
		n.Ranges = src.Ranges
	case "toggles":
		n.Toggles = src.Toggles
	case "color":
		n.Color = src.Color
	case "properties":
		n.Properties = src.Properties
	case "events":
		n.Events = src.Events
	}
	if reflect.DeepEqual(n, profile.Profile{}) {
		return nil
	}
	return &n
}

// sameField сообщает, что поле field в a и b одно и то же: пустой список равен незаданному,
// а on_off none - незаданному on_off.
func sameField(a, b *profile.Profile, field string) bool {
	norm := func(p *profile.Profile) profile.Profile {
		var n profile.Profile
		if x := setField(nil, field, p); x != nil {
			n = *x
		}
		if n.OnOff == "none" {
			n.OnOff = ""
		}
		if len(n.DefaultOptions) == 0 {
			n.DefaultOptions = nil
		}
		if len(n.Modes) == 0 {
			n.Modes = nil
		}
		if len(n.Ranges) == 0 {
			n.Ranges = nil
		}
		if len(n.Toggles) == 0 {
			n.Toggles = nil
		}
		if len(n.Properties) == 0 {
			n.Properties = nil
		}
		if n.Events != nil && len(n.Events.Button) == 0 && len(n.Events.FoodLevel) == 0 && len(n.Events.WaterLevel) == 0 {
			n.Events = nil
		}
		return n
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// applyField - новое переопределение после правки поля field: значение из формы
// или, при reset, унаследованное. Значение, равное унаследованному, не сохраняется.
func applyField(t *editTarget, field string, patch *profile.Profile, reset bool) *profile.Profile {
	if reset || sameField(patch, t.base(), field) {
		patch = nil
	}
	return setField(t.override(), field, patch)
}

// fk - имя поля формы из частей: fk("m", 0, "k", 3) = "m.0.k.3".
func fk(parts ...any) string {
	s := make([]string, len(parts))
	for i, p := range parts {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ".")
}

// parseField разбирает форму раздела field в профиль, где задано только это поле.
func parseField(field string, form url.Values) (*profile.Profile, error) {
	for _, vs := range form {
		for _, v := range vs {
			if len(v) > maxValue {
				return nil, invalid("значение длиннее %d байт", maxValue)
			}
		}
	}
	get := func(name string) string { return strings.TrimSpace(form.Get(name)) }
	p := &profile.Profile{}
	switch field {
	case "yandex_type":
		p.YandexType = get("yandex_type")
	case "on_off":
		p.OnOff = get("on_off")
		if setting := get("power_setting"); setting != "" {
			p.Power = &profile.Power{Setting: setting, On: get("power_on"), Off: get("power_off"),
				State: get("power_state"), Allowed: get("power_allowed")}
			for _, v := range strings.Split(get("power_idle"), ",") {
				if v = strings.TrimSpace(v); v != "" {
					p.Power.Idle = append(p.Power.Idle, v)
				}
			}
		}
	case "default_program":
		p.DefaultProgram = get("default_program")
	case "default_options":
		p.DefaultOptions = map[string]any{}
		for i := 0; form.Has(fk("o", i, "k")); i++ {
			if k, raw := get(fk("o", i, "k")), get(fk("o", i, "v")); k != "" && raw != "" {
				p.DefaultOptions[k] = optionValue(raw)
			}
		}
	case "modes":
		p.Modes = []profile.Mode{}
		for i := 0; form.Has(fk("m", i, "source")); i++ {
			if form.Get(fk("m", i, "delete")) != "" {
				continue
			}
			mapping := map[string]string{}
			for j := 0; form.Has(fk("m", i, "k", j)); j++ {
				if k, v := get(fk("m", i, "k", j)), get(fk("m", i, "v", j)); k != "" && v != "" {
					mapping[k] = v
				}
			}
			p.Modes = append(p.Modes, newMode(get(fk("m", i, "instance")), get(fk("m", i, "source")), mapping))
		}
		if instance := get("m.new.instance"); instance != "" {
			p.Modes = append(p.Modes, newMode(instance, get("m.new.source"), map[string]string{}))
		}
	case "ranges":
		p.Ranges = []profile.Range{}
		for i := 0; form.Has(fk("r", i, "instance")); i++ {
			instance := get(fk("r", i, "instance"))
			if instance == "" || form.Get(fk("r", i, "delete")) != "" {
				continue
			}
			r := profile.Range{Instance: instance, Option: get(fk("r", i, "option")), Unit: get(fk("r", i, "unit"))}
			for _, f := range []struct {
				name string
				dst  **float64
			}{{"min", &r.Min}, {"max", &r.Max}, {"precision", &r.Precision}} {
				raw := get(fk("r", i, f.name))
				if raw == "" {
					continue
				}
				v, err := strconv.ParseFloat(raw, 64)
				if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
					return nil, invalid("ranges %s: %s %q - не число", instance, f.name, raw)
				}
				*f.dst = &v
			}
			p.Ranges = append(p.Ranges, r)
		}
	case "toggles":
		p.Toggles = []profile.Toggle{}
		for i := 0; form.Has(fk("t", i, "instance")); i++ {
			if instance := get(fk("t", i, "instance")); instance != "" && form.Get(fk("t", i, "delete")) == "" {
				p.Toggles = append(p.Toggles, profile.Toggle{Instance: instance, Setting: get(fk("t", i, "setting")),
					On: get(fk("t", i, "on")), Off: get(fk("t", i, "off"))})
			}
		}
	case "color":
		c := &profile.Color{Setting: get("color_setting"), Hue: get("color_hue"), Saturation: get("color_saturation"),
			Temperature: get("color_temperature")}
		if raw := strings.Replace(get("color_hue_max"), ",", ".", 1); raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, invalid("color: верх шкалы оттенка %q - не число", raw)
			}
			c.HueMax = &v
		}
		for _, f := range []struct {
			name string
			dst  **int
		}{{"color_min_k", &c.MinK}, {"color_max_k", &c.MaxK}} {
			if raw := get(f.name); raw != "" {
				v, err := strconv.Atoi(raw)
				if err != nil {
					return nil, invalid("color: температура %q - не целое число кельвинов", raw)
				}
				*f.dst = &v
			}
		}
		p.Color = c
	case "properties":
		p.Properties = []profile.Property{}
		for i := 0; form.Has(fk("p", i, "kind")); i++ {
			typ, instance, _ := strings.Cut(get(fk("p", i, "kind")), "/")
			if typ == "" || form.Get(fk("p", i, "delete")) != "" {
				continue
			}
			if instance == customKind {
				instance = get(fk("p", i, "instance"))
			}
			pr := profile.Property{Type: typ, Instance: instance, Status: get(fk("p", i, "status"))}
			if typ == "float" {
				pr.Unit = get(fk("p", i, "unit"))
				switch c := get(fk("p", i, "convert")); c {
				case "":
				case convertCustom:
					for _, f := range []struct {
						name, label string
						dst         **float64
					}{{"scale", "множитель", &pr.Scale}, {"offset", "слагаемое", &pr.Offset}} {
						raw := strings.Replace(get(fk("p", i, f.name)), ",", ".", 1)
						if raw == "" {
							continue
						}
						v, err := strconv.ParseFloat(raw, 64)
						if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
							return nil, invalid("properties %s: %s %q - не число", instance, f.label, raw)
						}
						*f.dst = &v
					}
				default:
					pr.Convert = c
				}
			} else {
				pr.Values = map[string]string{}
				for j := 0; form.Has(fk("p", i, "k", j)); j++ {
					if k, v := get(fk("p", i, "k", j)), get(fk("p", i, "v", j)); k != "" && v != "" {
						pr.Values[k] = v
					}
				}
			}
			p.Properties = append(p.Properties, pr)
		}
	case "events":
		p.Events = &profile.Events{}
		button := map[string]string{}
		for j := 0; form.Has(fk("b", j, "k")); j++ {
			if k, v := get(fk("b", j, "k")), get(fk("b", j, "v")); k != "" && v != "" {
				button[k] = v
			}
		}
		if len(button) > 0 {
			p.Events.Button = button
		}
		for j := 0; form.Has(fk("f", j)); j++ {
			if v := get(fk("f", j)); v != "" && !slices.Contains(p.Events.FoodLevel, v) {
				p.Events.FoodLevel = append(p.Events.FoodLevel, v)
			}
		}
		for j := 0; form.Has(fk("w", j)); j++ {
			if v := get(fk("w", j)); v != "" && !slices.Contains(p.Events.WaterLevel, v) {
				p.Events.WaterLevel = append(p.Events.WaterLevel, v)
			}
		}
	}
	return p, nil
}

func newMode(instance, source string, mapping map[string]string) profile.Mode {
	if source == sourcePrograms || source == "" {
		return profile.Mode{Instance: instance, Programs: mapping}
	}
	return profile.Mode{Instance: instance, Option: source, Values: mapping}
}

// optionValue - значение параметра из поля формы: число, флаг или имя значения перечисления.
func optionValue(raw string) any {
	if f, err := strconv.ParseFloat(strings.Replace(raw, ",", ".", 1), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f
	}
	switch raw {
	case "true":
		return true
	case "false":
		return false
	}
	return raw
}

// formatValue - значение для страницы: число без лишних нулей, флаг, имя значения перечисления.
func formatValue(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	case map[string]any, []any:
		// Составное значение прибора: JSON читается лучше, чем вид по умолчанию.
		if data, err := json.Marshal(x); err == nil {
			return string(data)
		}
	}
	return fmt.Sprint(v)
}

// Представление редактора для шаблона.

type choice struct {
	Value, Label string
	Selected     bool
}

// choices - варианты выбора: list и текущее значение, если его нет в списке.
// blank - подпись пустого варианта; пустая подпись - пустого варианта нет.
func choices(list []string, cur, blank string, labels map[string]string) []choice {
	var out []choice
	if blank != "" {
		out = append(out, choice{Value: "", Label: blank, Selected: cur == ""})
	}
	if cur != "" && !slices.Contains(list, cur) {
		list = append([]string{cur}, list...)
	}
	for _, v := range list {
		label := v
		if l, ok := labels[v]; ok {
			label = v + " - " + l
		}
		out = append(out, choice{Value: v, Label: label, Selected: v == cur})
	}
	return out
}

type sectionView struct {
	Title, Action, Origin, ResetLabel, Error string
	Overridden                               bool
	// Preview - кнопка «Предпросмотр»: только на странице прибора.
	Preview bool
}

// row - строка сопоставления: имя функции прибора и значение Алисы.
type row struct {
	Index int
	Key   string
	// Missing - прибор не сообщал о таком имени; Now - у прибора сейчас это значение или эта
	// программа; Hint - подсказка к функции (editTarget.hint).
	Missing, Now bool
	Hint         string
	// Probe - режим Алисы для кнопки «Проверить»; пусто - кнопки нет.
	Probe string
	// Label - имя функции прибора коротко; полное - в подсказке.
	Label   string
	Value   string
	Choices []choice
}

type modeView struct {
	Index       int
	Instance    string
	Instances   []choice
	Source      string
	SourceLabel string
	Rows        []row
}

// rangeView - число Алисы. Hint - подсказка к параметру прибора, Warn - предупреждение.
type rangeView struct {
	Index                       int
	Instances, Units            []choice
	Option, Min, Max, Precision string
	Hint, Warn                  string
	New                         bool
}

type toggleView struct {
	Index      int
	Instances  []choice
	Setting    string
	On, Off    string
	Hint, Warn string
	New        bool
}

// powerView - питание через свойство прибора; Idle - значения через запятую. Подсказки - к
// свойству, состоянию и флагу разрешения, Warn - к свойству.
type powerView struct {
	Setting, On, Off, State, Idle, Allowed string
	SettingHint, StateHint, AllowedHint    string
	Warn                                   string
}

type propertyView struct {
	Index int
	Kinds []choice
	// Instance - instance своего свойства, которого нет в списках Яндекса.
	Instance string
	Status   string
	Unit     string
	// Convert - пересчёт числа; Scale и Offset - своя формула.
	Convert       []choice
	Scale, Offset string
	// Events - значения Алисы для подсказок у события.
	Events []choice
	// Warn - чего из свойства нет в документации Яндекса; Fit - функция прибора не подходит
	// показанию.
	Warn, Fit string
	// Hint - подсказка к состоянию прибора. Result - пересчёт значения сейчас словами, Raw -
	// само значение для пересчёта в скрипте; пусто - числа сейчас нет.
	Hint, Result, Raw string
	Rows              []row
	New               bool
}

type editorView struct {
	CSRF, Version string
	Sections      map[string]*sectionView
	// Списки для подсказок в полях имён функций прибора.
	Programs, NumericOptions, Settings, Statuses, Events []keyChoice

	YandexTypes, OnOff, DefaultProgram []choice
	// ProbeAction - адрес кнопки «Проверить» у режимов; пусто - кнопок нет.
	ProbeAction string
	// Power - поля питания; nil - их не показывать. LG - редактор прибора или типа LG,
	// HomeConnect - Home Connect: у него питание без свойства - PowerState.
	Power                            *powerView
	LG, HomeConnect                  bool
	Options                          []row
	Modes                            []modeView
	NewModeInstances, NewModeSources []choice
	Ranges                           []rangeView
	Toggles                          []toggleView
	Color                            colorView
	Properties                       []propertyView
	// Units - единицы Яндекса для подсказок у свойства.
	Units      []choice
	Buttons    []row
	FoodLevel  []row
	WaterLevel []row
}

// buildEditor собирает редактор. draft - введённое переопределение, которое не сохранилось
// из-за ошибки errText в разделе section: редактор показывает его, а не сохранённое.
func buildEditor(t *editTarget, csrf, version string, draft *profile.Profile, section, errText string) *editorView {
	override := t.override()
	if draft != nil {
		override = draft
	}
	eff := profile.Merge(t.base(), override)
	v := &editorView{CSRF: csrf, Version: version, Sections: map[string]*sectionView{}, LG: t.lg, ProbeAction: t.probe,
		HomeConnect: !t.lg && t.desc.Type != profile.DeLonghiType}
	for _, f := range profileFields {
		if t.lg && (f.name == "default_program" || f.name == "default_options") {
			// Программ ThinQ Connect не запускает.
			continue
		}
		sv := &sectionView{Title: f.title, Action: t.action + "/" + f.name, Origin: t.overrideOrigin(f.name), Preview: t.device,
			Overridden: has(t.override(), f.name), ResetLabel: t.resetLabel(f.name)}
		if f.name == section {
			sv.Error = errText
		}
		v.Sections[f.name] = sv
	}
	desc := &t.desc

	// Подсказки: что сообщили приборы и что уже есть в профиле.
	programs := slices.Clone(desc.Programs)
	if eff.DefaultProgram != "" {
		programs = append(programs, eff.DefaultProgram)
	}
	var numeric, settings, enums []string
	for k, o := range desc.Options {
		if strings.Contains(k, ".Setting.") || o.Setting {
			settings = append(settings, k)
		}
		if len(o.Values) > 0 {
			enums = append(enums, k)
		} else {
			numeric = append(numeric, k)
		}
	}
	var statuses, events []string
	for _, k := range t.values {
		switch {
		case t.lg, strings.Contains(k, ".Status."):
			statuses = append(statuses, k)
		case strings.Contains(k, ".Event."):
			events = append(events, k)
		}
	}
	if t.lg {
		// У прибора без программ состояние - любое его свойство, а события - уведомления.
		statuses = append(statuses, slices.Collect(maps.Keys(desc.Options))...)
		statuses = append(statuses, desc.Reports...)
	}
	events = append(events, desc.Events...)
	for _, m := range eff.Modes {
		programs = append(programs, slices.Collect(maps.Keys(m.Programs))...)
	}
	for _, r := range eff.Ranges {
		numeric = append(numeric, r.Option)
	}
	for _, tg := range eff.Toggles {
		if tg.Setting != "" {
			settings = append(settings, tg.Setting)
		}
	}
	for _, pr := range eff.Properties {
		statuses = append(statuses, pr.Status)
	}
	if eff.Events != nil {
		events = append(events, slices.Collect(maps.Keys(eff.Events.Button))...)
		events = append(events, eff.Events.FoodLevel...)
		events = append(events, eff.Events.WaterLevel...)
	}
	v.Programs, v.NumericOptions, v.Settings = t.keyChoices(uniq(programs)), t.keyChoices(uniq(numeric)), t.keyChoices(uniq(settings))
	v.Statuses, v.Events = t.keyChoices(uniq(statuses)), t.keyChoices(uniq(events))
	for i, c := range v.Programs {
		if c.Value == t.program {
			v.Programs[i].Label = "идёт сейчас"
		}
	}
	slices.Sort(enums)
	us := users(eff)

	v.YandexTypes = named(profile.YandexTypes, eff.YandexType, blankIf(eff.YandexType == "", "не задан"), yandexTypeNames)
	onOff := eff.OnOff
	if onOff == "" {
		onOff = "none"
	}
	v.OnOff = named(profile.OnOffKinds, onOff, "", onOffLabels)
	if t.lg || eff.Power != nil {
		v.Power = &powerView{}
		if pw := eff.Power; pw != nil {
			v.Power = &powerView{Setting: pw.Setting, On: pw.On, Off: pw.Off, State: pw.State,
				Idle: strings.Join(pw.Idle, ", "), Allowed: pw.Allowed, SettingHint: t.hint(pw.Setting),
				StateHint: t.hint(pw.State), AllowedHint: t.hint(pw.Allowed),
				Warn: taken(us, pw.Setting, itemLabel(alice.CapOnOff, "on"))}
		}
	}
	v.DefaultProgram = choices(uniq(desc.Programs), eff.DefaultProgram, blankIf(eff.DefaultProgram == "", "не задана"), nil)
	for i, c := range v.DefaultProgram {
		if c.Value != "" {
			v.DefaultProgram[i].Label = programName(c.Value, eff)
		}
	}

	// Параметры при запуске: параметры программ прибора и уже заданные.
	var optKeys []string
	for k := range desc.Options {
		if !strings.Contains(k, ".Setting.") {
			optKeys = append(optKeys, k)
		}
	}
	optKeys = append(optKeys, slices.Collect(maps.Keys(eff.DefaultOptions))...)
	for i, k := range uniq(optKeys) {
		o, known := desc.Options[k]
		r := row{Index: i, Key: k, Missing: !known && len(desc.Options) > 0, Value: formatValue(eff.DefaultOptions[k]), Hint: t.hint(k)}
		if len(o.Values) > 0 {
			r.Choices = choices(o.Values, r.Value, "не задан", nil)
		}
		v.Options = append(v.Options, r)
	}
	v.Options = append(v.Options, blank(len(v.Options), 1)...)

	modeValues := func(cur string) []choice { return named(profile.ModeValues, cur, "-", modeValueNames) }
	for i, m := range eff.Modes {
		mv := modeView{Index: i, Instance: m.Instance, Instances: named(profile.ModeInstances, m.Instance, "", modeInstanceNames)}
		var keys, known []string
		mapping := m.Values
		current := ""
		if m.Programs != nil {
			mv.Source, mv.SourceLabel = sourcePrograms, "программы прибора"
			known, mapping, current = desc.Programs, m.Programs, t.program
		} else {
			mv.Source, mv.SourceLabel = m.Option, "значения "+m.Option
			known = desc.Options[m.Option].Values
			if v, ok := t.now[m.Option]; ok {
				current = formatValue(v)
			}
		}
		keys = uniq(append(slices.Clone(known), slices.Collect(maps.Keys(mapping))...))
		for j, k := range keys {
			label := short(k)
			if m.Programs != nil {
				label = programShort(k)
			}
			mv.Rows = append(mv.Rows, row{Index: j, Key: k, Label: label, Missing: len(known) > 0 && !slices.Contains(known, k),
				Now: current != "" && k == current, Value: mapping[k], Choices: modeValues(mapping[k]), Probe: mapping[k]})
		}
		for _, b := range blank(len(mv.Rows), blankRows) {
			b.Choices = modeValues("")
			mv.Rows = append(mv.Rows, b)
		}
		v.Modes = append(v.Modes, mv)
	}
	v.NewModeInstances = named(profile.ModeInstances, "", "не добавлять", modeInstanceNames)
	v.NewModeSources = append([]choice{{Value: sourcePrograms, Label: "программы прибора", Selected: true}},
		choices(enums, "", "", nil)...)

	rangeUnits := uniq(slices.Concat(slices.Collect(maps.Values(profile.RangeUnits))...))
	rangeInstances := slices.Sorted(maps.Keys(profile.RangeUnits))
	for i, r := range append(slices.Clone(eff.Ranges), profile.Range{}) {
		v.Ranges = append(v.Ranges, rangeView{Index: i, New: i == len(eff.Ranges),
			Instances: named(rangeInstances, r.Instance, blankIf(r.Instance == "", "-"), rangeInstanceNames),
			Units:     named(rangeUnits, r.Unit, "без единицы", unitNames),
			Option:    r.Option, Min: number(r.Min), Max: number(r.Max), Precision: number(r.Precision), Hint: t.hint(r.Option),
			Warn: joinWarn(t.numberFit(r.Option, "числу Алисы"), taken(us, r.Option, itemLabel(alice.CapRange, r.Instance)))})
	}
	for i, tg := range append(slices.Clone(eff.Toggles), profile.Toggle{}) {
		v.Toggles = append(v.Toggles, toggleView{Index: i, New: i == len(eff.Toggles), Setting: tg.Setting, On: tg.On, Off: tg.Off,
			Instances: named(profile.ToggleInstances, tg.Instance, blankIf(tg.Instance == "", "-"), toggleInstanceNames),
			Hint:      t.hint(tg.Setting), Warn: joinWarn(t.toggleFit(&tg), taken(us, tg.Setting, itemLabel(alice.CapToggle, tg.Instance)))})
	}
	if c := eff.Color; c != nil {
		v.Color = colorView{Setting: c.Setting, Hue: c.Hue, Saturation: c.Saturation, HueMax: number(c.HueMax),
			Temperature: c.Temperature, MinK: intText(c.MinK), MaxK: intText(c.MaxK), SettingHint: t.hint(c.Setting),
			HueHint: t.hint(c.Hue), SaturationHint: t.hint(c.Saturation), TemperatureHint: t.hint(c.Temperature)}
	}

	var kinds []string
	kindLabels := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(profile.EventValues)) {
		kinds = append(kinds, "event/"+k)
		kindLabels["event/"+k] = itemLabel(alice.PropEvent, k)
	}
	for _, k := range slices.Sorted(maps.Keys(profile.FloatUnits)) {
		kinds = append(kinds, "float/"+k)
		kindLabels["float/"+k] = itemLabel(alice.PropFloat, k)
	}
	allUnits := uniq(slices.Concat(slices.Concat(slices.Collect(maps.Values(profile.FloatUnits))...),
		slices.Concat(slices.Collect(maps.Values(profile.RangeUnits))...)))
	for _, u := range allUnits {
		v.Units = append(v.Units, choice{Value: u, Label: unitNames[u]})
	}
	allEvents := uniq(slices.Concat(slices.Collect(maps.Values(profile.EventValues))...))
	for i, pr := range append(slices.Clone(eff.Properties), profile.Property{}) {
		pv := propertyView{Index: i, New: i == len(eff.Properties), Status: pr.Status, Unit: pr.Unit, Warn: propertyWarning(&pr),
			Hint: t.hint(pr.Status)}
		if pr.Type == "float" {
			pv.Fit = t.numberFit(pr.Status, "показанию Алисы")
			if f, ok := t.now[pr.Status].(float64); ok {
				pv.Raw, pv.Result = formatValue(f), "сейчас "+formatValue(f)+" → "+withUnit(pr.Value(f), pr.Unit)
			}
		}
		kind := ""
		if pr.Type != "" {
			kind = pr.Type + "/" + pr.Instance
			if !slices.Contains(kinds, kind) {
				kind, pv.Instance = pr.Type+"/"+customKind, pr.Instance
			}
		}
		pv.Kinds = append(named(kinds, kind, blankIf(kind == "", "-"), kindLabels),
			choice{Value: "float/" + customKind, Label: "Своё число (float)", Selected: kind == "float/"+customKind},
			choice{Value: "event/" + customKind, Label: "Своё событие (event)", Selected: kind == "event/"+customKind})
		convert := pr.Convert
		if pr.Scale != nil || pr.Offset != nil {
			convert, pv.Scale, pv.Offset = convertCustom, number(pr.Scale), number(pr.Offset)
		}
		pv.Convert = append([]choice{{Value: "", Label: "без пересчёта", Selected: convert == ""}}, conversionChoices(convert)...)
		events := allEvents
		if list, ok := profile.EventValues[pr.Instance]; ok && pr.Type == "event" {
			events = list
		}
		for _, e := range events {
			pv.Events = append(pv.Events, choice{Value: e, Label: eventValueNames[e]})
		}
		for j, k := range slices.Sorted(maps.Keys(pr.Values)) {
			pv.Rows = append(pv.Rows, row{Index: j, Key: k, Value: pr.Values[k]})
		}
		pv.Rows = append(pv.Rows, blank(len(pv.Rows), blankRows)...)
		v.Properties = append(v.Properties, pv)
	}

	var button map[string]string
	var food, water []string
	if eff.Events != nil {
		button, food, water = eff.Events.Button, eff.Events.FoodLevel, eff.Events.WaterLevel
	}
	for j, k := range slices.Sorted(maps.Keys(button)) {
		v.Buttons = append(v.Buttons, row{Index: j, Key: k, Value: button[k], Choices: named(profile.ButtonValues, button[k], "-", eventValueNames)})
	}
	for _, b := range blank(len(v.Buttons), blankRows) {
		b.Choices = named(profile.ButtonValues, "", "-", eventValueNames)
		v.Buttons = append(v.Buttons, b)
	}
	for j, k := range food {
		v.FoodLevel = append(v.FoodLevel, row{Index: j, Key: k})
	}
	v.FoodLevel = append(v.FoodLevel, blank(len(v.FoodLevel), blankRows)...)
	for j, k := range water {
		v.WaterLevel = append(v.WaterLevel, row{Index: j, Key: k})
	}
	v.WaterLevel = append(v.WaterLevel, blank(len(v.WaterLevel), blankRows)...)
	return v
}

// conversionLabels - готовые пересчёты по-русски, в порядке списка.
var conversionLabels = []kv{
	{"minutes_to_seconds", "минуты → секунды, ×60"},
	{"hours_to_seconds", "часы → секунды, ×3600"},
	{"fahrenheit_to_celsius", "°F → °C"},
	{"fraction_to_percent", "доля → проценты, ×100"},
	{convertCustom, "своя формула"},
}

func conversionChoices(cur string) []choice {
	var out []choice
	for _, c := range conversionLabels {
		out = append(out, choice{Value: c.Key, Label: c.Value, Selected: c.Key == cur})
	}
	return out
}

// conversionLabel - пересчёт свойства для страницы; пусто - без пересчёта.
func conversionLabel(pr *profile.Property) string {
	if pr.Convert != "" {
		for _, c := range conversionLabels {
			if c.Key == pr.Convert {
				return c.Value
			}
		}
		return pr.Convert
	}
	if pr.Scale == nil && pr.Offset == nil {
		return ""
	}
	f := pr.Formula()
	return "× " + formatValue(f.Scale) + " + " + formatValue(f.Offset)
}

// propertyWarning - чего из свойства нет в документации Яндекса: такое свойство Яндекс может
// не принять. Пусто - всё из документации.
func propertyWarning(pr *profile.Property) string {
	var odd []string
	_, isFloat := profile.FloatUnits[pr.Instance]
	_, isEvent := profile.EventValues[pr.Instance]
	known := pr.Type == "float" && isFloat || pr.Type == "event" && isEvent
	if pr.Instance != "" && (!known || slices.Contains(profile.Undocumented, pr.Instance)) {
		odd = append(odd, "instance "+pr.Instance)
	}
	if _, ok := unitNames[pr.Unit]; pr.Unit != "" && (!ok || slices.Contains(profile.Undocumented, pr.Unit)) {
		odd = append(odd, "единицы "+pr.Unit)
	}
	if len(odd) == 0 {
		return ""
	}
	return "В документации Яндекса нет " + strings.Join(odd, " и ") + ": Яндекс может не принять свойство."
}

// blank - n пустых строк с номерами от from.
func blank(from, n int) []row {
	out := make([]row, n)
	for i := range out {
		out[i].Index = from + i
	}
	return out
}

func blankIf(cond bool, label string) string {
	if cond {
		return label
	}
	return ""
}

func number(f *float64) string {
	if f == nil {
		return ""
	}
	return formatValue(*f)
}

// uniq - отсортированный список без повторов и пустых строк.
func uniq(list []string) []string {
	out := slices.DeleteFunc(slices.Clone(list), func(s string) bool { return s == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

// colorView - поля раздела «Цвет» и подсказки к функциям прибора.
type colorView struct {
	Setting, Hue, Saturation, HueMax, Temperature, MinK, MaxK string
	SettingHint, HueHint, SaturationHint, TemperatureHint     string
}

func intText(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}
