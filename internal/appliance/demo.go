package appliance

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sync"
	"time"
)

// Имена демо-приборов, которые используются и в профилях.
const (
	OptionSetpointTemperature = "Cooking.Oven.Option.SetpointTemperature"
	OptionRemainingTime       = "BSH.Common.Option.RemainingProgramTime"
	StatusCavityTemperature   = "Cooking.Oven.Status.CurrentCavityTemperature"
	EventProgramFinished      = "BSH.Common.Event.ProgramFinished"
	EventPreheatFinished      = "Cooking.Oven.Event.PreheatFinished"
	EventSaltNearlyEmpty      = "Dishcare.Dishwasher.Event.SaltNearlyEmpty"
	EventRinseAidNearlyEmpty  = "Dishcare.Dishwasher.Event.RinseAidNearlyEmpty"
)

var _ Appliance = (*Demo)(nil)

// DemoKinds - типы демо-приборов.
var DemoKinds = []string{"Oven", "Dishwasher", DemoAllInOne}

// roomTemperature - температура в камере выключенной демо-духовки.
const roomTemperature = 22.0

// Demo - прибор в памяти шлюза для проверки навыка без настоящих приборов.
// Программа идёт Duration и заканчивается событием ProgramFinished.
// Духовка разогревается за четверть программы, посудомойка после программы
// сообщает, что заканчивается соль.
type Demo struct {
	desc     Description
	duration time.Duration
	tick     time.Duration

	mu      sync.Mutex
	state   State
	elapsed time.Duration
	preheat bool
	step    int
	notify  Notify
}

// NewDemo создаёт демо-прибор типа kind из DemoKinds.
func NewDemo(kind string, duration time.Duration) (*Demo, error) {
	d := &Demo{duration: duration, tick: time.Second}
	values := map[string]any{
		StatusOperationState:     OperationReady,
		StatusDoorState:          DoorClosed,
		StatusRemoteStartAllowed: true,
		SettingPowerState:        PowerOn,
		EventProgramFinished:     EventOff,
	}
	switch kind {
	case "Oven":
		p := "Cooking.Oven.Program.HeatingMode."
		d.desc = Description{
			Type: kind,
			Programs: []string{p + "HotAir", p + "TopBottomHeating", p + "HotAirEco", p + "PizzaSetting",
				p + "PreHeating", p + "SlowCook", p + "GrillLargeArea", p + "Defrost"},
			Options: map[string]Option{
				OptionSetpointTemperature: {Key: OptionSetpointTemperature, Min: 30, Max: 275, Step: 5},
				SettingPowerState:         {Key: SettingPowerState, Values: []string{PowerOn, PowerStandby}},
			},
			Commands: []string{CommandPause, CommandResume},
		}
		values[StatusCavityTemperature] = roomTemperature
		values[EventPreheatFinished] = EventOff
	case "Dishwasher":
		p := "Dishcare.Dishwasher.Program."
		d.desc = Description{
			Type: kind,
			Programs: []string{p + "Auto2", p + "Eco50", p + "Quick45", p + "Intensiv70",
				p + "Normal65", p + "Glas40", p + "PreRinse", p + "NightWash"},
			Options: map[string]Option{
				SettingPowerState: {Key: SettingPowerState, Values: []string{PowerOn, PowerOff}},
			},
			Commands: []string{CommandPause, CommandResume},
		}
		values[EventSaltNearlyEmpty] = EventOff
		values[EventRinseAidNearlyEmpty] = EventOff
	case DemoAllInOne:
		d.desc = allInOneDescription()
	default:
		return nil, fmt.Errorf("нет демо-прибора типа %q", kind)
	}
	d.state = State{Online: true, Values: values}
	if kind == DemoAllInOne {
		d.setAllInOneStep(0)
	}
	return d, nil
}

func (d *Demo) Describe() Description {
	desc := d.desc
	desc.Options = maps.Clone(d.desc.Options)
	return desc
}

func (d *Demo) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.state
	s.Values = maps.Clone(d.state.Values)
	return s
}

func (d *Demo) Run(ctx context.Context, notify Notify) {
	d.mu.Lock()
	d.notify = notify
	d.mu.Unlock()
	t := time.NewTicker(d.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.advance(d.tick)
		}
	}
}

// advance продвигает работающую программу на dt.
func (d *Demo) advance(dt time.Duration) {
	d.mu.Lock()
	if d.state.Values[StatusOperationState] != OperationRun {
		d.mu.Unlock()
		return
	}
	d.elapsed += dt
	d.state.Values[OptionRemainingTime] = remainingSeconds(d.duration - d.elapsed)
	var events []string
	if d.desc.Type == "Oven" {
		target, _ := d.state.Values[OptionSetpointTemperature].(float64)
		heat := d.duration / 4
		temp := roomTemperature + (target-roomTemperature)*min(1, float64(d.elapsed)/float64(heat))
		d.state.Values[StatusCavityTemperature] = temp
		if !d.preheat && d.elapsed >= heat {
			d.preheat = true
			d.state.Values[EventPreheatFinished] = EventPresent
			events = append(events, EventPreheatFinished)
		}
	}
	if d.desc.Type == DemoAllInOne {
		if step := min(allInOneSteps, int(allInOneSteps*d.elapsed/d.duration)); step > d.step {
			d.setAllInOneStep(step)
		}
	}
	if d.elapsed >= d.duration {
		d.state.Values[StatusOperationState] = OperationFinished
		d.state.ActiveProgram = ""
		d.state.Values[EventProgramFinished] = EventPresent
		events = append(events, EventProgramFinished)
		if d.desc.Type == "Dishwasher" {
			d.state.Values[EventSaltNearlyEmpty] = EventPresent
			events = append(events, EventSaltNearlyEmpty)
		}
		if d.desc.Type == "Oven" {
			d.state.Values[StatusCavityTemperature] = roomTemperature
		}
	}
	d.changedLocked(events)
}

// remainingSeconds - оставшееся время программы в целых секундах, как его присылает
// Home Connect; после конца программы - 0.
func remainingSeconds(left time.Duration) float64 {
	return max(0, math.Round(left.Seconds()))
}

// changedLocked отпускает блокировку и сообщает об изменении.
func (d *Demo) changedLocked(events []string) {
	notify := d.notify
	d.mu.Unlock()
	if notify != nil {
		notify(events)
	}
}

func (d *Demo) Select(ctx context.Context, program string) error {
	if !d.desc.HasProgram(program) {
		return ErrInvalidValue
	}
	d.mu.Lock()
	d.state.SelectedProgram = program
	d.changedLocked(nil)
	return nil
}

func (d *Demo) Start(ctx context.Context, program string, options map[string]any) error {
	if !d.desc.HasProgram(program) {
		return ErrInvalidValue
	}
	for k, v := range options {
		if err := d.check(k, v); err != nil {
			return err
		}
	}
	d.mu.Lock()
	switch {
	case d.state.Running():
		d.mu.Unlock()
		return ErrBusy
	case d.state.Values[StatusRemoteStartAllowed] != true:
		d.mu.Unlock()
		return ErrRemoteDisabled
	case d.state.Values[StatusDoorState] == DoorOpen:
		d.mu.Unlock()
		return ErrDoorOpen
	}
	for k, v := range options {
		d.state.Values[k] = v
	}
	d.state.ActiveProgram = program
	d.state.SelectedProgram = program
	d.state.Values[OptionRemainingTime] = remainingSeconds(d.duration)
	d.state.Values[StatusOperationState] = OperationRun
	d.state.Values[SettingPowerState] = PowerOn
	d.state.Values[EventProgramFinished] = EventOff
	d.elapsed = 0
	d.preheat = false
	if d.desc.Type == "Oven" {
		d.state.Values[EventPreheatFinished] = EventOff
	}
	if d.desc.Type == DemoAllInOne {
		d.setAllInOneStep(0)
	}
	d.changedLocked(nil)
	return nil
}

func (d *Demo) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.state.Running() {
		d.mu.Unlock()
		return ErrWrongMode
	}
	d.state.ActiveProgram = ""
	d.state.Values[StatusOperationState] = OperationReady
	if d.desc.Type == "Oven" {
		d.state.Values[StatusCavityTemperature] = roomTemperature
	}
	if d.desc.Type == DemoAllInOne {
		d.setAllInOneStep(0)
	}
	d.changedLocked(nil)
	return nil
}

func (d *Demo) Command(ctx context.Context, key string) error {
	if !d.desc.HasCommand(key) {
		return ErrNotSupported
	}
	d.mu.Lock()
	op := d.state.Values[StatusOperationState]
	switch {
	case key == CommandPause && op == OperationRun:
		d.state.Values[StatusOperationState] = OperationPause
	case key == CommandResume && op == OperationPause:
		d.state.Values[StatusOperationState] = OperationRun
	default:
		d.mu.Unlock()
		return ErrWrongMode
	}
	d.changedLocked(nil)
	return nil
}

func (d *Demo) Set(ctx context.Context, key string, value any) error {
	if err := d.check(key, value); err != nil {
		return err
	}
	d.mu.Lock()
	// Параметр программы меняется только у работающей программы, настройка - всегда.
	if key != SettingPowerState && !d.state.Running() {
		d.mu.Unlock()
		return ErrWrongMode
	}
	if key == SettingPowerState && value != PowerOn && d.state.Running() {
		d.state.ActiveProgram = ""
		d.state.Values[StatusOperationState] = OperationReady
	}
	d.state.Values[key] = value
	d.changedLocked(nil)
	return nil
}

// check проверяет значение по описанию параметра.
func (d *Demo) check(key string, value any) error {
	opt, ok := d.desc.Options[key]
	if !ok {
		return ErrNotSupported
	}
	if len(opt.Values) > 0 {
		s, ok := value.(string)
		if !ok || !contains(opt.Values, s) {
			return ErrInvalidValue
		}
		return nil
	}
	f, ok := value.(float64)
	if !ok || f < opt.Min || f > opt.Max {
		return ErrInvalidValue
	}
	return nil
}
