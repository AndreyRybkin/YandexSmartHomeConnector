package polaris

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	"hcgateway/internal/appliance"
)

var (
	_ appliance.Appliance = (*Appliance)(nil)
	_ appliance.Stepper   = (*Appliance)(nil)
)

// Appliance - прибор Polaris как прибор по профилю. Программ у него нет: всё, что он умеет, -
// записать значение в топик control/<имя>.
type Appliance struct {
	hub *Hub
	id  string

	mu sync.Mutex
	// devType и reports - тип прибора и что он сообщает; лежат в файле прибора.
	devType string
	reports []string
	values  map[string]any
	// online и client - прибор прислал сообщение с запуска шлюза и клиент брокера ещё подключён.
	online bool
	client string
	notify appliance.Notify
	runs   int
	// offAt - когда шлюз выключил чайник командой; modeGen - номер последнего сообщения mode: ожидание
	// конца работы действует, только пока не пришло новое.
	offAt   time.Time
	modeGen int
	// finishAfter - значение finishWait; в тестах короче.
	finishAfter time.Duration
}

func newAppliance(h *Hub, id string) *Appliance {
	f := h.load(id)
	return &Appliance{hub: h, id: id, devType: f.DevType, reports: f.Reports, values: map[string]any{}, finishAfter: finishWait}
}

// DeviceID - идентификатор прибора в топиках.
func (a *Appliance) DeviceID() string {
	return a.id
}

func (a *Appliance) Describe() appliance.Description {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := appliance.Description{Type: profileType(a.devType), Options: map[string]appliance.Option{}, Reports: slices.Clone(a.reports)}
	if !Kettle(a.devType) {
		return d
	}
	teas := slices.Sorted(maps.Keys(teaTemperatures))
	d.Options[KeyMode] = appliance.Option{Key: KeyMode, Values: []string{"0", "1", "2", "3", "4", "5", "6"}, Setting: true}
	d.Options[KeyTemperature] = appliance.Option{Key: KeyTemperature, Min: minTemperature, Max: maxTemperature, Step: 1, Setting: true}
	d.Options[KeyTea] = appliance.Option{Key: KeyTea, Values: teas, Setting: true}
	for _, k := range []string{"backlight", "child_lock", "sound"} {
		d.Options[k] = appliance.Option{Key: k, Setting: true}
	}
	return d
}

func (a *Appliance) State() appliance.State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return appliance.State{Online: a.online, Values: maps.Clone(a.values)}
}

// Run подписывает прибор на уведомления. Сообщения приборов принимает брокер.
func (a *Appliance) Run(ctx context.Context, notify appliance.Notify) {
	a.mu.Lock()
	a.runs++
	gen := a.runs
	a.notify = notify
	a.mu.Unlock()
	<-ctx.Done()
	a.mu.Lock()
	if a.runs == gen {
		a.notify = nil
	}
	a.mu.Unlock()
}

// message применяет значение от прибора.
func (a *Appliance) message(client, key, value string) {
	a.mu.Lock()
	if _, known := a.values[key]; !known && len(a.values) >= maxValues {
		a.mu.Unlock()
		return
	}
	a.online, a.client = true, client
	previous, known := a.values[KeyMode]
	if a.devType == "" || Kettle(a.devType) || key == KeyMode {
		a.values[key] = parseValue(key, value)
	} else {
		a.values[key] = parseOther(key, value)
	}
	// Температура 0 в простое значит, что цель не задана: Алисе такую цель передавать нельзя.
	if f, ok := a.values[key].(float64); ok && key == KeyTemperature && f < minTemperature {
		delete(a.values, key)
	}
	if key == KeyMode {
		a.modeGen++
		if known && previous != modeOff && value == modeOff && time.Since(a.offAt) >= offWait {
			gen := a.modeGen
			time.AfterFunc(a.finishAfter, func() { a.finished(gen) })
		}
	}
	save := false
	if key == KeyDevType && value != a.devType {
		a.devType, save = value, true
	}
	if key == KeyErrorCode {
		// Код 01 - мало воды.
		a.values[KeyWaterAlarm] = map[bool]string{true: appliance.EventPresent, false: appliance.EventOff}[value == "01"]
		save = a.addReport(KeyWaterAlarm) || save
	}
	// Чайник выключился: выбранный чай больше не действует.
	if key == KeyMode && value == modeOff {
		delete(a.values, KeyTea)
	}
	save = a.addReport(key) || save
	if Kettle(a.devType) {
		save = a.addReport(KeyTea) || save
	}
	f := file{DevType: a.devType, Reports: slices.Clone(a.reports)}
	notify := a.notify
	a.mu.Unlock()
	if save {
		a.hub.save(a.id, f)
		a.hub.changed()
	}
	if notify != nil {
		notify(nil)
	}
}

// finished сообщает о конце работы, если чайник всё ещё выключен и его не сняли с базы.
func (a *Appliance) finished(gen int) {
	a.mu.Lock()
	notify := a.notify
	done := a.online && a.modeGen == gen && a.values[KeyEmptyBase] != true
	a.mu.Unlock()
	if done && notify != nil {
		notify([]string{EventBoilFinished})
	}
}

// addReport добавляет значение в список того, что прибор сообщает. true - список изменился.
func (a *Appliance) addReport(key string) bool {
	if slices.Contains(a.reports, key) || len(a.reports) >= maxValues {
		return false
	}
	a.reports = append(a.reports, key)
	slices.Sort(a.reports)
	return true
}

// clientGone снимает связь, если отключился клиент этого прибора.
func (a *Appliance) clientGone(client string) {
	a.mu.Lock()
	if !a.online || a.client != client {
		a.mu.Unlock()
		return
	}
	a.online = false
	notify := a.notify
	a.mu.Unlock()
	if notify != nil {
		notify(nil)
	}
}

func (a *Appliance) Select(context.Context, string) error {
	return appliance.ErrNotSupported
}

func (a *Appliance) Start(context.Context, string, map[string]any) error {
	return appliance.ErrNotSupported
}

func (a *Appliance) Stop(context.Context) error {
	return appliance.ErrNotSupported
}

func (a *Appliance) Command(context.Context, string) error {
	return appliance.ErrNotSupported
}

// Step меняет цель нагрева на шаг, когда цель не задана: шаг считается от температуры воды.
func (a *Appliance) Step(ctx context.Context, key string, delta float64) error {
	a.mu.Lock()
	current, ok := a.values[KeySensor].(float64)
	a.mu.Unlock()
	if key != KeyTemperature || !ok {
		return appliance.ErrNotSupported
	}
	return a.Set(ctx, key, math.Round(current+delta))
}

// Set записывает значение. Температура и чай сразу запускают разогрев: иначе «нагрей до 80» ничего
// бы не сделало.
func (a *Appliance) Set(_ context.Context, key string, value any) error {
	a.mu.Lock()
	online := a.online
	a.mu.Unlock()
	if !online {
		return appliance.ErrUnreachable
	}
	switch key {
	case KeyTemperature:
		f, ok := value.(float64)
		if !ok || f != math.Trunc(f) || f < minTemperature || f > maxTemperature {
			return fmt.Errorf("%w: температура %v", appliance.ErrInvalidValue, value)
		}
		return a.heat(int(f), "")
	case KeyTea:
		tea, _ := value.(string)
		t, ok := teaTemperatures[tea]
		if !ok {
			return fmt.Errorf("%w: чай %v", appliance.ErrInvalidValue, value)
		}
		return a.heat(t, tea)
	case KeyMode:
		mode := fmt.Sprint(value)
		if len(mode) != 1 || mode < "0" || mode > "6" {
			return fmt.Errorf("%w: режим %v", appliance.ErrInvalidValue, value)
		}
		if mode == modeOff {
			a.mu.Lock()
			delete(a.values, KeyTea)
			a.offAt = time.Now()
			a.mu.Unlock()
		}
		return a.hub.publish(a.id, KeyMode, mode)
	}
	text, ok := formatValue(value)
	if !ok || !partOK(key) {
		return fmt.Errorf("%w: %s", appliance.ErrInvalidValue, key)
	}
	return a.hub.publish(a.id, key, text)
}

// heat задаёт температуру и включает разогрев.
func (a *Appliance) heat(temperature int, tea string) error {
	if err := a.hub.publish(a.id, KeyTemperature, strconv.Itoa(temperature)); err != nil {
		return err
	}
	if err := a.hub.publish(a.id, KeyMode, modeHeat); err != nil {
		return err
	}
	a.mu.Lock()
	if tea != "" {
		a.values[KeyTea] = tea
	} else {
		delete(a.values, KeyTea)
	}
	a.mu.Unlock()
	return nil
}

// partOK проверяет имя функции из профиля: оно становится частью топика.
func partOK(key string) bool {
	if key == "" || len(key) > maxValueBytes {
		return false
	}
	for _, r := range key {
		if r == '+' || r == '#' || r == 0 {
			return false
		}
	}
	return true
}

// formatValue - значение команды строкой, как его ждёт прибор.
func formatValue(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}
