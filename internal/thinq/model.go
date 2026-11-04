package thinq

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Профиль прибора LG раскладывает свойства по ресурсам четырьмя способами, как в библиотеке LG:
//   - прибор из частей, например стиральная машина: property - список частей, у каждой
//     location.locationName, а команда несёт location той же части;
//   - прибор с камерами, например холодильник: ресурс - список записей с locationName,
//     а команда кладёт locationName внутрь ресурса. Запись MAIN, например дверь, относится
//     ко всему прибору и есть у каждой камеры;
//   - температура по единицам, например у кондиционера: ресурс - список записей с unit, в
//     каждой те же имена свойств. Шлюз читает только запись unit=C, хранит свойство с
//     окончанием C, а команда пишет имя без окончания и unit;
//   - колонна стиральной и сушильной машины: профиль и состояние лежат под верхними ключами
//     washer и dryer, а команда кладёт тело под ключ части.
// Шлюз работает в градусах Цельсия. Значения перечислений он хранит заглавными, а в
// команде отправляет так, как их записал профиль.

// prop - свойство профиля LG.
type prop struct {
	// Kind - вид из профиля: enum, range, boolean, number, string и другие.
	Kind        string
	Read, Write bool
	// Values - значения перечисления заглавными: для записи, а без записи - для чтения. У
	// записываемого range с шагом 1 и не больше maxRangeValues значений - целые числа строками:
	// такое число можно сопоставить режиму Алисы.
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

// celsius - единица записи, из которой шлюз читает температуру.
const celsius = "C"

// maxRangeValues - сколько целых значений range шлюз перечисляет в Values. Больше не нужно:
// скоростей вытяжки шесть, а температуру режимом не выбирают.
const maxRangeValues = 10

// towerParts - верхние ключи частей колонны в профиле, состоянии и командах.
var towerParts = []string{"washer", "dryer"}

const (
	// lampBrightnessKey - яркость подсветки вытяжки, число от 0.
	lampBrightnessKey = "lamp.lampBrightness"
	// lampEnabledKey - свойство шлюза: подсветка горит при любой яркости, кроме 0. Переключатель
	// Алисы умеет только два значения, а яркостей у вытяжки больше.
	lampEnabledKey = "lamp.lampEnabled"
)

// pairedRanges - свойства, которые LG принимает только вместе: команда одного несёт текущее
// значение другого.
var pairedRanges = map[string]string{
	"ventilation.fanSpeed": lampBrightnessKey,
	lampBrightnessKey:      "ventilation.fanSpeed",
}

// targetRule - куда записать заданную температуру. Прибор показывает её одним свойством shown,
// только для чтения, а записывает в свойство своего режима.
type targetRule struct {
	shown string
	// mode - свойство с режимом прибора. circuit - свойство с контуром: AIR или WATER; пусто -
	// контура нет. Без значения контура считается AIR, как в библиотеке LG.
	mode, circuit string
	// by - режим или «контур/режим» -> свойство для записи.
	by map[string]string
	// fallback - запись для режима без своего свойства.
	fallback string
}

var targetRules = map[string]targetRule{
	"DEVICE_AIR_CONDITIONER": {
		shown: "temperatureInUnits.targetTemperatureC",
		mode:  "airConJobMode.currentJobMode",
		by: map[string]string{
			"COOL": "temperatureInUnits.coolTargetTemperatureC",
			"HEAT": "temperatureInUnits.heatTargetTemperatureC",
			"AUTO": "temperatureInUnits.autoTargetTemperatureC",
		},
		fallback: "temperature.targetTemperature",
	},
	"DEVICE_SYSTEM_BOILER": {
		shown:   "roomTemperatureInUnits.targetTemperatureC",
		mode:    "boilerJobMode.currentJobMode",
		circuit: "operation.roomTempMode",
		by: map[string]string{
			"AIR/HEAT":   "roomTemperatureInUnits.airHeatTargetTemperatureC",
			"AIR/COOL":   "roomTemperatureInUnits.airCoolTargetTemperatureC",
			"WATER/HEAT": "roomTemperatureInUnits.waterHeatTargetTemperatureC",
			"WATER/COOL": "roomTemperatureInUnits.waterCoolTargetTemperatureC",
		},
	},
}

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
	// tower - верхний ключ части колонны; пусто - не колонна.
	tower string
	// wires - имя свойства в шлюзе -> имя в ThinQ, если они разные: одно и то же свойство
	// приборы называют по-разному, а встроенный профиль знает одно имя.
	wires map[string]string
	// target - запись заданной температуры по режиму; direct - показанное свойство записывается само.
	target *targetRule
	direct bool
	// Location - камера или часть модели; пусто - камер нет.
	Location string
	// Locations - все камеры или части профиля по порядку.
	Locations []string
	// Push - коды уведомлений заглавными.
	Push []string
}

// errNoLocation - в профиле нет камеры из настроек.
var errNoLocation = errors.New("в профиле прибора нет такой камеры")

// parseTower разбирает профиль колонны: каждая часть лежит под своим верхним ключом и выглядит
// как обычный профиль. ok - профиль такой раскладки.
func parseTower(deviceType string, raw []byte, location string) (m *model, ok bool, err error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || top["property"] != nil {
		return nil, false, nil
	}
	var parts []string
	for _, part := range towerParts {
		if top[part] != nil {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil, false, nil
	}
	if location == "" {
		location = parts[0]
	}
	if !slices.Contains(parts, location) {
		return nil, true, errNoLocation
	}
	if m, err = parseProfile(deviceType, top[location], ""); err != nil {
		return nil, true, err
	}
	m.tower, m.Locations = location, parts
	// Встроенный профиль у колонны один, а питание у частей названо по-разному.
	m.rename("operation.dryerOperationMode", "operation.washerOperationMode")
	return m, true, nil
}

// parseProfile разбирает профиль LG для камеры location; пусто - первая камера или без камер.
func parseProfile(deviceType string, raw []byte, location string) (*model, error) {
	if m, ok, err := parseTower(deviceType, raw, location); ok {
		return m, err
	}
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
		m.finish()
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
	m.finish()
	return m, nil
}

// finish приводит свойства профиля к именам встроенных профилей и добавляет свойства шлюза.
func (m *model) finish() {
	m.rename("airFlow.windStrengthLevel", "airFlow.windStrength")
	switch m.DeviceType {
	case "DEVICE_AIR_CONDITIONER":
		// Прибор без температуры по единицам: те же значения лежат в ресурсе temperature.
		m.rename("temperature.targetTemperature", "temperatureInUnits.targetTemperatureC")
		m.rename("temperature.currentTemperature", "temperatureInUnits.currentTemperatureC")
	case "DEVICE_SYSTEM_BOILER":
		m.rename("roomTemperatureInUnits.airCurrentTemperatureC", "roomTemperatureInUnits.currentTemperatureC")
	}
	if r, ok := targetRules[m.DeviceType]; ok {
		m.addTarget(&r)
	}
	if p := m.props[lampBrightnessKey]; p != nil && p.Write {
		m.props[lampEnabledKey] = &prop{Kind: "boolean", Read: true, Write: true}
	}
}

// rename показывает свойство wire под именем name, если свойства с таким именем в профиле нет.
func (m *model) rename(wire, name string) {
	p := m.props[wire]
	if p == nil || m.props[name] != nil {
		return
	}
	delete(m.props, wire)
	m.props[name] = p
	if m.wires == nil {
		m.wires = map[string]string{}
	}
	m.wires[name] = wire
}

// addTarget делает показанную температуру записываемой, если прибор записывает её хотя бы в
// одном режиме. Устройство Алисы одно на все режимы, поэтому пределы - общие для всех записей,
// а точные пределы шлюз проверяет по свойству того режима, в котором прибор сейчас.
func (m *model) addTarget(r *targetRule) {
	shown := m.props[r.shown]
	m.direct = shown != nil && shown.Write
	var writers []*prop
	if m.direct {
		own := *shown
		writers = append(writers, &own)
	}
	for _, key := range append(slices.Collect(maps.Values(r.by)), r.fallback) {
		if p := m.props[key]; p != nil && p.Write {
			writers = append(writers, p)
		}
	}
	if len(writers) == 0 {
		return
	}
	if shown == nil {
		shown = &prop{Read: true}
		m.props[r.shown] = shown
	}
	shown.Kind, shown.Write, shown.Ranged = "range", true, false
	for _, p := range writers {
		if !p.Ranged {
			continue
		}
		if !shown.Ranged {
			shown.Min, shown.Max, shown.Step, shown.Ranged = p.Min, p.Max, p.Step, true
			continue
		}
		shown.Min, shown.Max = min(shown.Min, p.Min), max(shown.Max, p.Max)
		if p.Step > 0 && (shown.Step == 0 || p.Step < shown.Step) {
			shown.Step = p.Step
		}
	}
	m.target = r
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
			unit, byUnit := stringField(e, "unit")
			byUnit = byUnit && !camera
			if camera {
				m.layouts[resource] = cameraList
				if !m.ownLocation(name) {
					continue
				}
			} else if byUnit {
				m.layouts[resource] = unitList
				if !strings.EqualFold(unit, celsius) {
					continue
				}
			}
			for key, d := range e {
				if key != "locationName" && key != "unit" {
					pr := parseProp(d)
					pr.loc = name
					if byUnit {
						key = celsiusName(key)
					}
					m.props[resource+"."+key] = pr
				}
			}
		}
	}
}

// celsiusName - имя свойства из записи unit=C в шлюзе: с окончанием C. В ThinQ окончания нет.
func celsiusName(name string) string {
	return strings.TrimSuffix(name, celsius) + celsius
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
			p.Values = integerValues(p)
		}
	}
	return p
}

// integerValues - целые значения записываемого range с шагом 1, строками; nil, если их много
// или значения не целые.
func integerValues(p *prop) []string {
	if !p.Write || p.Step != 1 || p.Min != math.Trunc(p.Min) || p.Max < p.Min || p.Max-p.Min >= maxRangeValues {
		return nil
	}
	var out []string
	for v := p.Min; v <= p.Max; v++ {
		out = append(out, strconv.FormatFloat(v, 'f', -1, 64))
	}
	return out
}

// flatten раскладывает состояние или его часть в значения «ресурс.свойство» для модели.
func (m *model) flatten(raw json.RawMessage, out map[string]any) {
	if m.tower != "" {
		var top map[string]json.RawMessage
		if json.Unmarshal(raw, &top) != nil || top[m.tower] == nil {
			return
		}
		raw = top[m.tower]
	}
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
	if json.Unmarshal(raw, &res) != nil {
		return
	}
	// Отчёт одной части объектом, а не списком: чужая часть не меняет состояние этой.
	if name := partName(res); name != "" && m.Location != "" && name != m.Location {
		return
	}
	m.flattenResources(res, out)
}

// put кладёт значение под именем свойства в шлюзе.
func (m *model) put(out map[string]any, key string, v any) {
	for name, wire := range m.wires {
		if wire == key {
			key = name
			break
		}
	}
	out[key] = v
}

func (m *model) flattenResources(res map[string]json.RawMessage, out map[string]any) {
	for resource, raw := range res {
		if resource == "location" {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			// Единица может прийти один раз: отчёт с изменениями её не повторяет.
			if unit, ok := stringField(obj, "unit"); ok {
				out[resource+".unit"] = strings.ToUpper(unit)
			}
			if out[resource+".unit"] == "F" {
				continue
			}
			for key, v := range obj {
				switch {
				case key == "unit":
				case m.layouts[resource] == unitList:
					m.put(out, resource+"."+celsiusName(key), canon(v))
				default:
					m.put(out, resource+"."+key, canon(v))
				}
			}
			continue
		}
		for _, e := range entries(raw) {
			name, camera := stringField(e, "locationName")
			if camera && !m.ownLocation(name) {
				continue
			}
			unit, byUnit := stringField(e, "unit")
			byUnit = byUnit && !camera
			if byUnit && !strings.EqualFold(unit, celsius) {
				continue
			}
			for key, v := range e {
				if key != "locationName" && key != "unit" {
					if byUnit {
						key = celsiusName(key)
					}
					m.put(out, resource+"."+key, canon(v))
				}
			}
		}
	}
}

// derive считает свойства шлюза из значений прибора.
func (m *model) derive(values map[string]any) {
	if m.props[lampEnabledKey] == nil {
		return
	}
	if b, ok := values[lampBrightnessKey].(float64); ok {
		values[lampEnabledKey] = b != 0
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

// errReadOnly, errBadValue и errWrongState - команда не уходит: свойство не записать, значение не
// подходит или прибор сейчас в таком состоянии, что команду не собрать.
var (
	errReadOnly   = errors.New("свойство только для чтения или его нет в профиле")
	errBadValue   = errors.New("значения нет в профиле или оно вне пределов")
	errWrongState = errors.New("для команды нет записи в этом режиме или значения второго свойства")
)

// command - тело команды записи key по профилю и текущему состоянию прибора.
func (m *model) command(key string, value any, state map[string]any) (map[string]any, error) {
	if key == lampEnabledKey && m.props[key] != nil {
		on, ok := value.(bool)
		if !ok {
			return nil, errBadValue
		}
		cur, _ := state[lampBrightnessKey].(float64)
		switch {
		case !on:
			cur = 0
		case cur == 0:
			cur = max(m.props[lampBrightnessKey].Min, 1)
		}
		key, value = lampBrightnessKey, cur
	}
	key, err := m.writeKey(key, state)
	if err != nil {
		return nil, err
	}
	v, err := m.value(key, value)
	if err != nil {
		return nil, err
	}
	out := m.payload(key, v)
	if other := pairedRanges[key]; other != "" && m.props[other] != nil {
		cur, ok := state[other].(float64)
		if !ok {
			return nil, errWrongState
		}
		maps.Copy(out, m.payload(other, cur))
	}
	return out, nil
}

// writeKey - свойство, в которое уходит запись key: у заданной температуры - по режиму прибора.
func (m *model) writeKey(key string, state map[string]any) (string, error) {
	r := m.target
	if r == nil || key != r.shown {
		return key, nil
	}
	mode, _ := state[r.mode].(string)
	if r.circuit != "" {
		circuit, _ := state[r.circuit].(string)
		if circuit == "" {
			circuit = "AIR"
		}
		mode = circuit + "/" + mode
	}
	if p := m.props[r.by[mode]]; p != nil && p.Write {
		return r.by[mode], nil
	}
	if m.direct {
		return key, nil
	}
	if p := m.props[r.fallback]; p != nil && p.Write {
		return r.fallback, nil
	}
	return "", errWrongState
}

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
	wire := key
	if w, ok := m.wires[key]; ok {
		wire = w
	}
	resource, name, _ := strings.Cut(wire, ".")
	body := map[string]any{}
	switch m.layouts[resource] {
	case cameraList:
		body["locationName"] = m.props[key].loc
		body[name] = v
		if strings.HasSuffix(name, "TemperatureC") {
			body["unit"] = "C"
		}
	case unitList:
		body[strings.TrimSuffix(name, celsius)] = v
		body["unit"] = celsius
	default:
		body[name] = v
		// Ресурс температуры без записей по единицам сообщает единицу своим свойством.
		if resource == "temperature" && m.props[resource+".unit"] != nil {
			body["unit"] = celsius
		}
	}
	out := map[string]any{resource: body}
	if m.parts && m.Location != "" {
		out["location"] = map[string]any{"locationName": m.Location}
	}
	if m.tower != "" {
		return map[string]any{m.tower: out}
	}
	return out
}
