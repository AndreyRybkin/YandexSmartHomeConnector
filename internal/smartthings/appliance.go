package smartthings

import (
	"context"
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
	lists  map[string]map[string][]string
	have   bool
	notify appliance.Notify
	runs   int
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

// maxSteps - самый длинный список целых значений, который шлюз строит из пределов атрибута:
// так скорость вентилятора 1-4 становится режимами.
const maxSteps = 10

// Describe - что умеет прибор по описаниям его capability. Без устройства в списке аккаунта
// или без описания хотя бы одной capability типа нет: прибор ждёт данных облака и не
// обещает Алисе того, чего у него нет.
func (a *Appliance) Describe() appliance.Description {
	waiting := appliance.Description{Options: map[string]appliance.Option{}}
	d := a.cloud.device(a.deviceID)
	if d == nil {
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
	a.mu.Unlock()
	for i, ref := range refs {
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
			if !ad.writable() {
				continue
			}
			s := ad.Schema.Properties.Value
			opt := appliance.Option{Key: key, Setting: true, Values: slices.Clone(s.Enum)}
			if s.boolEnum() {
				opt.Values = nil
			}
			if supported := supportedList(lists[ref.ID], name, writable); supported != nil {
				opt.Values = supported
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
	return desc
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
	a.mu.Lock()
	defer a.mu.Unlock()
	return appliance.State{Online: connected && a.have && online, Values: maps.Clone(a.values)}
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

// set запоминает значение атрибута; a.mu захвачен.
func (a *Appliance) setLocked(d *Device, capability, attribute string, v AttrValue) {
	key := Key(capability, attribute)
	if strings.HasPrefix(attribute, "supported") {
		if list := listValues(v); list != nil {
			if a.lists[capability] == nil {
				a.lists[capability] = map[string][]string{}
			}
			a.lists[capability][attribute] = list
		}
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

// integerRe - целое число строкой.
var integerRe = regexp.MustCompile(`^-?[0-9]{1,9}$`)

// load применяет всё состояние устройства.
func (a *Appliance) load(st Status) {
	d := a.cloud.device(a.deviceID)
	a.mu.Lock()
	a.values = map[string]any{}
	for comp, caps := range st {
		for capability, attrs := range caps {
			if !a.mine(d, comp, capability) {
				continue
			}
			for attr, v := range attrs {
				a.setLocked(d, capability, attr, v)
			}
		}
	}
	a.have = true
	a.mu.Unlock()
	a.changed(nil)
}

// apply применяет событие: новое значение атрибута.
func (a *Appliance) apply(component, capability, attribute string, v AttrValue) {
	d := a.cloud.device(a.deviceID)
	if !a.mine(d, component, capability) {
		return
	}
	a.mu.Lock()
	if !a.have {
		a.mu.Unlock()
		return
	}
	a.setLocked(d, capability, attribute, v)
	a.mu.Unlock()
	a.changed(nil)
}

// target - компонент и описание capability для команды.
func (a *Appliance) target(key string) (string, string, string, *CapDef, error) {
	capability, name, ok := SplitKey(key)
	d := a.cloud.device(a.deviceID)
	if !ok || d == nil {
		return "", "", "", nil, fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
	}
	for _, ref := range d.capabilities(a.component) {
		if ref.ID == capability {
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
