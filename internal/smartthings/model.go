package smartthings

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// TypePrefix - приставка типа устройства SmartThings для профиля: SmartThings.Television.
// Без неё категория Washer совпала бы с типом Home Connect.
const TypePrefix = "SmartThings."

// mainComponent - основной компонент устройства.
const mainComponent = "main"

// Device - устройство аккаунта SmartThings.
type Device struct {
	ID           string      `json:"deviceId"`
	Name         string      `json:"name"`
	Label        string      `json:"label"`
	Manufacturer string      `json:"manufacturerName"`
	LocationID   string      `json:"locationId"`
	Components   []Component `json:"components"`
	// DisabledComponents - компоненты, которых на этом устройстве на самом деле нет: их шлюз
	// узнаёт из состояния custom.disabledComponents и хранит вместе со списком.
	DisabledComponents []string `json:"disabledComponents,omitempty"`
}

// Component - компонент устройства: capability и категории.
type Component struct {
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	Capabilities []CapRef `json:"capabilities"`
	Categories   []struct {
		Name string `json:"name"`
		// Type - manufacturer, если категорию задал производитель, или user, если владелец.
		Type string `json:"categoryType"`
	} `json:"categories"`
}

// CapRef - capability компонента и её версия.
type CapRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// DisplayName - имя устройства для списка: подпись владельца, иначе имя.
func (d *Device) DisplayName() string {
	if d.Label != "" {
		return d.Label
	}
	return d.Name
}

// Component - компонент по id или nil.
func (d *Device) Component(id string) *Component {
	for i := range d.Components {
		if d.Components[i].ID == id {
			return &d.Components[i]
		}
	}
	return nil
}

// Category - категория компонента: производителя, иначе первая. У компонента без категории или
// с общей Other и Others - категория main: так производитель помечает камеры холодильника.
func (d *Device) Category(component string) string {
	general := ""
	for _, id := range []string{component, mainComponent} {
		c := d.Component(id)
		if c == nil || len(c.Categories) == 0 {
			continue
		}
		name := c.Categories[0].Name
		for _, cat := range c.Categories {
			if cat.Type == "manufacturer" {
				name = cat.Name
				break
			}
		}
		if name != "Other" && name != "Others" {
			return name
		}
		if general == "" {
			general = name
		}
	}
	if general != "" {
		return general
	}
	return "Others"
}

// Type - тип устройства для профиля по категории компонента.
func (d *Device) Type(component string) string {
	return TypePrefix + d.Category(component)
}

// Parts - компоненты, которые можно добавить отдельным прибором: main и те, у которых есть свои
// capability, кроме отключённых.
func (d *Device) Parts() []string {
	var out []string
	for _, c := range d.Components {
		if c.ID == mainComponent || (len(c.Capabilities) > 0 && !d.componentOff(c.ID)) {
			out = append(out, c.ID)
		}
	}
	return out
}

// componentOff сообщает, что компонент отключён. Компонент lamp остаётся даже отключённым: так
// поступает Home Assistant.
func (d *Device) componentOff(id string) bool {
	if id == mainComponent || id == "lamp" {
		return false
	}
	return slices.ContainsFunc(d.DisabledComponents, func(off string) bool {
		return burnerNumber(off) == burnerNumber(id)
	})
}

// burnerNumber - имя компонента без нуля в номере конфорки: компонент называется burner-06, а в
// списке отключённых - burner-6.
func burnerNumber(id string) string {
	if n, ok := strings.CutPrefix(id, "burner-"); ok {
		return "burner-" + strings.TrimLeft(n, "0")
	}
	return id
}

// hasDisabledList сообщает, может ли у устройства быть список отключённых компонентов.
func (d *Device) hasDisabledList() bool {
	c := d.Component(mainComponent)
	return c != nil && len(d.Components) > 1 && slices.ContainsFunc(c.Capabilities, func(cr CapRef) bool {
		return cr.ID == capDisabledComponents
	})
}

// disabledComponents - отключённые компоненты из состояния устройства.
func disabledComponents(st Status) []string {
	return listValues(st[mainComponent][capDisabledComponents]["disabledComponents"])
}

// capabilities - capability прибора: своего компонента, а у компонента не main - ещё
// capability main, которых у компонента нет, как дверь у камеры холодильника.
func (d *Device) capabilities(component string) []CapRef {
	var out []CapRef
	seen := map[string]bool{}
	for _, id := range []string{component, mainComponent} {
		c := d.Component(id)
		if c == nil {
			continue
		}
		for _, cr := range c.Capabilities {
			if !seen[cr.ID] {
				seen[cr.ID] = true
				out = append(out, cr)
			}
		}
	}
	return out
}

// owner - компонент, из которого прибор берёт capability: свой, если он у него есть, иначе main.
func (d *Device) owner(component, capability string) string {
	if c := d.Component(component); c != nil {
		for _, cr := range c.Capabilities {
			if cr.ID == capability {
				return component
			}
		}
	}
	return mainComponent
}

// CapDef - описание capability из облака: атрибуты и команды.
type CapDef struct {
	ID         string             `json:"id"`
	Version    int                `json:"version"`
	Attributes map[string]AttrDef `json:"attributes"`
	Commands   map[string]CmdDef  `json:"commands"`
}

// AttrDef - атрибут capability: схема значения, команда записи и команды по значениям.
type AttrDef struct {
	Schema struct {
		Properties struct {
			Value Schema `json:"value"`
		} `json:"properties"`
	} `json:"schema"`
	Setter       string `json:"setter"`
	EnumCommands []struct {
		Command string `json:"command"`
		Value   string `json:"value"`
	} `json:"enumCommands"`
}

// CmdDef - команда capability и её аргументы.
type CmdDef struct {
	Arguments []struct {
		Name     string `json:"name"`
		Optional bool   `json:"optional"`
		Schema   Schema `json:"schema"`
	} `json:"arguments"`
}

// Schema - схема значения JSON Schema: тип, перечисление и пределы.
type Schema struct {
	Type    string   `json:"type"`
	Enum    []string `json:"enum"`
	Minimum *float64 `json:"minimum"`
	Maximum *float64 `json:"maximum"`
}

// required - число обязательных аргументов команды.
func (c *CmdDef) required() int {
	n := 0
	for _, a := range c.Arguments {
		if !a.Optional {
			n++
		}
	}
	return n
}

// writable сообщает, можно ли записать атрибут: командой записи или командой по значению.
func (a *AttrDef) writable() bool {
	return a.Setter != "" || len(a.EnumCommands) > 0
}

// boolEnum - атрибут-строка со значениями true и false: шлюз хранит его флагом.
func (s *Schema) boolEnum() bool {
	return s.Type == "string" && len(s.Enum) == 2 && slices.Contains(s.Enum, "true") && slices.Contains(s.Enum, "false")
}

// Status - состояние устройства: компонент -> capability -> атрибут -> значение.
type Status map[string]map[string]map[string]AttrValue

// AttrValue - значение атрибута с единицей.
type AttrValue struct {
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit"`
	// StateChange - из потока событий: false, если значение не новое. Состояние его не содержит.
	StateChange *bool `json:"-"`
}

// Key - имя функции прибора: capability и атрибут или команда.
func Key(capability, name string) string {
	return capability + "." + name
}

// SplitKey разбирает имя функции: capability бывает с точкой, например samsungce.washerCycle,
// а у атрибута и команды точки нет.
func SplitKey(key string) (capability, name string, ok bool) {
	i := strings.LastIndexByte(key, '.')
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// toCelsius - температура в °F в градусах Цельсия с точностью 0,1.
func toCelsius(f float64) float64 {
	return math.Round((f-32)*5/9*10) / 10
}

// scalar - значение атрибута для прибора: строка как есть, число - float64, флаг - bool.
// Списки и объекты в состояние не попадают. Температура в °F - в °C: шлюз работает в
// градусах Цельсия. Энергия в Wh - в кВт·ч, как ждёт Алиса, а значения в mWh и kVAh шлюз не
// передаёт: их нельзя перевести в кВт·ч без догадки. Объём газа в ccf - в м³, ток в mA - в А.
func scalar(v AttrValue, s *Schema) (any, bool) {
	var x any
	if len(v.Value) == 0 || json.Unmarshal(v.Value, &x) != nil {
		return nil, false
	}
	switch t := x.(type) {
	case string:
		if s != nil && s.boolEnum() {
			return t == "true", true
		}
		return t, true
	case float64:
		switch v.Unit {
		case "F":
			return toCelsius(t), true
		case "Wh":
			return t / 1000, true
		case "mWh", "kVAh":
			return nil, false
		case "ccf":
			return t * cubicMetersPerCcf, true
		case "mA":
			return t / 1000, true
		}
		return t, true
	case bool:
		return t, true
	}
	return nil, false
}

// cubicMetersPerCcf - кубометров в сотне кубических футов, ccf.
const cubicMetersPerCcf = 2.83168

// numberField - поле-число атрибута-объекта: шлюз хранит его отдельным значением «атрибут.поле».
type numberField struct {
	Name string
	// Divisor переводит значение устройства в единицу Алисы.
	Divisor float64
	// Counter - счётчик: отрицательное значение значит, что устройство счётчика не знает, Алиса его не получает.
	Counter bool
}

// objectFields - атрибуты-объекты, из которых шлюз берёт числа. Отчёт о расходе сообщает энергию в Wh, а
// мощность в Вт; расход воды - в мл: так его понимает Home Assistant.
var objectFields = map[string][]numberField{
	"powerConsumptionReport.powerConsumption":           {{Name: "energy", Divisor: 1000, Counter: true}, {Name: "power", Divisor: 1}},
	"samsungce.waterConsumptionReport.waterConsumption": {{Name: "cumulativeAmount", Divisor: 1_000_000, Counter: true}},
}

// fieldValues - поля атрибута-объекта key, которые сообщило устройство: имя значения и число. nil вместо
// числа - поле есть, но Алисе его передавать нельзя.
func fieldValues(key string, v AttrValue) map[string]*float64 {
	var obj map[string]any
	if len(v.Value) == 0 || json.Unmarshal(v.Value, &obj) != nil {
		return nil
	}
	out := map[string]*float64{}
	for _, f := range objectFields[key] {
		x, ok := obj[f.Name].(float64)
		switch {
		case !ok && obj[f.Name] == nil:
		case !ok || (f.Counter && x < 0):
			out[Key(key, f.Name)] = nil
		default:
			x /= f.Divisor
			out[Key(key, f.Name)] = &x
		}
	}
	return out
}

// setpointRange - пределы уставки из атрибута вроде coolingSetpointRange в °C. unit - единица, если у
// самого атрибута её нет. Шаг в °F становится целым числом °C: команда всё равно округляется до °F.
func setpointRange(v AttrValue, unit string) (limit, bool) {
	var r struct {
		Minimum *float64 `json:"minimum"`
		Maximum *float64 `json:"maximum"`
		Step    *float64 `json:"step"`
	}
	if len(v.Value) == 0 || json.Unmarshal(v.Value, &r) != nil || r.Minimum == nil || r.Maximum == nil {
		return limit{}, false
	}
	if v.Unit != "" {
		unit = v.Unit
	}
	l := limit{Min: *r.Minimum, Max: *r.Maximum}
	if r.Step != nil {
		l.Step = *r.Step
	}
	if !l.known() {
		return limit{}, false
	}
	if unit == "F" {
		l.Min, l.Max, l.Step = toCelsius(l.Min), toCelsius(l.Max), max(1, math.Round(l.Step*5/9))
	}
	return l, true
}

// limit - пределы и шаг уставки в °C.
type limit struct {
	Min, Max, Step float64
}

// known сообщает, что пределы настоящие: устройство пишет -1000, если предела не знает.
func (l limit) known() bool {
	return l.Min > unknownBound && l.Max > unknownBound && l.Min < l.Max
}

// unknownBound - температура ниже этой не бывает: так устройство сообщает, что предела не знает, и
// в °C, и после перевода из °F.
const unknownBound = -273.0

// listValues - значения из атрибута-списка supported*: строки или объекты с id.
func listValues(v AttrValue) []string {
	if len(v.Value) == 0 {
		return nil
	}
	var strs []string
	if json.Unmarshal(v.Value, &strs) == nil {
		return strs
	}
	var objs []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(v.Value, &objs) != nil {
		return nil
	}
	var out []string
	for _, o := range objs {
		if o.ID != "" {
			out = append(out, o.ID)
		}
	}
	return out
}

// argument - значение команды по схеме аргумента: целое - числом без дробной части, строка -
// текстом, флаг - флагом, объект - как есть.
func argument(v any, s *Schema) (any, error) {
	switch s.Type {
	case "integer", "number":
		f, ok := v.(float64)
		if str, isStr := v.(string); isStr {
			var err error
			f, err = strconv.ParseFloat(str, 64)
			ok = err == nil
		}
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("нужно число, а не %v", v)
		}
		if (s.Minimum != nil && f < *s.Minimum) || (s.Maximum != nil && f > *s.Maximum) {
			return nil, fmt.Errorf("%v вне пределов", v)
		}
		if s.Type == "integer" {
			return int64(math.Round(f)), nil
		}
		return f, nil
	case "string":
		var str string
		switch x := v.(type) {
		case string:
			str = x
		case float64:
			str = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			str = strconv.FormatBool(x)
		default:
			return nil, fmt.Errorf("нужна строка, а не %v", v)
		}
		if len(s.Enum) > 0 && !slices.Contains(s.Enum, str) {
			return nil, fmt.Errorf("значения %q нет в списке", str)
		}
		return str, nil
	case "boolean":
		switch v {
		case true, "true":
			return true, nil
		case false, "false":
			return false, nil
		}
		return nil, fmt.Errorf("нужен флаг, а не %v", v)
	}
	return v, nil
}
