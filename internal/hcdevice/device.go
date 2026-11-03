// Package hcdevice - прибор Home Connect или LG в Алисе. Команды Алисы и состояние прибора
// переводятся по профилю (пакет profile), а сам прибор виден через appliance.Appliance:
// как он подключён, этому пакету не важно.
package hcdevice

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/metrics"
	"hcgateway/internal/profile"
)

// eventsSuffix - окончание идентификатора датчика событий прибора.
const eventsSuffix = ".events"

var _ alice.Device = (*Device)(nil)

// Device - прибор Home Connect и его профиль в Алисе.
type Device struct {
	ID        string
	Name      string
	Room      string
	Profile   *profile.Profile
	Appliance appliance.Appliance
	// Manufacturer - производитель в списке устройств Алисы; пусто - BSH.
	Manufacturer string
	// Journal получает новые изменения прибора, чтобы они пережили перезапуск; nil - только память.
	// Не должен ждать диска: его вызывает поток событий прибора.
	Journal func([]Change)

	// actMu не даёт двум командам Алисы менять прибор одновременно:
	// смена программы - это остановка и запуск, между ними нельзя вклиниться.
	actMu sync.Mutex

	mu sync.Mutex
	// pending - параметры, заданные голосом, пока программа не идёт; применяются при запуске.
	pending map[string]any
	// button - последнее событие датчика: click, double_click или long_press.
	button string
	// last - последние значения состояний, переходы которых датчик сообщает событием.
	last map[string]string
	// seen и online - значения и связь при прошлом изменении; history - последние изменения.
	seen    map[string]string
	online  bool
	history []Change
	// remain и remainAt - оставшееся время, которое видит Яндекс, и когда оно сменилось;
	// remainSet - его уже сообщали (reported).
	remain    float64
	remainAt  time.Time
	remainSet bool
	// now - часы; nil - time.Now.
	now func() time.Time
}

// RemainingTime - instance свойства float с оставшимся временем программы в секундах. В
// документации Яндекса его нет, и с ним Яндекс отвечает 400 на уведомления о приборе целиком.
const RemainingTime = "remaining_time"

// Duration - время словами с точностью до минуты, минуты - с округлением вверх: «1 ч 25 мин».
func Duration(sec float64) string {
	m := int(math.Ceil(sec / 60))
	switch {
	case m <= 0:
		return "0 мин"
	case m < 60:
		return fmt.Sprintf("%d мин", m)
	case m%60 == 0:
		return fmt.Sprintf("%d ч", m/60)
	}
	return fmt.Sprintf("%d ч %d мин", m/60, m%60)
}

// remainingEvery - не чаще чем через сколько Яндекс видит новое оставшееся время. Чуть
// меньше минуты: значение, которое пришло на долю секунды раньше минуты, не ждёт следующей.
const remainingEvery = 55 * time.Second

// Working сообщает, идёт ли у прибора программа: у Home Connect - по состоянию работы, у
// прибора с питанием через состояние (поле power профиля) - пока оно не из списка idle.
func Working(p *profile.Profile, st *appliance.State) bool {
	if st.Running() {
		return true
	}
	pw := p.Power
	return pw != nil && pw.State != "" && powered(pw, st)
}

// Remaining - оставшееся время программы в секундах по свойству remaining_time профиля,
// после пересчёта свойства. ok - профиль передаёт это время и оно известно. Вне программы
// время - 0: прибор присылает его и для выбранной, ещё не запущенной программы, а после
// конца остаётся последнее.
func Remaining(p *profile.Profile, st *appliance.State) (float64, bool) {
	for i := range p.Properties {
		pr := &p.Properties[i]
		if pr.Type != "float" || pr.Instance != RemainingTime {
			continue
		}
		if !Working(p, st) {
			return 0, true
		}
		v, ok := st.Values[pr.Status].(float64)
		return max(pr.Value(v), 0), ok
	}
	return 0, false
}

// reported - оставшееся время, которое видит Яндекс: новое значение - не чаще раза в
// remainingEvery, а начало и конец программы, то есть переход через 0, - сразу.
func (d *Device) reported(v float64) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if d.now != nil {
		now = d.now()
	}
	if !d.remainSet || (v == 0) != (d.remain == 0) || (v != d.remain && now.Sub(d.remainAt) >= remainingEvery) {
		d.remain, d.remainAt, d.remainSet = v, now, true
	}
	return d.remain
}

// historySize - сколько последних изменений прибор помнит.
const historySize = 50

// Change - изменение прибора для страницы «Всё о приборе».
type Change struct {
	Time time.Time
	// Kind - event, value, online или отметка простоя шлюза.
	Kind  string
	Key   string
	Value string
}

// Restore задаёт сохранённую историю прибора, старые изменения первыми. Вызывается до Run.
func (d *Device) Restore(history []Change) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.history = slices.Clone(history)
}

// History - последние изменения прибора, новые первыми.
func (d *Device) History() []Change {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Change, len(d.history))
	for i, c := range d.history {
		out[len(out)-1-i] = c
	}
	return out
}

// record запоминает события, изменившиеся значения и связь. Первое состояние после
// запуска только запоминается: иначе каждое значение выглядело бы изменением.
func (d *Device) record(st *appliance.State, events []string) {
	now := time.Now()
	var added []Change
	d.mu.Lock()
	add := func(kind, key, value string) {
		c := Change{Time: now, Kind: kind, Key: key, Value: value}
		added = append(added, c)
		d.history = append(d.history, c)
		if len(d.history) > historySize {
			d.history = d.history[len(d.history)-historySize:]
		}
	}
	first := d.seen == nil
	if !first && st.Online != d.online {
		add("online", "", strconv.FormatBool(st.Online))
	}
	d.online = st.Online
	cur := make(map[string]string, len(st.Values))
	for _, k := range slices.Sorted(maps.Keys(st.Values)) {
		v := valueString(st.Values[k])
		cur[k] = v
		if !first && d.seen[k] != v {
			add("value", k, v)
		}
	}
	d.seen = cur
	for _, e := range events {
		add("event", e, "")
	}
	d.mu.Unlock()
	if d.Journal != nil && len(added) > 0 {
		d.Journal(added)
	}
}

// isSetting отличает настройку прибора от параметра программы:
// настройка меняется всегда, параметр - только у работающей программы.
func isSetting(desc *appliance.Description, key string) bool {
	return strings.Contains(key, ".Setting.") || desc.Options[key].Setting
}

func (d *Device) manufacturer() string {
	if d.Manufacturer != "" {
		return d.Manufacturer
	}
	return "BSH"
}

// valueString - значение прибора строкой: так его сравнивают со значениями профиля.
func valueString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// powered сообщает, включён ли прибор по полю power профиля.
func powered(pw *profile.Power, st *appliance.State) bool {
	if pw.State != "" {
		v, ok := st.Values[pw.State]
		return ok && !slices.Contains(pw.Idle, valueString(v))
	}
	return valueString(st.Values[pw.Setting]) == pw.On
}

// toggleState - положение переключателя: флаг настройки или её значения on и off из профиля.
func toggleState(t *profile.Toggle, st *appliance.State) (bool, bool) {
	raw, ok := st.Values[t.Setting]
	if !ok {
		return false, false
	}
	if t.On == "" {
		b, ok := raw.(bool)
		return b, ok
	}
	switch valueString(raw) {
	case t.On:
		return true, true
	case t.Off:
		return false, true
	}
	return false, false
}

func toggleValue(t *profile.Toggle, on bool) any {
	switch {
	case t.On == "":
		return on
	case on:
		return t.On
	}
	return t.Off
}

// IDs - сам прибор и, если профиль сообщает о событиях, его датчик событий.
func (d *Device) IDs() []string {
	if d.Profile.HasEvents() {
		return []string{d.ID, d.ID + eventsSuffix}
	}
	return []string{d.ID}
}

// Run держит связь с прибором и сообщает об изменениях, пока ctx не отменён.
// Событие датчика уходит в changed отдельно: Яндексу его нужно отправить сразу.
func (d *Device) Run(ctx context.Context, changed alice.Changed) {
	d.Appliance.Run(ctx, func(events []string) {
		st := d.Appliance.State()
		events = once(append(slices.Clone(events), d.transitions(&st)...))
		for _, e := range events {
			metrics.DeviceEvent(d.ID, e)
		}
		d.record(&st, events)
		var urgent []alice.DeviceState
		for _, b := range d.buttonFor(events) {
			urgent = append(urgent, buttonState(d.ID+eventsSuffix, b))
		}
		changed(append(urgent, d.pressed(events)...))
	})
}

// once - события без повторов, в прежнем порядке: источник и переход значения могут сообщить об
// одном и том же.
func once(events []string) []string {
	var out []string
	for _, e := range events {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// buttonState - нажатие кнопки устройства id: только оно, без остальных свойств.
func buttonState(id, value string) alice.DeviceState {
	return alice.DeviceState{ID: id, Properties: []alice.CapState{{Type: alice.PropEvent,
		State: alice.StateValue{Instance: alice.InstanceButton, Value: value}}}}
}

// pressed - нажатия кнопки самого прибора, например кнопки SmartThings: событие состояние=значение
// для её свойства button.
func (d *Device) pressed(events []string) []alice.DeviceState {
	var out []alice.DeviceState
	for i := range d.Profile.Properties {
		pr := &d.Profile.Properties[i]
		if pr.Type != "event" || pr.Instance != alice.InstanceButton {
			continue
		}
		for _, e := range events {
			if raw, ok := strings.CutPrefix(e, pr.Status+"="); ok {
				if v, ok := pr.Values[raw]; ok {
					out = append(out, buttonState(d.ID, v))
				}
			}
		}
	}
	return out
}

// transitions - переходы значений из профиля датчика и свойств button, которые случились с прошлого
// изменения: событие состояние=значение. Первое значение после запуска только запоминается, иначе
// прибор, который уже закончил работу, сообщал бы об этом при каждом запуске шлюза.
func (d *Device) transitions(st *appliance.State) []string {
	if !st.Online {
		return nil
	}
	watch := map[string][]string{}
	if d.Profile.HasEvents() {
		watch = d.Profile.Events.Transitions()
	}
	for i := range d.Profile.Properties {
		if pr := &d.Profile.Properties[i]; pr.Type == "event" && pr.Instance == alice.InstanceButton {
			watch[pr.Status] = append(watch[pr.Status], slices.Collect(maps.Keys(pr.Values))...)
		}
	}
	if len(watch) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = map[string]string{}
	}
	var out []string
	for _, state := range slices.Sorted(maps.Keys(watch)) {
		raw, ok := st.Values[state]
		if !ok {
			continue
		}
		cur := valueString(raw)
		prev, seen := d.last[state]
		d.last[state] = cur
		if seen && prev != cur && slices.Contains(watch[state], cur) {
			out = append(out, state+"="+cur)
		}
	}
	return out
}

// Info - описание устройств для списка устройств: сам прибор и, если нужно, датчик событий.
func (d *Device) Info() []alice.DeviceInfo {
	desc := d.Appliance.Describe()
	p := d.Profile
	dev := alice.DeviceInfo{
		ID: d.ID, Name: d.Name, Room: d.Room, Type: alice.TypePrefix + p.YandexType,
		DeviceInfo: alice.Manufacturer{Manufacturer: d.manufacturer(), Model: desc.Type},
	}
	if p.OnOff != "none" {
		dev.Capabilities = append(dev.Capabilities, alice.Capability{Type: alice.CapOnOff, Retrievable: true, Reportable: true})
	}
	for _, m := range p.Modes {
		values := d.modeValues(&m, &desc)
		if len(values) == 0 {
			continue
		}
		dev.Capabilities = append(dev.Capabilities, alice.Capability{Type: alice.CapMode, Retrievable: true, Reportable: true,
			Parameters: alice.ModeParams{Instance: m.Instance, Modes: alice.Items(values)}})
	}
	for _, r := range p.Ranges {
		lim, ok := rangeLimits(&r, &desc)
		if !ok {
			continue
		}
		dev.Capabilities = append(dev.Capabilities, alice.Capability{Type: alice.CapRange, Retrievable: true, Reportable: true,
			Parameters: alice.RangeParams{Instance: r.Instance, Unit: r.Unit, RandomAccess: true, Range: lim}})
	}
	for _, t := range p.Toggles {
		if !hasToggle(&t, &desc) {
			continue
		}
		dev.Capabilities = append(dev.Capabilities, alice.Capability{Type: alice.CapToggle, Retrievable: true, Reportable: true,
			Parameters: alice.InstanceParams{Instance: t.Instance}})
	}
	if c, ok := colorParams(p.Color, &desc); ok {
		dev.Capabilities = append(dev.Capabilities, alice.Capability{Type: alice.CapColor, Retrievable: true, Reportable: true,
			Parameters: c})
	}
	for _, pr := range p.Properties {
		if !desc.Knows(pr.Status) {
			continue
		}
		dev.Properties = append(dev.Properties, propertyInfo(&pr))
	}
	out := []alice.DeviceInfo{dev}
	if p.HasEvents() {
		sensor := alice.DeviceInfo{
			ID: d.ID + eventsSuffix, Name: d.Name + " триггер", Room: d.Room, Type: alice.TypeButton,
			DeviceInfo: alice.Manufacturer{Manufacturer: d.manufacturer(), Model: desc.Type},
		}
		if values := buttonValues(p.Events); len(values) > 0 {
			sensor.Properties = append(sensor.Properties, alice.Capability{Type: alice.PropEvent, Reportable: true,
				Parameters: alice.EventParams{Instance: alice.InstanceButton, Events: alice.Items(values)}})
		}
		for _, l := range levels(p.Events, &desc) {
			values := []string{"low", "normal"}
			if slices.ContainsFunc(l.keys, func(k string) bool { return slices.Contains(p.Events.Empty, k) }) {
				values = []string{"empty", "low", "normal"}
			}
			sensor.Properties = append(sensor.Properties, alice.Capability{Type: alice.PropEvent, Retrievable: true, Reportable: true,
				Parameters: alice.EventParams{Instance: l.instance, Events: alice.Items(values)}})
		}
		out = append(out, sensor)
	}
	return out
}

// level - свойство уровня датчика и события «заканчивается», которые его задают.
type level struct {
	instance string
	keys     []string
}

// levels - уровни этого прибора: сухое и жидкости.
func levels(e *profile.Events, desc *appliance.Description) []level {
	var out []level
	if keys := knownKeys(e.FoodLevel, e.FoodLevelIfKnown, desc); len(keys) > 0 {
		out = append(out, level{"food_level", keys})
	}
	if keys := knownKeys(e.WaterLevel, e.WaterLevelIfKnown, desc); len(keys) > 0 {
		out = append(out, level{"water_level", keys})
	}
	return out
}

// knownKeys - события «заканчивается» этого прибора: общие для типа и те, что прибор описывает сам.
func knownKeys(always, ifKnown []string, desc *appliance.Description) []string {
	out := always
	for _, k := range ifKnown {
		if slices.Contains(desc.Events, k) {
			out = append(slices.Clip(out), k)
		}
	}
	return out
}

func buttonValues(e *profile.Events) []string {
	var out []string
	for _, v := range e.Button {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

func propertyInfo(pr *profile.Property) alice.Capability {
	if pr.Type == "float" {
		return alice.Capability{Type: alice.PropFloat, Retrievable: true, Reportable: true,
			Parameters: alice.FloatParams{Instance: pr.Instance, Unit: pr.Unit}}
	}
	values := slices.Sorted(maps.Values(pr.Values))
	values = slices.Compact(values)
	return alice.Capability{Type: alice.PropEvent, Retrievable: pr.Instance != alice.InstanceButton, Reportable: true,
		Parameters: alice.EventParams{Instance: pr.Instance, Events: alice.Items(values)}}
}

// modeValues - режимы Алисы, которые есть у прибора, в постоянном порядке:
// Яндекс требует, чтобы порядок не менялся между запросами.
func (d *Device) modeValues(m *profile.Mode, desc *appliance.Description) []string {
	src := m.Programs
	if src == nil {
		src = m.Values
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(src)) {
		if m.Programs != nil && !desc.HasProgram(key) {
			continue
		}
		if m.Programs == nil {
			opt, ok := desc.Options[m.Option]
			if !ok || !slices.Contains(opt.Values, key) {
				continue
			}
		}
		out = append(out, src[key])
	}
	return out
}

// rangeLimits берёт пределы из профиля, а недостающие - из описания прибора.
func rangeLimits(r *profile.Range, desc *appliance.Description) (alice.RangeLimit, bool) {
	opt, known := desc.Options[r.Option]
	lim := alice.RangeLimit{Min: opt.Min, Max: opt.Max, Precision: opt.Step}
	if r.Min != nil {
		lim.Min = *r.Min
	}
	if r.Max != nil {
		lim.Max = *r.Max
	}
	if r.Precision != nil {
		lim.Precision = *r.Precision
	}
	if lim.Precision <= 0 {
		lim.Precision = 1
	}
	return lim, (known || (r.Min != nil && r.Max != nil)) && lim.Min < lim.Max
}

// States - состояния прибора и его датчика событий.
func (d *Device) States() []alice.DeviceState {
	st := d.Appliance.State()
	if !st.Online {
		out := []alice.DeviceState{{ID: d.ID, ErrorCode: alice.CodeUnreachable}}
		if d.Profile.HasEvents() {
			out = append(out, alice.DeviceState{ID: d.ID + eventsSuffix, ErrorCode: alice.CodeUnreachable})
		}
		return out
	}
	out := []alice.DeviceState{d.mainState(&st)}
	if d.Profile.HasEvents() {
		out = append(out, d.sensorState(&st))
	}
	return out
}

func (d *Device) mainState(st *appliance.State) alice.DeviceState {
	p := d.Profile
	desc := d.Appliance.Describe()
	ds := alice.DeviceState{ID: d.ID}
	add := func(t, instance string, v any) {
		ds.Capabilities = append(ds.Capabilities, alice.CapState{Type: t, State: alice.StateValue{Instance: instance, Value: v}})
	}
	d.mu.Lock()
	pending := maps.Clone(d.pending)
	d.mu.Unlock()

	switch p.OnOff {
	case "program":
		add(alice.CapOnOff, "on", st.Running())
	case "power":
		if p.Power != nil {
			add(alice.CapOnOff, "on", powered(p.Power, st))
		} else {
			add(alice.CapOnOff, "on", st.Values[appliance.SettingPowerState] == appliance.PowerOn)
		}
	}
	for _, m := range p.Modes {
		if len(d.modeValues(&m, &desc)) == 0 {
			continue
		}
		var key string
		if m.Programs != nil {
			key = st.ActiveProgram
			if key == "" {
				key = st.SelectedProgram
			}
			if v, ok := m.Programs[key]; ok {
				add(alice.CapMode, m.Instance, v)
			}
			continue
		}
		key = valueString(st.Values[m.Option])
		if pv, ok := pending[m.Option].(string); ok && !st.Running() {
			key = pv
		}
		if v, ok := m.Values[key]; ok {
			add(alice.CapMode, m.Instance, v)
		}
	}
	for _, r := range p.Ranges {
		if _, ok := rangeLimits(&r, &desc); !ok {
			continue
		}
		if v, ok := d.rangeValue(&r, &desc, st, pending); ok {
			add(alice.CapRange, r.Instance, v)
		}
	}
	for _, t := range p.Toggles {
		if !hasToggle(&t, &desc) {
			continue
		}
		if t.Instance == "pause" && t.Setting == "" {
			add(alice.CapToggle, "pause", st.Values[appliance.StatusOperationState] == appliance.OperationPause)
			continue
		}
		if v, ok := toggleState(&t, st); ok {
			add(alice.CapToggle, t.Instance, v)
		}
	}
	if v, ok := d.colorState(st, &desc); ok {
		add(alice.CapColor, v.Instance, v.Value)
	}
	for _, pr := range p.Properties {
		if pr.Type == "float" && pr.Instance == RemainingTime {
			if v, ok := Remaining(p, st); ok {
				ds.Properties = append(ds.Properties, alice.CapState{Type: alice.PropFloat,
					State: alice.StateValue{Instance: RemainingTime, Value: d.reported(v)}})
			}
			continue
		}
		raw, ok := profile.StatusValue(st.Values, pr.Status)
		if !ok {
			continue
		}
		var v any
		switch pr.Type {
		case "float":
			f, ok := raw.(float64)
			if !ok {
				continue
			}
			v = pr.Value(f)
		default:
			m, ok := pr.Values[valueString(raw)]
			if !ok {
				continue
			}
			v = m
		}
		ds.Properties = append(ds.Properties, alice.CapState{Type: typeOf(&pr), State: alice.StateValue{Instance: pr.Instance, Value: v}})
	}
	return ds
}

func typeOf(pr *profile.Property) string {
	if pr.Type == "float" {
		return alice.PropFloat
	}
	return alice.PropEvent
}

// rangeValue - значение для Алисы: у работающей программы - текущее,
// иначе заданное голосом для следующего запуска или значение по умолчанию.
func (d *Device) rangeValue(r *profile.Range, desc *appliance.Description, st *appliance.State, pending map[string]any) (float64, bool) {
	if isSetting(desc, r.Option) || st.Running() {
		v, ok := st.Values[r.Option].(float64)
		return v, ok
	}
	if v, ok := pending[r.Option].(float64); ok {
		return v, true
	}
	if v, ok := d.Profile.DefaultOptions[r.Option].(float64); ok {
		return v, true
	}
	v, ok := st.Values[r.Option].(float64)
	return v, ok
}

// sensorState - состояние датчика событий: последнее нажатие и уровни.
func (d *Device) sensorState(st *appliance.State) alice.DeviceState {
	ds := alice.DeviceState{ID: d.ID + eventsSuffix}
	e := d.Profile.Events
	d.mu.Lock()
	button := d.button
	d.mu.Unlock()
	if button != "" {
		ds.Properties = append(ds.Properties, alice.CapState{Type: alice.PropEvent, State: alice.StateValue{Instance: alice.InstanceButton, Value: button}})
	}
	desc := d.Appliance.Describe()
	for _, l := range levels(e, &desc) {
		value := "normal"
		for _, key := range l.keys {
			if st.Values[key] != appliance.EventPresent {
				continue
			}
			if slices.Contains(e.Empty, key) {
				value = "empty"
				break
			}
			value = "low"
		}
		ds.Properties = append(ds.Properties, alice.CapState{Type: alice.PropEvent, State: alice.StateValue{Instance: l.instance, Value: value}})
	}
	return ds
}

// buttonFor переводит события прибора в событие датчика и запоминает его.
func (d *Device) buttonFor(events []string) []string {
	if !d.Profile.HasEvents() {
		return nil
	}
	var out []string
	for _, e := range events {
		if v, ok := d.Profile.Events.Match(e); ok {
			out = append(out, v)
		}
	}
	if len(out) > 0 {
		d.mu.Lock()
		d.button = out[len(out)-1]
		d.mu.Unlock()
	}
	return out
}

// Act выполняет одну команду Алисы и возвращает код ошибки Яндекса или пустую строку.
// У датчика событий умений нет.
func (d *Device) Act(ctx context.Context, id string, a *alice.Action) string {
	if id != d.ID {
		return alice.CodeInvalidAction
	}
	d.actMu.Lock()
	defer d.actMu.Unlock()
	p := d.Profile
	desc := d.Appliance.Describe()
	st := d.Appliance.State()
	if !st.Online {
		return alice.CodeUnreachable
	}
	switch a.Type {
	case alice.CapOnOff:
		on, ok := a.State.Value.(bool)
		if !ok || a.State.Instance != "on" {
			return alice.CodeInvalidValue
		}
		switch p.OnOff {
		case "program":
			if on == st.Running() {
				return ""
			}
			if !on {
				return code(d.Appliance.Stop(ctx))
			}
			program := st.SelectedProgram
			if pm := p.ProgramMode(); pm == nil || pm.Programs[program] == "" {
				program = p.DefaultProgram
			}
			if program == "" {
				return alice.CodeWrongMode
			}
			return d.start(ctx, &st, program)
		case "power":
			pw := p.Power
			if pw == nil {
				return code(d.Appliance.Set(ctx, appliance.SettingPowerState, powerValue(on, &desc)))
			}
			if on == powered(pw, &st) {
				if on && p.DefaultProgram != "" {
					// «Включи» включённой кофемашины с default_program - напиток.
					return d.start(ctx, &st, p.DefaultProgram)
				}
				return ""
			}
			if on && pw.Allowed != "" && st.Values[pw.Allowed] == false {
				return alice.CodeRemoteDisabled
			}
			value := pw.Off
			if on {
				value = pw.On
			}
			return code(d.Appliance.Set(ctx, pw.Setting, value))
		}
	case alice.CapMode:
		v, ok := a.State.Value.(string)
		if !ok {
			return alice.CodeInvalidValue
		}
		m := findMode(p.Modes, a.State.Instance)
		if m == nil {
			return alice.CodeInvalidAction
		}
		if m.Programs != nil {
			program := keyFor(m.Programs, v)
			if program == "" || !desc.HasProgram(program) {
				return alice.CodeInvalidValue
			}
			if st.Running() {
				if st.ActiveProgram == program {
					return ""
				}
				if err := d.Appliance.Stop(ctx); err != nil {
					return code(err)
				}
				return d.start(ctx, &st, program)
			}
			if p.OnOff != "program" {
				// Без запуска по «включи» выбор режима сразу запускает программу, например напиток.
				return d.start(ctx, &st, program)
			}
			return code(d.Appliance.Select(ctx, program))
		}
		value := keyFor(m.Values, v)
		if value == "" {
			return alice.CodeInvalidValue
		}
		return d.setOption(ctx, &st, m.Option, value)
	case alice.CapRange:
		r := findRange(p.Ranges, a.State.Instance)
		if r == nil {
			return alice.CodeInvalidAction
		}
		lim, ok := rangeLimits(r, &desc)
		f, isNum := a.State.Value.(float64)
		if !ok || !isNum || math.IsNaN(f) || math.IsInf(f, 0) {
			return alice.CodeInvalidValue
		}
		if a.State.Relative {
			d.mu.Lock()
			cur, known := d.rangeValue(r, &desc, &st, d.pending)
			d.mu.Unlock()
			if s, ok := d.Appliance.(appliance.Stepper); ok && !known {
				// Текущего значения прибор не сообщает: шаг уходит ему как есть.
				return code(s.Step(ctx, r.Option, f))
			}
			f += cur
		}
		if f < lim.Min || f > lim.Max {
			return alice.CodeInvalidValue
		}
		return d.setOption(ctx, &st, r.Option, f)
	case alice.CapToggle:
		on, ok := a.State.Value.(bool)
		if !ok {
			return alice.CodeInvalidValue
		}
		t := findToggle(p.Toggles, a.State.Instance)
		if t == nil {
			return alice.CodeInvalidAction
		}
		if t.Instance != "pause" || t.Setting != "" {
			return code(d.Appliance.Set(ctx, t.Setting, toggleValue(t, on)))
		}
		if !st.Running() {
			return alice.CodeWrongMode
		}
		paused := st.Values[appliance.StatusOperationState] == appliance.OperationPause
		if on == paused {
			return ""
		}
		cmd := appliance.CommandResume
		if on {
			cmd = appliance.CommandPause
		}
		return code(d.Appliance.Command(ctx, cmd))
	case alice.CapColor:
		return d.setColor(ctx, &desc, &a.State)
	}
	return alice.CodeInvalidAction
}

// start запускает программу с параметрами по умолчанию и заданными голосом.
// Условия удалённого старта проверяются до команды, чтобы Алиса сразу назвала причину.
func (d *Device) start(ctx context.Context, st *appliance.State, program string) string {
	if v, ok := st.Values[appliance.StatusRemoteStartAllowed].(bool); ok && !v {
		return alice.CodeRemoteDisabled
	}
	if st.Values[appliance.StatusDoorState] == appliance.DoorOpen {
		return alice.CodeDoorOpen
	}
	desc := d.Appliance.Describe()
	options := map[string]any{}
	for k, v := range d.Profile.DefaultOptions {
		if _, ok := desc.Options[k]; ok {
			options[k] = v
		}
	}
	d.mu.Lock()
	maps.Copy(options, d.pending)
	d.mu.Unlock()
	if err := d.Appliance.Start(ctx, program, options); err != nil {
		return code(err)
	}
	d.mu.Lock()
	d.pending = nil
	d.mu.Unlock()
	return ""
}

// setOption меняет параметр у работающей программы или настройку,
// а без программы запоминает параметр для следующего запуска.
func (d *Device) setOption(ctx context.Context, st *appliance.State, key string, value any) string {
	desc := d.Appliance.Describe()
	if isSetting(&desc, key) || st.Running() {
		return code(d.Appliance.Set(ctx, key, value))
	}
	if _, ok := desc.Options[key]; !ok {
		return alice.CodeInvalidAction
	}
	d.mu.Lock()
	if d.pending == nil {
		d.pending = map[string]any{}
	}
	d.pending[key] = value
	d.mu.Unlock()
	return ""
}

func powerValue(on bool, desc *appliance.Description) string {
	if on {
		return appliance.PowerOn
	}
	if slices.Contains(desc.Options[appliance.SettingPowerState].Values, appliance.PowerOff) {
		return appliance.PowerOff
	}
	return appliance.PowerStandby
}

func keyFor(m map[string]string, value string) string {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if m[k] == value {
			return k
		}
	}
	return ""
}

func findMode(list []profile.Mode, instance string) *profile.Mode {
	for i := range list {
		if list[i].Instance == instance {
			return &list[i]
		}
	}
	return nil
}

func findRange(list []profile.Range, instance string) *profile.Range {
	for i := range list {
		if list[i].Instance == instance {
			return &list[i]
		}
	}
	return nil
}

func findToggle(list []profile.Toggle, instance string) *profile.Toggle {
	for i := range list {
		if list[i].Instance == instance {
			return &list[i]
		}
	}
	return nil
}

// code переводит ошибку прибора в код ошибки Яндекса.
func code(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, appliance.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		return alice.CodeUnreachable
	case errors.Is(err, appliance.ErrRemoteDisabled):
		return alice.CodeRemoteDisabled
	case errors.Is(err, appliance.ErrDoorOpen):
		return alice.CodeDoorOpen
	case errors.Is(err, appliance.ErrBusy):
		return alice.CodeBusy
	case errors.Is(err, appliance.ErrInvalidValue):
		return alice.CodeInvalidValue
	case errors.Is(err, appliance.ErrNotSupported):
		return alice.CodeInvalidAction
	case errors.Is(err, appliance.ErrWrongMode):
		return alice.CodeWrongMode
	}
	return alice.CodeInternal
}
