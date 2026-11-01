package thinq

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Профиль прибора LG раскладывает свойства по ресурсам тремя способами, как в библиотеке LG:
//   - прибор из частей, например стиральная машина: property - список частей, у каждой
//     location.locationName, а команда несёт location той же части;
//   - прибор с камерами, например холодильник: ресурс - список записей с locationName,
//     а команда кладёт locationName внутрь ресурса. Запись MAIN, например дверь, относится
//     ко всему прибору и есть у каждой камеры;
//   - температура по единицам, например у кондиционера: ресурс - список записей с unit,
//     имена свойств оканчиваются на C или F, а команда пишет имя без окончания и unit.
// Шлюз работает в градусах Цельсия. Значения перечислений он хранит заглавными, а в
// команде отправляет так, как их записал профиль.

// prop - свойство профиля LG.
type prop struct {
	// Kind - вид из профиля: enum, range, boolean, number, string и другие.
	Kind        string
	Read, Write bool
	// Values - значения перечисления заглавными: для записи, а без записи - для чтения.
	Values []string
	// orig - значение заглавными -> значение, как его записал профиль.
	orig map[string]string
	// Min, Max и Step - пределы range; Ranged - пределы заданы.
	Min, Max, Step float64
	Ranged         bool
	// loc - камера записи, из которой взято свойство; пусто - без камеры.
	loc string
}

// mainLocation - запись или часть всего прибора.
const mainLocation = "MAIN"

// layout - как ресурс лежит в профиле и в командах.
type layout int

const (
	flat layout = iota
	cameraList
	unitList
)

// model - то, что шлюз знает о приборе LG или его камере по профилю.
type model struct {
	DeviceType string
	props      map[string]*prop
	layouts    map[string]layout
	// parts - прибор из частей: команды несут location.
	parts bool
	// Location - камера или часть модели; пусто - камер нет.
	Location string
	// Locations - все камеры или части профиля по порядку.
	Locations []string
	// Push - коды уведомлений заглавными.
	Push []string
}

// errNoLocation - в профиле нет камеры из настроек.
var errNoLocation = errors.New("в профиле прибора нет такой камеры")

// parseProfile разбирает профиль LG для камеры location; пусто - первая камера или без камер.
func parseProfile(deviceType string, raw []byte, location string) (*model, error) {
	var p struct {
		Property     json.RawMessage `json:"property"`
		Notification struct {
			Push []string `json:"push"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("профиль LG не разобран: %w", err)
	}
	m := &model{DeviceType: deviceType, props: map[string]*prop{}, layouts: map[string]layout{}}
	for _, code := range p.Notification.Push {
		m.Push = append(m.Push, strings.ToUpper(code))
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(p.Property, &parts) == nil {
		m.parts = true
		for _, part := range parts {
			m.Locations = append(m.Locations, partName(part))
		}
		// Без камеры в настройках - часть MAIN, а без неё - первая.
		switch {
		case location == "" && slices.Contains(m.Locations, mainLocation):
			location = mainLocation
		case location == "" && len(m.Locations) > 0:
			location = m.Locations[0]
		}
		i := slices.Index(m.Locations, location)
		if i < 0 {
			return nil, errNoLocation
		}
		m.Location = location
		m.addResources(parts[i])
		return m, nil
	}
	var res map[string]json.RawMessage
	if err := json.Unmarshal(p.Property, &res); err != nil {
		return nil, fmt.Errorf("в профиле LG нет свойств: %w", err)
	}
	for _, key := range sortedKeys(res) {
		for _, e := range entries(res[key]) {
			if name, ok := stringField(e, "locationName"); ok && name != mainLocation && !slices.Contains(m.Locations, name) {
				m.Locations = append(m.Locations, name)
			}
		}
	}
	switch {
	case location != "" && !slices.Contains(m.Locations, location):
		return nil, errNoLocation
	case location != "":
		m.Location = location
	case len(m.Locations) > 0:
		m.Location = m.Locations[0]
	}
	m.addResources(res)
	return m, nil
}

func partName(part map[string]json.RawMessage) string {
	var loc struct {
		LocationName string `json:"locationName"`
	}
	json.Unmarshal(part["location"], &loc)
	return loc.LocationName
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// entries - записи ресурса-списка; у ресурса-объекта записей нет.
func entries(raw json.RawMessage) []map[string]json.RawMessage {
	var list []map[string]json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	return list
}

func stringField(e map[string]json.RawMessage, key string) (string, bool) {
	var s string
	if v, ok := e[key]; !ok || json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// addResources добавляет свойства ресурсов: без камеры и камеры модели.
func (m *model) addResources(res map[string]json.RawMessage) {
	for resource, raw := range res {
		if resource == "location" {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			m.layouts[resource] = flat
			for key, d := range obj {
				m.props[resource+"."+key] = parseProp(d)
			}
			continue
		}
		for _, e := range entries(raw) {
			name, camera := stringField(e, "locationName")
			if camera {
				m.layouts[resource] = cameraList
				if !m.ownLocation(name) {
					continue
				}
			} else if _, ok := e["unit"]; ok {
				m.layouts[resource] = unitList
			}
			for key, d := range e {
				if key != "locationName" && key != "unit" {
					pr := parseProp(d)
					pr.loc = name
					m.props[resource+"."+key] = pr
				}
			}
		}
	}
}

// ownLocation сообщает, относится ли запись камеры name к модели: своя камера или MAIN.
func (m *model) ownLocation(name string) bool {
	return name == m.Location || name == mainLocation
}

// parseProp разбирает описание свойства. Описание строкой - свойство только для чтения.
func parseProp(raw json.RawMessage) *prop {
	var d struct {
		Type  string   `json:"type"`
		Mode  []string `json:"mode"`
		Value struct {
			R json.RawMessage `json:"r"`
			W json.RawMessage `json:"w"`
		} `json:"value"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return &prop{Kind: "string", Read: true}
	}
	p := &prop{Kind: d.Type, Read: d.Mode == nil || slices.Contains(d.Mode, "r"), Write: slices.Contains(d.Mode, "w")}
	src := d.Value.R
	if p.Write && len(d.Value.W) > 0 {
		src = d.Value.W
	}
	switch d.Type {
	case "enum":
		var values []any
		json.Unmarshal(src, &values)
		p.orig = map[string]string{}
		for _, v := range values {
			if s, ok := v.(string); ok {
				up := strings.ToUpper(s)
				p.Values = append(p.Values, up)
				p.orig[up] = s
			}
		}
	case "range":
		var r struct {
			Min, Max, Step *float64
		}
		if json.Unmarshal(src, &r) == nil && r.Min != nil && r.Max != nil {
			p.Min, p.Max, p.Ranged = *r.Min, *r.Max, true
			if r.Step != nil {
				p.Step = *r.Step
			}
		}
	}
	return p
}

// flatten раскладывает состояние или его часть в значения «ресурс.свойство» для модели.
func (m *model) flatten(raw json.RawMessage, out map[string]any) {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		for _, part := range parts {
			if partName(part) == m.Location || (m.Location == "" && len(parts) == 1) {
				m.flattenResources(part, out)
			}
		}
		return
	}
	var res map[string]json.RawMessage
	if json.Unmarshal(raw, &res) == nil {
		m.flattenResources(res, out)
	}
}

func (m *model) flattenResources(res map[string]json.RawMessage, out map[string]any) {
	for resource, raw := range res {
		if resource == "location" {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			for key, v := range obj {
				out[resource+"."+key] = canon(v)
			}
			continue
		}
		for _, e := range entries(raw) {
			if name, ok := stringField(e, "locationName"); ok && !m.ownLocation(name) {
				continue
			}
			for key, v := range e {
				if key != "locationName" && key != "unit" {
					out[resource+"."+key] = canon(v)
				}
			}
		}
	}
}

// canon - значение состояния: строка заглавными, число - float64, флаг - bool.
func canon(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return strings.ToUpper(s)
	}
	return v
}

// errReadOnly и errBadValue - команда не уходит: свойство не записать или значение не подходит.
var (
	errReadOnly = errors.New("свойство только для чтения или его нет в профиле")
	errBadValue = errors.New("значения нет в профиле или оно вне пределов")
)

// value - значение для команды по профилю: значение перечисления как в профиле, число в
// пределах и с шагом, флаг.
func (m *model) value(key string, v any) (any, error) {
	p := m.props[key]
	if p == nil || !p.Write {
		return nil, errReadOnly
	}
	switch p.Kind {
	case "enum":
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		if orig, ok := p.orig[strings.ToUpper(s)]; ok {
			return orig, nil
		}
		return nil, errBadValue
	case "range", "number":
		f, ok := v.(float64)
		if s, isString := v.(string); isString {
			var err error
			if f, err = strconv.ParseFloat(s, 64); err != nil {
				return nil, errBadValue
			}
			ok = true
		}
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errBadValue
		}
		if p.Ranged && (f < p.Min || f > p.Max) {
			return nil, errBadValue
		}
		if p.Ranged && p.Step > 0 && math.Abs(math.Remainder(f-p.Min, p.Step)) > 1e-9 {
			return nil, errBadValue
		}
		return f, nil
	case "boolean":
		switch v {
		case true, "true", "TRUE":
			return true, nil
		case false, "false", "FALSE":
			return false, nil
		}
		return nil, errBadValue
	}
	return v, nil
}

// payload - тело команды: ресурс со свойством и, если нужно, камера и единица.
func (m *model) payload(key string, v any) map[string]any {
	resource, name, _ := strings.Cut(key, ".")
	body := map[string]any{}
	switch m.layouts[resource] {
	case cameraList:
		body["locationName"] = m.props[key].loc
		body[name] = v
		if strings.HasSuffix(name, "TemperatureC") {
			body["unit"] = "C"
		}
	case unitList:
		body[strings.TrimSuffix(name, "C")] = v
		body["unit"] = "C"
	default:
		body[name] = v
	}
	out := map[string]any{resource: body}
	if m.parts && m.Location != "" {
		out["location"] = map[string]any{"locationName": m.Location}
	}
	return out
}
