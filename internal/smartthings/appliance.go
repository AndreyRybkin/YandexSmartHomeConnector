package smartthings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"hcgateway/internal/appliance"
)

var _ appliance.Appliance = (*Appliance)(nil)
var _ appliance.Stepper = (*Appliance)(nil)

// Appliance - компонент устройства SmartThings как прибор по профилю. Имя функции -
// «capability.атрибут» или «capability.команда» компонента; у компонента не main - ещё
// capability main, которых у него нет.
type Appliance struct {
	cloud     *Cloud
	deviceID  string
	component string

	mu     sync.Mutex
	values map[string]any
	// units - единица атрибута, например F у температуры: по ней шлюз пересчитывает уставку.
	units map[string]string
	// lists - списки supported* по capability и атрибуту: значения, которые принимает устройство.
	lists map[string]map[string][]string
	// limits - пределы уставок в °C, которые сообщает устройство, по имени уставки.
	limits map[string]limit
	// reported - поля атрибутов-объектов, которые устройство сообщило с прошлой загрузки состояния. Поле
	// не пропадает до новой загрузки, чтобы редкое значение без него не меняло список устройств Алисы.
	reported map[string]bool
	// disabled - отключённые capability по компонентам, как их сообщает устройство.
	disabled map[string][]string
	have     bool
	notify   appliance.Notify
	runs     int
}

// DeviceID и Component - устройство и компонент прибора.
func (a *Appliance) DeviceID() string  { return a.deviceID }
func (a *Appliance) Component() string { return a.component }

// Manufacturer - производитель устройства из SmartThings; пусто - не известен.
func (a *Appliance) Manufacturer() string {
	if d := a.cloud.device(a.deviceID); d != nil {
		return d.Manufacturer
	}
	return ""
}

// stepCommands - команды шага по атрибутам: так «громче» и «следующий канал» работают и без
// текущего значения.
var stepCommands = map[string][2]string{
	"audioVolume.volume":  {"volumeUp", "volumeDown"},
	"tvChannel.tvChannel": {"channelUp", "channelDown"},
}

// Функции, о которых шлюз знает особо.
const (
	capDisabledCapabilities = "custom.disabledCapabilities"
	capDisabledComponents   = "custom.disabledComponents"
	capSetpointControl      = "custom.thermostatSetpointControl"

	keyTemperature  = "temperatureMeasurement.temperature"
	keyCooling      = "thermostatCoolingSetpoint.coolingSetpoint"
	keyHeating      = "thermostatHeatingSetpoint.heatingSetpoint"
	keyOven         = "ovenSetpoint.ovenSetpoint"
	keyMinimum      = capSetpointControl + ".minimumSetpoint"
	keyMaximum      = capSetpointControl + ".maximumSetpoint"
	keyOscillation  = "fanOscillationMode.fanOscillationMode"
	keyThermostat   = "thermostatMode.thermostatMode"
	keyShade        = "windowShade.windowShade"
	shadeCommands   = "supportedWindowShadeCommands"
	shadePause      = "pause"
	noValues        = "hcgateway.noValues"
	thermostatList  = "supportedThermostatModes"
	categoryAC      = "AirConditioner"
	categoryHeating = "Thermostat"

	// ThermostatSetpoint - уставка термостата по его режиму: при cool - охлаждения, при остальных -
	// нагрева. Такого атрибута у устройства нет, значение считает шлюз.
	ThermostatSetpoint = "hcgateway.thermostatSetpoint"
)

// setpointRanges - атрибут с пределами уставки и сама уставка.
var setpointRanges = map[string]string{
	"thermostatCoolingSetpoint.coolingSetpointRange": keyCooling,
	"thermostatHeatingSetpoint.heatingSetpointRange": keyHeating,
	"ovenSetpoint.ovenSetpointRange":                 keyOven,
}

// Пределы уставки, если устройство их не сообщает. Команду вне настоящего диапазона отклонит само
// устройство. У холодильника запасных нет: неверная уставка испортит продукты.
var (
	acFallback         = limit{Min: 16, Max: 30, Step: 1}
	thermostatFallback = limit{Min: 5, Max: 35, Step: 1}
	ovenFallback       = limit{Min: 30, Max: 300, Step: 5}
)

// thermostatOn - какой режим включает термостат без режима heat.
var thermostatOn = []string{"auto", "cool", "eco"}

// maxSteps - самый длинный список целых значений, который шлюз строит из пределов атрибута:
// так скорость вентилятора 1-4 становится режимами.
const maxSteps = 10

// Describe - что умеет прибор по описаниям его capability. Без устройства в списке аккаунта
// или без описания хотя бы одной capability типа нет: прибор ждёт данных облака и не
// обещает Алисе того, чего у него нет.
func (a *Appliance) Describe() appliance.Description {
	waiting := appliance.Description{Options: map[string]appliance.Option{}}
	d := a.cloud.device(a.deviceID)
	if d == nil || d.componentOff(a.component) {
		return waiting
	}
	refs := d.capabilities(a.component)
	defs := make([]*CapDef, len(refs))
	for i, ref := range refs {
		if defs[i] = a.cloud.def(ref); defs[i] == nil {
			signal(a.cloud.fetch)
			return waiting
		}
	}
	desc := appliance.Description{Type: d.Type(a.component), Options: map[string]appliance.Option{}}
	a.mu.Lock()
	lists := maps.Clone(a.lists)
	reported := maps.Clone(a.reported)
	off := a.offLocked(d)
	limits := a.limitsLocked(d, off)
	a.mu.Unlock()
	for i, ref := range refs {
		if off[ref.ID] {
			continue
		}
		def := defs[i]
		writable := 0
		for _, ad := range def.Attributes {
			if ad.writable() {
				writable++
			}
		}
		for _, name := range slices.Sorted(maps.Keys(def.Attributes)) {
			ad := def.Attributes[name]
			key := Key(ref.ID, name)
			desc.Reports = append(desc.Reports, key)
			for _, f := range objectFields[key] {
				if reported[Key(key, f.Name)] {
					desc.Reports = append(desc.Reports, Key(key, f.Name))
				}
			}
			if !ad.writable() {
				continue
			}
			s := ad.Schema.Properties.Value
			opt := appliance.Option{Key: key, Setting: true, Values: slices.Clone(s.Enum)}
			if s.boolEnum() {
				opt.Values = nil
			}
			supported := supportedList(lists[ref.ID], name, writable)
			if supported != nil {
				opt.Values = supported
			}
			if key == keyOscillation && supported == nil {
				// В схеме - все положения, какие бывают: без списка устройства положений не обещаем.
				opt.Values = nil
			}
			if s.Minimum != nil && s.Maximum != nil {
				opt.Min, opt.Max = *s.Minimum, *s.Maximum
			}
			if s.Type == "integer" {
				opt.Step = 1
				if len(opt.Values) == 0 && s.Minimum != nil && s.Maximum != nil && *s.Maximum-*s.Minimum < maxSteps {
					for v := *s.Minimum; v <= *s.Maximum; v++ {
						opt.Values = append(opt.Values, strconv.FormatFloat(v, 'f', -1, 64))
					}
				}
			}
			if isSetpoint(key) {
				// В схеме пределов уставки нет настоящих, например -460..10000: только данные устройства.
				l := limits[key]
				opt.Min, opt.Max, opt.Step = l.Min, l.Max, l.Step
			}
			desc.Options[key] = opt
		}
		for _, name := range slices.Sorted(maps.Keys(def.Commands)) {
			cmd := def.Commands[name]
			key := Key(ref.ID, name)
			if _, isAttr := def.Attributes[name]; isAttr {
				continue
			}
			switch cmd.required() {
			case 0:
				desc.Commands = append(desc.Commands, key)
			case 1:
				// Команда с одним аргументом - настройка, например setColor с цветом.
				desc.Options[key] = appliance.Option{Key: key, Setting: true}
				desc.Reports = append(desc.Reports, key)
			}
		}
	}
	if d.Category(a.component) == categoryHeating && (slices.ContainsFunc(refs, enabledCap(off, keyHeating)) || slices.ContainsFunc(refs, enabledCap(off, keyCooling))) {
		l := widest(limits[keyHeating], limits[keyCooling])
		desc.Options[ThermostatSetpoint] = appliance.Option{Key: ThermostatSetpoint, Setting: true, Min: l.Min, Max: l.Max, Step: l.Step}
		desc.Reports = append(desc.Reports, ThermostatSetpoint)
	}
	// Штора принимает паузу командой: настройка есть, только если устройство назвало её в своём списке команд.
	delete(desc.Options, keyShade)
	if cmds := lists["windowShade"][shadeCommands]; slices.ContainsFunc(refs, enabledCap(off, keyShade)) && slices.Contains(cmds, shadePause) {
		desc.Options[keyShade] = appliance.Option{Key: keyShade, Setting: true, Values: slices.Clone(cmds)}
	}
	if len(desc.Reports) == 0 {
		// Пустой список значил бы «прибор может сообщать всё»: устройство только с командами получило бы все
		// свойства профиля.
		desc.Reports = []string{noValues}
	}
	return desc
}

// isSetpoint сообщает, что key - уставка температуры.
func isSetpoint(key string) bool {
	return key == keyCooling || key == keyHeating || key == keyOven
}

// enabledCap - проверка, что ссылка - включённая capability функции key.
func enabledCap(off map[string]bool, key string) func(CapRef) bool {
	capability, _, _ := SplitKey(key)
	return func(r CapRef) bool { return r.ID == capability && !off[r.ID] }
}

// widest - пределы, в которые входят оба набора: термостат сменит режим, а список умений Алисы
// от этого меняться не должен. Точные пределы текущей уставки проверяет Set.
func widest(x, y limit) limit {
	switch {
	case !x.known():
		return y
	case !y.known():
		return x
	}
	return limit{Min: min(x.Min, y.Min), Max: max(x.Max, y.Max), Step: min(x.Step, y.Step)}
}

// offLocked - отключённые capability прибора, с учётом того, чей компонент их даёт. Устройство
// часто отключает то, что на самом деле работает: эти исключения взяты из Home Assistant. a.mu захвачен.
func (a *Appliance) offLocked(d *Device) map[string]bool {
	off := map[string]bool{}
	if d == nil {
		return off
	}
	for _, ref := range d.capabilities(a.component) {
		if slices.Contains(a.disabled[d.owner(a.component, ref.ID)], ref.ID) && !a.keptLocked(ref.ID) {
			off[ref.ID] = true
		}
	}
	return off
}

// keptLocked сообщает, что отключённая capability на деле работает: без этого стиральная машина
// потеряла бы питание. a.mu захвачен.
func (a *Appliance) keptLocked(capability string) bool {
	switch capability {
	case "washerOperatingState", "dryerOperatingState":
		return len(a.lists[capability]["supportedMachineStates"]) > 0
	case "samsungce.airConditionerLighting":
		_, ok := a.values[Key(capability, "lighting")]
		return ok
	case "samsungce.airConditionerBeep":
		_, ok := a.values[Key(capability, "beep")]
		return ok
	}
	return false
}

// limitsLocked - пределы уставок в °C: диапазон уставки, у охлаждения затем
// custom.thermostatSetpointControl, у кондиционера, термостата и духовки затем запасные. Другой
// уставке без данных пределов нет, и в Алисе её нет: «поставь 100 градусов» не должно проходить.
// a.mu захвачен.
func (a *Appliance) limitsLocked(d *Device, off map[string]bool) map[string]limit {
	out := maps.Clone(a.limits)
	if _, ok := out[keyCooling]; !ok && !off[capSetpointControl] {
		lo, okLo := a.values[keyMinimum].(float64)
		hi, okHi := a.values[keyMaximum].(float64)
		if l := (limit{Min: lo, Max: hi}); okLo && okHi && l.known() {
			out[keyCooling] = l
		}
	}
	if d == nil {
		return out
	}
	if _, ok := out[keyCooling]; !ok && d.Category(a.component) == categoryAC {
		out[keyCooling] = acFallback
	}
	refs := d.capabilities(a.component)
	fallback := func(key string, l limit) {
		if _, ok := out[key]; !ok && slices.ContainsFunc(refs, enabledCap(off, key)) {
			out[key] = l
		}
	}
	if d.Category(a.component) == categoryHeating {
		fallback(keyHeating, thermostatFallback)
		fallback(keyCooling, thermostatFallback)
	}
	fallback(keyOven, ovenFallback)
	return out
}

// shapeLocked - то, от чего зависит описание прибора, кроме описаний capability. a.mu захвачен.
func (a *Appliance) shapeLocked(d *Device) string {
	off := a.offLocked(d)
	data, _ := json.Marshal(map[string]any{"lists": a.lists, "off": off, "limits": a.limitsLocked(d, off), "fields": a.reported})
	return string(data)
}

// supportedList - значения атрибута name из списка supported* его capability: устройство
// принимает только их. Подходит список, в имени которого есть имя атрибута, а у capability
// с одним записываемым атрибутом - любой список.
func supportedList(lists map[string][]string, name string, writable int) []string {
	if len(lists) == 0 {
		return nil
	}
	for _, k := range slices.Sorted(maps.Keys(lists)) {
		if strings.Contains(strings.ToLower(k), strings.ToLower(name)) {
			return slices.Clone(lists[k])
		}
	}
	if writable == 1 {
		k := slices.Sorted(maps.Keys(lists))[0]
		return slices.Clone(lists[k])
	}
	return nil
}

// State - состояние прибора. Прибор на связи, пока открыт поток событий, получено состояние и
// SmartThings не сообщил, что устройство не на связи.
func (a *Appliance) State() appliance.State {
	connected := a.cloud.Connected()
	online := a.cloud.online(a.deviceID)
	d := a.cloud.device(a.deviceID)
	a.mu.Lock()
	defer a.mu.Unlock()
	off := a.offLocked(d)
	values := maps.Clone(a.values)
	// Нажатие кнопки приходит только событием: значение в состоянии hcdevice принял бы за новое нажатие
	// после загрузки состояния или события без stateChange.
	delete(values, buttonKey)
	maps.DeleteFunc(values, func(key string, _ any) bool {
		capability, _, ok := SplitKey(key)
		if _, field := objectFields[capability]; field {
			capability, _, ok = SplitKey(capability)
		}
		return ok && off[capability]
	})
	if key := a.setpointLocked(d, off); key != "" {
		if v, ok := values[key]; ok {
			values[ThermostatSetpoint] = v
		}
	}
	return appliance.State{Online: connected && a.have && online, Values: values}
}

// setpointLocked - уставка термостата по его режиму; пусто - прибор не термостат или уставок нет.
// a.mu захвачен.
func (a *Appliance) setpointLocked(d *Device, off map[string]bool) string {
	if d == nil || d.Category(a.component) != categoryHeating {
		return ""
	}
	refs := d.capabilities(a.component)
	heating := slices.ContainsFunc(refs, enabledCap(off, keyHeating))
	cooling := slices.ContainsFunc(refs, enabledCap(off, keyCooling))
	switch {
	case cooling && (a.values[keyThermostat] == "cool" || !heating):
		return keyCooling
	case heating:
		return keyHeating
	}
	return ""
}

// Run подписывает прибор на состояние из облака: поток событий держит Cloud, один на аккаунт.
func (a *Appliance) Run(ctx context.Context, notify appliance.Notify) {
	a.mu.Lock()
	a.runs++
	gen := a.runs
	a.notify = notify
	a.mu.Unlock()
	a.cloud.attach(a)
	<-ctx.Done()
	a.cloud.detach(a)
	a.mu.Lock()
	if a.runs == gen {
		a.notify = nil
	}
	a.mu.Unlock()
}

func (a *Appliance) changed(events []string) {
	a.mu.Lock()
	notify := a.notify
	a.mu.Unlock()
	if notify != nil {
		notify(events)
	}
}

// mine сообщает, относится ли атрибут компонента к прибору: свой компонент, а у прибора не
// main - ещё capability main, которых у его компонента нет.
func (a *Appliance) mine(d *Device, component, capability string) bool {
	if d == nil {
		return component == a.component
	}
	return d.owner(a.component, capability) == component
}

// schema - схема значения атрибута или nil, если описание capability не загружено.
func (a *Appliance) schema(d *Device, capability, attribute string) *Schema {
	if d == nil {
		return nil
	}
	for _, ref := range d.capabilities(a.component) {
		if ref.ID != capability {
			continue
		}
		if def := a.cloud.def(ref); def != nil {
			if ad, ok := def.Attributes[attribute]; ok {
				s := ad.Schema.Properties.Value
				return &s
			}
		}
	}
	return nil
}

// setLocked запоминает значение атрибута; a.mu захвачен.
func (a *Appliance) setLocked(d *Device, capability, attribute string, v AttrValue) {
	key := Key(capability, attribute)
	if capability == capDisabledCapabilities && attribute == "disabledCapabilities" {
		return
	}
	if strings.HasPrefix(attribute, "supported") {
		list := listValues(v)
		switch {
		case list != nil && a.lists[capability] == nil:
			a.lists[capability] = map[string][]string{attribute: list}
		case list != nil:
			a.lists[capability][attribute] = list
		default:
			delete(a.lists[capability], attribute)
			if len(a.lists[capability]) == 0 {
				delete(a.lists, capability)
			}
		}
		return
	}
	if target, ok := setpointRanges[key]; ok {
		if l, ok := setpointRange(v, a.units[keyTemperature]); ok {
			a.limits[target] = l
		} else {
			delete(a.limits, target)
		}
		return
	}
	if fields, ok := objectFields[key]; ok {
		for _, f := range fields {
			delete(a.values, Key(key, f.Name))
		}
		for name, x := range fieldValues(key, v) {
			a.reported[name] = true
			if x != nil {
				a.values[name] = *x
			}
		}
		return
	}
	// Уставка духовки и пределы приходят без единицы: она та же, что у температуры в камере.
	if v.Unit == "" && (key == keyOven || key == keyMinimum || key == keyMaximum) {
		v.Unit = a.units[keyTemperature]
	}
	if key == keyOven && ovenIdle(v) {
		delete(a.values, key)
		return
	}
	x, ok := scalar(v, a.schema(d, capability, attribute))
	if !ok {
		delete(a.values, key)
		return
	}
	if s, isStr := x.(string); isStr && integerRe.MatchString(s) {
		// Номер строкой, например канал телевизора: шлюз хранит его числом.
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			x = f
		}
	}
	a.values[key] = x
	a.units[key] = v.Unit
}

const (
	// buttonKey - значение кнопки SmartThings.
	buttonKey = "button.button"
	// maxButtonValue - предел значения кнопки: оно попадает в метку метрики событий.
	maxButtonValue = 32
)

// integerRe - целое число строкой.
var integerRe = regexp.MustCompile(`^-?[0-9]{1,9}$`)

// ovenIdle сообщает, что значение уставки духовки значит «уставки нет»: в простое духовка пишет -17,
// 0 или 1.
func ovenIdle(v AttrValue) bool {
	var f float64
	return json.Unmarshal(v.Value, &f) == nil && (f == -17 || f == 0 || f == 1)
}

// load применяет всё состояние устройства.
func (a *Appliance) load(st Status) {
	d := a.cloud.device(a.deviceID)
	a.mu.Lock()
	before := a.shapeLocked(d)
	a.values = map[string]any{}
	a.lists = map[string]map[string][]string{}
	a.limits = map[string]limit{}
	a.reported = map[string]bool{}
	a.disabled = map[string][]string{}
	for comp, caps := range st {
		if v, ok := caps[capDisabledCapabilities]["disabledCapabilities"]; ok {
			a.disabled[comp] = listValues(v)
		}
	}
	// Сначала температура: по её единице шлюз понимает значения без единицы.
	for _, temperature := range []bool{true, false} {
		for comp, caps := range st {
			for capability, attrs := range caps {
				if (capability == "temperatureMeasurement") != temperature || !a.mine(d, comp, capability) {
					continue
				}
				for attr, v := range attrs {
					a.setLocked(d, capability, attr, v)
				}
			}
		}
	}
	a.have = true
	reshaped := before != a.shapeLocked(d)
	a.mu.Unlock()
	a.reshaped(reshaped)
	a.changed(nil)
}

// reshaped просит шлюз собрать приборы заново, если изменилось то, что Алиса видит в списке
// устройств. Только при настоящем изменении: каждая сборка - уведомление Яндексу.
func (a *Appliance) reshaped(changed bool) {
	if changed {
		signal(a.cloud.changes)
	}
}

// apply применяет событие: новое значение атрибута.
func (a *Appliance) apply(component, capability, attribute string, v AttrValue) {
	d := a.cloud.device(a.deviceID)
	a.mu.Lock()
	if !a.have {
		a.mu.Unlock()
		return
	}
	before := a.shapeLocked(d)
	if capability == capDisabledCapabilities && attribute == "disabledCapabilities" {
		a.disabled[component] = listValues(v)
	}
	if !a.mine(d, component, capability) {
		reshaped := before != a.shapeLocked(d)
		a.mu.Unlock()
		a.reshaped(reshaped)
		return
	}
	a.setLocked(d, capability, attribute, v)
	var events []string
	// Кнопка сообщает о каждом нажатии, и значение может не измениться: нажатие - отдельное событие.
	// Событие со stateChange false - не нажатие: так драйвер сообщает начальное значение.
	pressed := capability == "button" && attribute == "button" && (v.StateChange == nil || *v.StateChange)
	if s, ok := a.values[buttonKey].(string); ok && pressed && len(s) <= maxButtonValue {
		events = []string{buttonKey + "=" + s}
	}
	reshaped := before != a.shapeLocked(d)
	a.mu.Unlock()
	a.reshaped(reshaped)
	a.changed(events)
}

// target - компонент и описание capability для команды.
func (a *Appliance) target(key string) (string, string, string, *CapDef, error) {
	capability, name, ok := SplitKey(key)
	d := a.cloud.device(a.deviceID)
	if !ok || d == nil {
		return "", "", "", nil, fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
	}
	a.mu.Lock()
	off := a.offLocked(d)[capability]
	a.mu.Unlock()
	for _, ref := range d.capabilities(a.component) {
		if ref.ID == capability && !off {
			if def := a.cloud.def(ref); def != nil {
				return d.owner(a.component, capability), capability, name, def, nil
			}
		}
	}
	return "", "", "", nil, fmt.Errorf("%w: у устройства нет %s", appliance.ErrNotSupported, capability)
}

func (a *Appliance) Select(context.Context, string) error {
	return fmt.Errorf("%w: SmartThings не выбирает программы", appliance.ErrNotSupported)
}

func (a *Appliance) Start(context.Context, string, map[string]any) error {
	return fmt.Errorf("%w: SmartThings не запускает программы", appliance.ErrNotSupported)
}

func (a *Appliance) Stop(context.Context) error {
	return fmt.Errorf("%w: остановка устройства SmartThings - через свойство из power", appliance.ErrNotSupported)
}

// Command выполняет команду без аргументов, например audioVolume.volumeUp.
func (a *Appliance) Command(ctx context.Context, key string) error {
	comp, capability, name, def, err := a.target(key)
	if err != nil {
		return err
	}
	if cmd, ok := def.Commands[name]; !ok || cmd.required() > 0 {
		return fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
	}
	return mapErr(a.cloud.command(ctx, a.deviceID, comp, capability, name, nil))
}

// Set записывает атрибут или выполняет команду с одним аргументом. Атрибут записывается
// командой его значения, например switch on, затем командой записи, например setVolume, а
// у шторы - командой с именем значения, например close.
func (a *Appliance) Set(ctx context.Context, key string, value any) error {
	key, value, err := a.resolve(key, value)
	if err != nil {
		return err
	}
	comp, capability, name, def, err := a.target(key)
	if err != nil {
		return err
	}
	if ad, ok := def.Attributes[name]; ok {
		s := fmt.Sprint(value)
		for _, ec := range ad.EnumCommands {
			if ec.Value == s {
				return a.sent(key, value, a.cloud.command(ctx, a.deviceID, comp, capability, ec.Command, nil))
			}
		}
		if cmd, ok := def.Commands[ad.Setter]; ok && len(cmd.Arguments) > 0 {
			v := a.toDevice(key, value)
			arg, err := argument(v, &cmd.Arguments[0].Schema)
			if err != nil {
				return fmt.Errorf("%w: %s = %v: %w", appliance.ErrInvalidValue, key, value, err)
			}
			return a.sent(key, value, a.cloud.command(ctx, a.deviceID, comp, capability, ad.Setter, []any{arg}))
		}
		if cmd, ok := def.Commands[s]; ok && cmd.required() == 0 {
			return mapErr(a.cloud.command(ctx, a.deviceID, comp, capability, s, nil))
		}
		if !ad.writable() {
			return fmt.Errorf("%w: %s только для чтения", appliance.ErrNotSupported, key)
		}
		return fmt.Errorf("%w: %s = %v", appliance.ErrInvalidValue, key, value)
	}
	cmd, ok := def.Commands[name]
	if !ok {
		return fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
	}
	if len(cmd.Arguments) == 0 {
		return mapErr(a.cloud.command(ctx, a.deviceID, comp, capability, name, nil))
	}
	arg, err := argument(value, &cmd.Arguments[0].Schema)
	if err != nil {
		return fmt.Errorf("%w: %s = %v: %w", appliance.ErrInvalidValue, key, value, err)
	}
	return mapErr(a.cloud.command(ctx, a.deviceID, comp, capability, name, []any{arg}))
}

// resolve - настоящие функция и значение команды: уставка термостата - по его режиму, а «включи»
// термостата без режима heat - режим, который у него есть.
func (a *Appliance) resolve(key string, value any) (string, any, error) {
	switch {
	case key == ThermostatSetpoint:
		d := a.cloud.device(a.deviceID)
		a.mu.Lock()
		off := a.offLocked(d)
		target := a.setpointLocked(d, off)
		l, known := a.limitsLocked(d, off)[target]
		a.mu.Unlock()
		if target == "" {
			return "", nil, fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
		}
		if f, ok := value.(float64); ok && known && (f < l.Min || f > l.Max) {
			return "", nil, fmt.Errorf("%w: %s = %v", appliance.ErrInvalidValue, key, value)
		}
		return target, value, nil
	case key == keyThermostat && value == "heat":
		a.mu.Lock()
		list := slices.Clone(a.lists["thermostatMode"][thermostatList])
		a.mu.Unlock()
		if len(list) == 0 || slices.Contains(list, "heat") {
			return key, value, nil
		}
		for _, mode := range thermostatOn {
			if slices.Contains(list, mode) {
				return key, mode, nil
			}
		}
		if i := slices.IndexFunc(list, func(mode string) bool { return mode != "off" }); i >= 0 {
			return key, list[i], nil
		}
		return "", nil, fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
	}
	return key, value, nil
}

// toDevice - значение в единицах устройства: уставку в °C шлюз пересчитывает в °F, если
// устройство сообщает температуру в °F.
func (a *Appliance) toDevice(key string, v any) any {
	a.mu.Lock()
	unit := a.units[key]
	a.mu.Unlock()
	if f, ok := v.(float64); ok && unit == "F" {
		return math.Round(f*9/5 + 32)
	}
	return v
}

// sent - итог команды записи: при успехе шлюз сразу запоминает новое значение, а SmartThings
// подтверждает его событием.
func (a *Appliance) sent(key string, value any, err error) error {
	if err != nil {
		return mapErr(err)
	}
	a.mu.Lock()
	if a.have {
		a.values[key] = value
	}
	a.mu.Unlock()
	a.changed(nil)
	return nil
}

// Step меняет громкость или канал на шаг командами устройства.
func (a *Appliance) Step(ctx context.Context, key string, delta float64) error {
	cmds, ok := stepCommands[key]
	if !ok || cmds[0] == "" {
		return fmt.Errorf("%w: шаг %s", appliance.ErrNotSupported, key)
	}
	capability, _, _ := SplitKey(key)
	name := cmds[0]
	if delta < 0 {
		name = cmds[1]
	}
	n := min(int(math.Abs(math.Round(delta))), maxStepCommands)
	for range n {
		if err := a.Command(ctx, Key(capability, name)); err != nil {
			return err
		}
	}
	return nil
}

// maxStepCommands - предел команд шага одной командой Алисы: каждая - запрос из квоты.
const maxStepCommands = 10

// mapErr переводит ошибку облака в ошибку прибора: из неё hcdevice получает код Яндекса.
func mapErr(err error) error {
	var ae *APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotConfigured), errors.Is(err, ErrLoginRequired):
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
	case errors.As(err, &ae):
		switch {
		case ae.Status == 400 || ae.Status == 422:
			return fmt.Errorf("%w: %w", appliance.ErrInvalidValue, err)
		case ae.Status == 403 || ae.Status == 404:
			return fmt.Errorf("%w: %w", appliance.ErrNotSupported, err)
		case ae.Status == 409:
			return fmt.Errorf("%w: %w", appliance.ErrWrongMode, err)
		case ae.Status == 429:
			return fmt.Errorf("%w: %w", appliance.ErrBusy, err)
		case ae.Status >= 500:
			return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
		}
		return err
	case errors.Is(err, context.Canceled):
		return err
	}
	return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
}
