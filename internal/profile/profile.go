// Package profile описывает, как прибор Home Connect или LG выглядит в Алисе.
// Для каждого типа прибора в шлюз встроен профиль по умолчанию,
// а в настройках любую его часть можно заменить для типа или для отдельного прибора.
package profile

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile - устройство Алисы для прибора. Пустое поле переопределения
// оставляет значение из профиля по умолчанию, заданное - заменяет его целиком.
type Profile struct {
	// YandexType - тип устройства без префикса devices.types., например cooking.
	YandexType string `yaml:"yandex_type"`
	// OnOff - что делает «включи»: program - запуск и остановка программы,
	// power - питание прибора, none - умения on_off нет.
	OnOff string `yaml:"on_off"`
	// Power - питание через свойство прибора, например у приборов LG. Без него
	// on_off power меняет BSH.Common.Setting.PowerState.
	Power *Power `yaml:"power"`
	// DefaultProgram запускается по «включи», если программа не выбрана.
	DefaultProgram string `yaml:"default_program"`
	// DefaultOptions - параметры программы при запуске, если их не задали голосом.
	DefaultOptions map[string]any `yaml:"default_options"`
	Modes          []Mode         `yaml:"modes"`
	Ranges         []Range        `yaml:"ranges"`
	Toggles        []Toggle       `yaml:"toggles"`
	// Color - умение color_setting: цвет и цветовая температура лампы.
	Color      *Color     `yaml:"color"`
	Properties []Property `yaml:"properties"`
	Events     *Events    `yaml:"events"`
}

// Color - умение color_setting: цвет HSV, цветовая температура или оба. Пустой Color -
// умения нет.
type Color struct {
	// Setting - настройка цвета: прибор получает {"hue": оттенок, "saturation": насыщенность}
	// в своей шкале. Hue и Saturation - значения прибора, по которым шлюз видит текущий цвет.
	Setting    string `yaml:"setting,omitempty"`
	Hue        string `yaml:"hue,omitempty"`
	Saturation string `yaml:"saturation,omitempty"`
	// HueMax - верх шкалы оттенка у прибора, например 100 у SmartThings; без него - 360, как у Алисы.
	HueMax *float64 `yaml:"hue_max,omitempty"`
	// Temperature - цветовая температура в кельвинах: настройка-число. MinK и MaxK - пределы;
	// без них - из описания прибора.
	Temperature string `yaml:"temperature,omitempty"`
	MinK        *int   `yaml:"min_k,omitempty"`
	MaxK        *int   `yaml:"max_k,omitempty"`
}

// HueScale - верх шкалы оттенка у прибора.
func (c *Color) HueScale() float64 {
	if c.HueMax != nil {
		return *c.HueMax
	}
	return 360
}

// Mode - умение mode. Задаётся одно из двух: Programs - выбор программы,
// или Option с Values - значение перечисления параметра или настройки.
type Mode struct {
	Instance string `yaml:"instance"`
	// Programs - программа Home Connect -> режим Алисы.
	Programs map[string]string `yaml:"programs"`
	Option   string            `yaml:"option"`
	// Values - значение Home Connect -> режим Алисы.
	Values map[string]string `yaml:"values"`
}

// Range - умение range: число в параметре программы или в настройке.
type Range struct {
	Instance string `yaml:"instance"`
	Option   string `yaml:"option"`
	Unit     string `yaml:"unit,omitempty"`
	// Min, Max и Precision берутся из описания прибора, если не заданы.
	Min       *float64 `yaml:"min,omitempty"`
	Max       *float64 `yaml:"max,omitempty"`
	Precision *float64 `yaml:"precision,omitempty"`
}

// Toggle - умение toggle. Для instance pause без Setting шлюз ставит программу на паузу и
// продолжает её командами, а с Setting, например у телевизора, меняет настройку. Для
// остальных instance Setting обязателен - настройка-флаг прибора.
type Toggle struct {
	Instance string `yaml:"instance"`
	Setting  string `yaml:"setting,omitempty"`
	// On и Off - значения настройки для «включено» и «выключено»; без них - true и false.
	On  string `yaml:"on,omitempty"`
	Off string `yaml:"off,omitempty"`
	// Optional - переключатель есть, только если прибор описывает эту настройку и разрешает её менять:
	// такая настройка есть не у всех приборов типа.
	Optional bool `yaml:"optional,omitempty"`
}

// Power - питание через свойство прибора: Setting получает On или Off.
type Power struct {
	Setting string `yaml:"setting"`
	On      string `yaml:"on"`
	Off     string `yaml:"off"`
	// State и Idle: прибор включён, пока значение State не из Idle. Так стиральная машина
	// включена, пока идёт стирка. Без State прибор включён, пока Setting равно On.
	State string   `yaml:"state,omitempty"`
	Idle  []string `yaml:"idle,omitempty"`
	// Allowed - флаг прибора: при false «включи» получает REMOTE_CONTROL_DISABLED.
	Allowed string `yaml:"allowed,omitempty"`
}

// Property - свойство устройства.
type Property struct {
	// Type - event или float.
	Type     string `yaml:"type"`
	Instance string `yaml:"instance"`
	Status   string `yaml:"status"`
	Unit     string `yaml:"unit,omitempty"`
	// Convert - готовый пересчёт значения прибора для float из Conversions.
	Convert string `yaml:"convert,omitempty"`
	// Scale и Offset - своя формула пересчёта для float: значение × Scale + Offset.
	Scale  *float64 `yaml:"scale,omitempty"`
	Offset *float64 `yaml:"offset,omitempty"`
	// Values - для event: значение Home Connect -> значение Алисы.
	Values map[string]string `yaml:"values,omitempty"`
}

// otherNames - другие имена значений прибора: одно и то же значение приборы называют по-разному,
// например духовка с номером камеры в именах. Профиль хранит основное имя, а значение ищется по всем.
var otherNames = map[string][]string{
	"Cooking.Oven.Status.CurrentCavityTemperature": {"Cooking.Oven.Status.Cavity.001.CurrentTemperature"},
}

// StatusNames - все имена значения status: основное и другие.
func StatusNames(status string) []string {
	return append([]string{status}, otherNames[status]...)
}

// StatusValue - значение status в values: по основному имени, затем по другим.
func StatusValue(values map[string]any, status string) (any, bool) {
	if v, ok := values[status]; ok {
		return v, true
	}
	for _, name := range otherNames[status] {
		if v, ok := values[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// WithStatusNames - значения values, где значение с другим именем есть и под основным: так его
// видят подсказки редактора.
func WithStatusNames(values map[string]any) map[string]any {
	out := maps.Clone(values)
	for status := range otherNames {
		if v, ok := StatusValue(values, status); ok {
			out[status] = v
		}
	}
	return out
}

// Conversion - пересчёт значения прибора: значение × Scale + Offset.
type Conversion struct {
	Scale, Offset float64
}

// Conversions - готовые пересчёты значения прибора для свойства float, например минуты
// прибора в секунды, которые ждёт Алиса.
var Conversions = map[string]Conversion{
	"minutes_to_seconds":    {Scale: 60},
	"hours_to_seconds":      {Scale: 3600},
	"fahrenheit_to_celsius": {Scale: 5.0 / 9, Offset: -160.0 / 9},
	"fraction_to_percent":   {Scale: 100},
}

// Formula - пересчёт свойства: готовый convert или scale и offset; без них - значение как есть.
func (pr *Property) Formula() Conversion {
	if c, ok := Conversions[pr.Convert]; ok {
		return c
	}
	c := Conversion{Scale: 1}
	if pr.Scale != nil {
		c.Scale = *pr.Scale
	}
	if pr.Offset != nil {
		c.Offset = *pr.Offset
	}
	return c
}

// Value - значение для Алисы из значения прибора. Пересчитанное значение округляется до
// сотых: у °F → °C иначе получались бы дроби вроде 21,111111.
func (pr *Property) Value(v float64) float64 {
	c := pr.Formula()
	if c == (Conversion{Scale: 1}) {
		return v
	}
	return math.Round((v*c.Scale+c.Offset)*100) / 100
}

var (
	// instanceRe - instance свойства: так их пишет Яндекс, например pm2.5_density.
	instanceRe = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)
	// unitRe - единица Яндекса, например unit.time.seconds.
	unitRe = regexp.MustCompile(`^unit(\.[a-z0-9_]{1,32}){1,4}$`)
	// eventValueRe - значение свойства event, например long_press.
	eventValueRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// Events - отдельное устройство-датчик «<прибор> - события».
type Events struct {
	// Button - событие прибора -> click, double_click или long_press. Ключ - имя события,
	// шаблон с * (например *_IS_COMPLETE) или переход значения: состояние=значение.
	Button map[string]string `yaml:"button,omitempty"`
	// FoodLevel - события «заканчивается» сухого: соль, зёрна. Если есть хотя бы одно, уровень low.
	FoodLevel []string `yaml:"food_level,omitempty"`
	// FoodLevelIfKnown - такие же события, которые есть не у всех приборов типа: шлюз учитывает их,
	// только если прибор сообщил, что умеет их присылать.
	FoodLevelIfKnown []string `yaml:"food_level_if_known,omitempty"`
	// WaterLevel и WaterLevelIfKnown - то же для жидкостей: вода, ополаскиватель.
	WaterLevel        []string `yaml:"water_level,omitempty"`
	WaterLevelIfKnown []string `yaml:"water_level_if_known,omitempty"`
	// Empty - какие из этих событий значат «уже нет»: при них уровень empty, а не low.
	Empty []string `yaml:"empty,omitempty"`
}

// Resolve собирает профиль прибора: встроенный для типа, затем переопределение
// типа, затем переопределение прибора. Типа без встроенного профиля нужен
// профиль из настроек с yandex_type.
func Resolve(haType string, forType, forDevice *Profile) (*Profile, error) {
	p := Merge(Merge(builtin[haType], forType), forDevice)
	if p.OnOff == "" {
		p.OnOff = "none"
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("профиль %s: %w", haType, err)
	}
	return p, nil
}

// Merge - профиль base, поверх которого заданы поля override. Профиль не проверяется,
// base и override не меняются, nil - пустой профиль.
func Merge(base, override *Profile) *Profile {
	p := &Profile{}
	p.apply(base)
	p.apply(override)
	return p
}

func (p *Profile) apply(o *Profile) {
	if o == nil {
		return
	}
	if o.YandexType != "" {
		p.YandexType = o.YandexType
	}
	if o.OnOff != "" {
		p.OnOff = o.OnOff
	}
	if o.Power != nil {
		pw := *o.Power
		pw.Idle = slices.Clone(o.Power.Idle)
		p.Power = &pw
	}
	if o.DefaultProgram != "" {
		p.DefaultProgram = o.DefaultProgram
	}
	if o.DefaultOptions != nil {
		p.DefaultOptions = maps.Clone(o.DefaultOptions)
	}
	if o.Modes != nil {
		p.Modes = slices.Clone(o.Modes)
	}
	if o.Ranges != nil {
		p.Ranges = slices.Clone(o.Ranges)
	}
	if o.Toggles != nil {
		p.Toggles = slices.Clone(o.Toggles)
	}
	if o.Color != nil {
		c := *o.Color
		p.Color = &c
	}
	if o.Properties != nil {
		p.Properties = slices.Clone(o.Properties)
	}
	if o.Events != nil {
		e := *o.Events
		// Редактор задаёт только основные списки: остальное наследуется, иначе правка кнопки
		// отключила бы уровень воды духовки.
		if p.Events != nil {
			if e.FoodLevelIfKnown == nil {
				e.FoodLevelIfKnown = p.Events.FoodLevelIfKnown
			}
			if e.WaterLevelIfKnown == nil {
				e.WaterLevelIfKnown = p.Events.WaterLevelIfKnown
			}
			if e.Empty == nil {
				e.Empty = p.Events.Empty
			}
		}
		p.Events = &e
	}
}

func (p *Profile) clone() *Profile {
	c := &Profile{}
	c.apply(p)
	return c
}

// Normalize приводит целые числа в default_options к float64: YAML читает 200 как целое,
// а прибор принимает число только как float64.
func (p *Profile) Normalize() {
	for k, v := range p.DefaultOptions {
		switch n := v.(type) {
		case int:
			p.DefaultOptions[k] = float64(n)
		case int64:
			p.DefaultOptions[k] = float64(n)
		case uint64:
			p.DefaultOptions[k] = float64(n)
		}
	}
}

// MarshalYAML пишет только заданные поля. Незаданное поле переопределения - nil,
// а пустой список заменяет встроенный пустым, поэтому nil и пустое различаются.
func (p Profile) MarshalYAML() (any, error) {
	return mapping([]field{
		{"yandex_type", p.YandexType != "", p.YandexType},
		{"on_off", p.OnOff != "", p.OnOff},
		{"power", p.Power != nil, p.Power},
		{"default_program", p.DefaultProgram != "", p.DefaultProgram},
		{"default_options", p.DefaultOptions != nil, p.DefaultOptions},
		{"modes", p.Modes != nil, p.Modes},
		{"ranges", p.Ranges != nil, p.Ranges},
		{"toggles", p.Toggles != nil, p.Toggles},
		{"color", p.Color != nil, p.Color},
		{"properties", p.Properties != nil, p.Properties},
		{"events", p.Events != nil, p.Events},
	})
}

// MarshalYAML пишет programs или values, даже пустые: по ним видно, что выбирает режим.
func (m Mode) MarshalYAML() (any, error) {
	return mapping([]field{
		{"instance", true, m.Instance},
		{"programs", m.Programs != nil, m.Programs},
		{"option", m.Option != "", m.Option},
		{"values", m.Values != nil, m.Values},
	})
}

type field struct {
	key string
	set bool
	v   any
}

func mapping(fields []field) (*yaml.Node, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, f := range fields {
		if !f.set {
			continue
		}
		var v yaml.Node
		if err := v.Encode(f.v); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: f.key}, &v)
	}
	return n, nil
}

// ProgramMode возвращает умение mode, которое выбирает программу, или nil.
func (p *Profile) ProgramMode() *Mode {
	for i := range p.Modes {
		if p.Modes[i].Programs != nil {
			return &p.Modes[i]
		}
	}
	return nil
}

// Validate проверяет профиль. Ошибка в профиле видна при запуске, а не при команде Алисы.
func (p *Profile) Validate() error {
	if p.YandexType == "" {
		return errors.New("не задан yandex_type")
	}
	if !slices.Contains(OnOffKinds, p.OnOff) {
		return fmt.Errorf("on_off: %q, допустимо %v", p.OnOff, OnOffKinds)
	}
	if p.OnOff == "program" && p.ProgramMode() == nil && p.DefaultProgram == "" {
		return errors.New("on_off: program требует default_program или режим с programs")
	}
	if p.OnOff == "none" && len(p.Modes)+len(p.Ranges)+len(p.Toggles)+len(p.Properties) == 0 && p.Color == nil {
		return errors.New("у устройства нет ни умений, ни свойств")
	}
	if err := p.Color.validate(); err != nil {
		return err
	}
	if pw := p.Power; pw != nil {
		switch {
		case pw.Setting == "" || pw.On == "" || pw.Off == "":
			return errors.New("power: нужны setting, on и off")
		case pw.On == pw.Off:
			return errors.New("power: on и off совпадают")
		case (pw.State == "") != (len(pw.Idle) == 0):
			return errors.New("power: state и idle задаются вместе")
		}
	}
	instances := map[string]bool{}
	unique := func(kind, instance string) error {
		if instance == "" {
			return fmt.Errorf("%s: не задан instance", kind)
		}
		if instances[kind+"/"+instance] {
			return fmt.Errorf("%s: instance %q повторяется", kind, instance)
		}
		instances[kind+"/"+instance] = true
		return nil
	}
	programModes := 0
	for _, m := range p.Modes {
		if err := unique("modes", m.Instance); err != nil {
			return err
		}
		if !slices.Contains(ModeInstances, m.Instance) {
			return fmt.Errorf("modes: instance %q не поддерживается", m.Instance)
		}
		values := m.Values
		switch {
		case m.Programs != nil && m.Option == "" && m.Values == nil:
			programModes++
			values = m.Programs
		case m.Programs == nil && m.Option != "" && m.Values != nil:
		default:
			return fmt.Errorf("modes %s: нужно задать programs или option с values", m.Instance)
		}
		seen := map[string]bool{}
		for _, v := range values {
			if !slices.Contains(ModeValues, v) {
				return fmt.Errorf("modes %s: режим %q не поддерживается", m.Instance, v)
			}
			if seen[v] {
				return fmt.Errorf("modes %s: режим %q назначен двум значениям", m.Instance, v)
			}
			seen[v] = true
		}
	}
	if programModes > 1 {
		return errors.New("modes: выбор программы может быть только в одном режиме")
	}
	for _, r := range p.Ranges {
		if err := unique("ranges", r.Instance); err != nil {
			return err
		}
		if r.Option == "" {
			return fmt.Errorf("ranges %s: не задан option", r.Instance)
		}
		if r.Min != nil && r.Max != nil && *r.Min >= *r.Max {
			return fmt.Errorf("ranges %s: min не меньше max", r.Instance)
		}
		if r.Precision != nil && *r.Precision <= 0 {
			return fmt.Errorf("ranges %s: precision должен быть больше нуля", r.Instance)
		}
	}
	for _, t := range p.Toggles {
		if err := unique("toggles", t.Instance); err != nil {
			return err
		}
		if t.Instance != "pause" && t.Setting == "" {
			return fmt.Errorf("toggles %s: не задан setting; без него бывает только pause", t.Instance)
		}
		if (t.On == "") != (t.Off == "") || (t.On != "" && t.On == t.Off) {
			return fmt.Errorf("toggles %s: on и off задаются вместе и различаются", t.Instance)
		}
	}
	for _, pr := range p.Properties {
		if !slices.Contains(PropertyTypes, pr.Type) {
			return fmt.Errorf("properties %s: type %q, допустимо %v", pr.Instance, pr.Type, PropertyTypes)
		}
		if err := unique("properties/"+pr.Type, pr.Instance); err != nil {
			return err
		}
		if !instanceRe.MatchString(pr.Instance) {
			return fmt.Errorf("properties %s: instance - латинские строчные буквы, цифры, _ и ., до 64 знаков", pr.Instance)
		}
		if pr.Status == "" {
			return fmt.Errorf("properties %s: не задан status", pr.Instance)
		}
		if pr.Unit != "" && !unitRe.MatchString(pr.Unit) {
			return fmt.Errorf("properties %s: единица %q - вида unit.time.seconds", pr.Instance, pr.Unit)
		}
		if err := checkFormula(&pr); err != nil {
			return err
		}
		if pr.Type == "event" && len(pr.Values) == 0 {
			return fmt.Errorf("properties %s: для event нужны values", pr.Instance)
		}
		for _, v := range pr.Values {
			if !eventValueRe.MatchString(v) {
				return fmt.Errorf("properties %s: значение Алисы %q - латинские строчные буквы, цифры и _", pr.Instance, v)
			}
		}
	}
	if p.Events != nil {
		for k, v := range p.Events.Button {
			if !slices.Contains(ButtonValues, v) {
				return fmt.Errorf("events.button %s: %q, допустимо %v", k, v, ButtonValues)
			}
			if state, value, ok := strings.Cut(k, "="); ok && (state == "" || value == "") {
				return fmt.Errorf("events.button %s: переход задаётся как состояние=значение", k)
			}
			if _, err := path.Match(k, ""); err != nil {
				return fmt.Errorf("events.button %s: неверный шаблон", k)
			}
		}
	}
	return nil
}

// Match - событие датчика для события прибора: точное имя, затем шаблон с *.
// Из шаблонов выигрывает самый длинный, при равной длине - первый по алфавиту.
func (e *Events) Match(event string) (string, bool) {
	if v, ok := e.Button[event]; ok {
		return v, true
	}
	var best string
	found := false
	for k := range e.Button {
		if !strings.Contains(k, "*") {
			continue
		}
		if ok, _ := path.Match(k, event); ok && (!found || len(k) > len(best) || (len(k) == len(best) && k < best)) {
			best, found = k, true
		}
	}
	if !found {
		return "", false
	}
	return e.Button[best], true
}

// Transitions - переходы значений, на которые датчик отвечает событием: состояние -> значения.
func (e *Events) Transitions() map[string][]string {
	out := map[string][]string{}
	for k := range e.Button {
		if state, value, ok := strings.Cut(k, "="); ok {
			out[state] = append(out[state], value)
		}
	}
	return out
}

// Пределы цветовой температуры, которые принимает шлюз.
const (
	minKelvin = 1000
	maxKelvin = 10000
)

// validate проверяет цвет: цвет задаётся настройкой вместе с оттенком и насыщенностью,
// температура - настройкой с пределами в кельвинах. nil - умения нет.
func (c *Color) validate() error {
	if c == nil || *c == (Color{}) {
		// Пустой цвет - переопределение без умения: так его убирают у типа или прибора.
		return nil
	}
	hsv := c.Setting != "" || c.Hue != "" || c.Saturation != ""
	switch {
	case !hsv && c.Temperature == "":
		return errors.New("color: нужны setting с hue и saturation или temperature")
	case hsv && (c.Setting == "" || c.Hue == "" || c.Saturation == ""):
		return errors.New("color: setting, hue и saturation задаются вместе")
	case c.HueMax != nil && !(*c.HueMax > 0 && *c.HueMax <= 65535):
		return errors.New("color: hue_max - число больше 0 и не больше 65535")
	case (c.MinK != nil || c.MaxK != nil) && c.Temperature == "":
		return errors.New("color: min_k и max_k задаются только с temperature")
	}
	for _, k := range []*int{c.MinK, c.MaxK} {
		if k != nil && (*k < minKelvin || *k > maxKelvin) {
			return fmt.Errorf("color: температура %d K, допустимо от %d до %d", *k, minKelvin, maxKelvin)
		}
	}
	if c.MinK != nil && c.MaxK != nil && *c.MinK >= *c.MaxK {
		return errors.New("color: min_k не меньше max_k")
	}
	return nil
}

// HasEvents сообщает, нужен ли прибору датчик событий.
func (p *Profile) HasEvents() bool {
	return p.Events != nil && (len(p.Events.Button) > 0 || len(p.Events.FoodLevel) > 0 || len(p.Events.WaterLevel) > 0)
}

// checkFormula проверяет пересчёт свойства: он только у float, convert - из Conversions и
// не вместе со scale и offset, множитель не 0, числа конечные.
func checkFormula(pr *Property) error {
	custom := pr.Scale != nil || pr.Offset != nil
	if pr.Convert == "" && !custom {
		return nil
	}
	switch {
	case pr.Type != "float":
		return fmt.Errorf("properties %s: пересчёт - только у float", pr.Instance)
	case pr.Convert != "" && custom:
		return fmt.Errorf("properties %s: convert и scale с offset вместе не задаются", pr.Instance)
	case pr.Convert != "":
		if _, ok := Conversions[pr.Convert]; !ok {
			return fmt.Errorf("properties %s: convert %q, допустимо %v", pr.Instance, pr.Convert, slices.Sorted(maps.Keys(Conversions)))
		}
		return nil
	}
	c := pr.Formula()
	if c.Scale == 0 || math.IsNaN(c.Scale) || math.IsInf(c.Scale, 0) || math.IsNaN(c.Offset) || math.IsInf(c.Offset, 0) {
		return fmt.Errorf("properties %s: scale - число, кроме 0, offset - число", pr.Instance)
	}
	return nil
}
