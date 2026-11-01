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

// Category - категория компонента: производителя, иначе первая; у компонента без категории -
// категория main.
func (d *Device) Category(component string) string {
	for _, id := range []string{component, mainComponent} {
		c := d.Component(id)
		if c == nil || len(c.Categories) == 0 {
			continue
		}
		for _, cat := range c.Categories {
			if cat.Type == "manufacturer" {
				return cat.Name
			}
		}
		return c.Categories[0].Name
	}
	return "Others"
}

// Type - тип устройства для профиля по категории компонента.
func (d *Device) Type(component string) string {
	return TypePrefix + d.Category(component)
}

// Components - компоненты, которые можно добавить отдельным прибором: main и те, у которых
// есть свои capability, кроме служебных.
func (d *Device) Parts() []string {
	var out []string
	for _, c := range d.Components {
		if c.ID == mainComponent || len(c.Capabilities) > 0 {
			out = append(out, c.ID)
		}
	}
	return out
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

// scalar - значение атрибута для прибора: строка как есть, число - float64, флаг - bool.
// Списки и объекты в состояние не попадают. Температура в °F - в °C: шлюз работает в
// градусах Цельсия.
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
		if v.Unit == "F" {
			return math.Round((t-32)*5/9*10) / 10, true
		}
		return t, true
	case bool:
		return t, true
	}
	return nil, false
}

// listValues - значения из атрибута-списка supported*: строки или объекты с id.
func listValues(v AttrValue) []string {
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
