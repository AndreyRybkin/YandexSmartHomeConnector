package hcsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/atomicfile"
	"hcgateway/internal/hcapi"
)

const (
	// runWait - сколько шлюз в фоне ждёт состояния Run после запуска программы.
	runWait = 15 * time.Second
	// readyWait - сколько новая программа в фоне ждёт Ready после остановки прежней.
	readyWait = 2 * time.Minute
	// loadTimeout и describeTimeout - пределы загрузки состояния и описания прибора.
	loadTimeout     = time.Minute
	describeTimeout = 10 * time.Minute
	// maxDescription - предел файла описания.
	maxDescription = 1 << 20
	// maxRefused - сколько отказов подряд стерпеть за одну загрузку пределов настроек. Шлюз встаёт
	// на паузу после 5 ошибок подряд, а эта пауза задержала бы и команды Алисы.
	maxRefused = 2
)

// Имена Home Connect, которые сообщает только облако.
const (
	statusLocalControl = "BSH.Common.Status.LocalControlActive"
	rootSelected       = "BSH.Common.Root.SelectedProgram"
	rootActive         = "BSH.Common.Root.ActiveProgram"
)

// programPause - пауза между запросами описаний программ: загрузка растягивается
// и оставляет место в лимитах для команд. Тесты её сокращают.
var programPause = 2 * time.Second

var _ appliance.Appliance = (*Appliance)(nil)

// description - что известно о приборе. Программы и их параметры облако отдаёт по одной,
// поэтому описание хранится на диске и после перезапуска заново не загружается.
type description struct {
	Type     string            `json:"type"`
	Name     string            `json:"name,omitempty"`
	Brand    string            `json:"brand,omitempty"`
	Programs []string          `json:"programs"`
	Options  map[string]option `json:"options"`
	Commands []string          `json:"commands"`
	// CommandsLoaded - список команд уже запрашивали: пустой список значит, что команд нет.
	CommandsLoaded bool `json:"commands_loaded"`
}

type option struct {
	Min    float64  `json:"min,omitempty"`
	Max    float64  `json:"max,omitempty"`
	Step   float64  `json:"step,omitempty"`
	Values []string `json:"values,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	// Access - у настройки: read значит, что менять её нельзя.
	Access string `json:"access,omitempty"`
	// Loaded - пределы настройки уже запрашивали. В старых файлах настройки лежат без пределов и
	// без этого признака, поэтому шлюз запросит их один раз.
	Loaded bool `json:"loaded,omitempty"`
}

// Appliance - прибор из облака.
type Appliance struct {
	cloud *Cloud
	haID  string
	path  string

	mu    sync.Mutex
	desc  description
	state appliance.State
	// present - прибор есть в аккаунте и облако считает его на связи.
	present bool
	notify  appliance.Notify
	// runs - поколение Run: уведомления получает последний.
	runs int
	// stoppedAt - когда шлюз остановил программу: новая программа подождёт Ready.
	stoppedAt time.Time
	// loading и describing - загрузка состояния и описания уже идёт.
	loading, describing bool
}

func newAppliance(c *Cloud, haID, path string) *Appliance {
	a := &Appliance{cloud: c, haID: haID, path: path, state: appliance.State{Values: map[string]any{}}}
	a.desc.Options = map[string]option{}
	data, err := os.ReadFile(path)
	switch {
	case err != nil:
	case len(data) > maxDescription:
		c.log.Warn("описание прибора Home Connect больше предела, загружу заново", "ha_id", haID)
	default:
		var d description
		if err := json.Unmarshal(data, &d); err != nil {
			c.log.Warn("описание прибора Home Connect не разобрано, загружу заново", "ha_id", haID, "err", err)
			break
		}
		if d.Options == nil {
			d.Options = map[string]option{}
		}
		a.desc = d
	}
	return a
}

// HaID - haId прибора.
func (a *Appliance) HaID() string {
	return a.haID
}

func (a *Appliance) Describe() appliance.Description {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := appliance.Description{Type: a.desc.Type, Programs: slices.Clone(a.desc.Programs),
		Commands: slices.Clone(a.desc.Commands), Options: map[string]appliance.Option{}}
	for k, o := range a.desc.Options {
		d.Options[k] = appliance.Option{Key: k, Min: o.Min, Max: o.Max, Step: o.Step, Values: slices.Clone(o.Values),
			ReadOnly: strings.EqualFold(o.Access, "read")}
	}
	return d
}

func (a *Appliance) State() appliance.State {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state
	s.Values = maps.Clone(a.state.Values)
	return s
}

// Run подписывает прибор на уведомления. Сам поток событий один на все приборы, его держит Cloud.
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

// changedLocked отпускает блокировку и сообщает об изменении.
func (a *Appliance) changedLocked(events []string) {
	notify := a.notify
	a.mu.Unlock()
	if notify != nil {
		notify(events)
	}
}

// identify - сведения о приборе из списка аккаунта: тип, имя и связь.
func (a *Appliance) identify(h hcapi.HomeAppliance) {
	a.mu.Lock()
	changed := a.desc.Type != h.Type || a.desc.Name != h.Name || a.desc.Brand != h.Brand
	a.desc.Type, a.desc.Name, a.desc.Brand = h.Type, h.Name, h.Brand
	a.present = h.Connected
	if changed {
		a.saveLocked()
	}
	wasOnline := a.state.Online
	if !h.Connected {
		a.state.Online = false
	}
	if wasOnline && !h.Connected {
		a.changedLocked(nil)
	} else {
		a.mu.Unlock()
	}
	if changed {
		a.cloud.changed()
	}
}

func (a *Appliance) reachable() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.present
}

func (a *Appliance) setOnline(on bool) {
	a.mu.Lock()
	if a.state.Online == on {
		a.mu.Unlock()
		return
	}
	a.state.Online = on
	if !on {
		a.present = false
	}
	a.changedLocked(nil)
}

// load загружает состояние прибора, а затем, если нужно, его описание.
func (a *Appliance) load(ctx context.Context) {
	if a.refresh(ctx) {
		a.describe(ctx)
	}
}

// refresh загружает состояние прибора. false - не загружено или уже загружается.
func (a *Appliance) refresh(ctx context.Context) bool {
	a.mu.Lock()
	if a.loading {
		a.mu.Unlock()
		return false
	}
	a.loading = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.loading = false
		a.mu.Unlock()
	}()
	if err := a.loadState(ctx); err != nil {
		a.cloud.log.Warn("состояние прибора Home Connect не загружено", "ha_id", a.haID, "err", err)
		return false
	}
	return true
}

func (a *Appliance) loadState(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	api := a.cloud.API
	status, err := api.Status(ctx, a.haID)
	if err != nil {
		return err
	}
	settings, err := api.Settings(ctx, a.haID)
	if err != nil {
		return err
	}
	selected, err := api.SelectedProgram(ctx, a.haID)
	if err != nil {
		return err
	}
	active, err := api.ActiveProgram(ctx, a.haID)
	if err != nil {
		return err
	}
	st := appliance.State{Online: true, Values: map[string]any{}}
	for _, list := range [][]hcapi.Value{status, settings} {
		for _, v := range list {
			st.Values[v.Key] = v.Value
		}
	}
	if selected != nil {
		st.SelectedProgram = selected.Key
	}
	if active != nil {
		st.ActiveProgram = active.Key
		for _, v := range active.Options {
			st.Values[v.Key] = v.Value
		}
	}
	a.mu.Lock()
	// В снимке облака нет событий: без них уровень соли или воды вернулся бы в normal.
	for k, v := range a.state.Values {
		if strings.Contains(k, ".Event.") {
			st.Values[k] = v
		}
	}
	a.state, a.present = st, true
	a.changedLocked(nil)
	return nil
}

// describe дозагружает описание прибора, если эта загрузка ещё не идёт: сначала команды и
// программы, затем пределы настроек. Что уже загружено, сохраняется и при обрыве следующего шага.
func (a *Appliance) describe(ctx context.Context) {
	a.mu.Lock()
	if a.describing {
		a.mu.Unlock()
		return
	}
	a.describing = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.describing = false
		a.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	a.describePrograms(ctx)
	a.describeSettings(ctx)
}

// describePrograms дозагружает команды и описания новых программ. Каждая программа - отдельный
// запрос, поэтому между ними пауза programPause.
func (a *Appliance) describePrograms(ctx context.Context) {
	api := a.cloud.API
	changed := false
	a.mu.Lock()
	needCommands := !a.desc.CommandsLoaded
	known := slices.Clone(a.desc.Programs)
	for k := range a.state.Values {
		if strings.Contains(k, ".Setting.") {
			if _, ok := a.desc.Options[k]; !ok {
				a.desc.Options[k] = option{}
				changed = true
			}
		}
	}
	a.mu.Unlock()
	if needCommands {
		cmds, err := api.Commands(ctx, a.haID)
		var ae *hcapi.APIError
		switch {
		case err == nil:
		case errors.As(err, &ae) && ae.Status < 500 && ae.Status != 429:
			// Облако не отдаёт команды прибора: умения «пауза» не будет.
			cmds = nil
		default:
			a.cloud.log.Warn("команды прибора Home Connect не загружены", "ha_id", a.haID, "err", err)
			needCommands = false
		}
		if needCommands {
			a.mu.Lock()
			a.desc.Commands, a.desc.CommandsLoaded = cmds, true
			a.mu.Unlock()
			changed = true
		}
	}
	programs, err := api.AvailablePrograms(ctx, a.haID)
	if err != nil {
		a.cloud.log.Warn("программы прибора Home Connect не загружены", "ha_id", a.haID, "err", err)
	}
	first := true
	for _, p := range programs {
		if slices.Contains(known, p) {
			continue
		}
		if !first && sleep(ctx, programPause) != nil {
			break
		}
		first = false
		opts, err := api.ProgramOptions(ctx, a.haID, p)
		if err != nil {
			a.cloud.log.Warn("параметры программы Home Connect не загружены", "ha_id", a.haID, "program", p, "err", err)
			continue
		}
		a.mu.Lock()
		a.desc.Programs = append(a.desc.Programs, p)
		for _, o := range opts {
			if _, ok := a.desc.Options[o.Key]; ok {
				continue
			}
			op := option{Values: o.Constraints.AllowedValues, Unit: o.Unit}
			if o.Constraints.Min != nil {
				op.Min = *o.Constraints.Min
			}
			if o.Constraints.Max != nil {
				op.Max = *o.Constraints.Max
			}
			if o.Constraints.StepSize != nil {
				op.Step = *o.Constraints.StepSize
			}
			a.desc.Options[o.Key] = op
		}
		a.mu.Unlock()
		known = append(known, p)
		changed = true
	}
	if changed {
		a.mu.Lock()
		slices.Sort(a.desc.Programs)
		a.saveLocked()
		a.mu.Unlock()
		a.cloud.changed()
	}
}

// describeSettings запрашивает пределы и допустимые значения настроек, которых ещё нет в файле.
// Каждая настройка - отдельный запрос, поэтому между ними пауза programPause. Сбой или лимит
// облака прекращают загрузку до следующего подключения прибора: повторять сразу незачем, а
// прежнее описание остаётся как было.
func (a *Appliance) describeSettings(ctx context.Context) {
	a.mu.Lock()
	var keys []string
	for k, o := range a.desc.Options {
		if strings.Contains(k, ".Setting.") && !o.Loaded {
			keys = append(keys, k)
		}
	}
	a.mu.Unlock()
	slices.Sort(keys)
	changed := false
	refused := 0
loop:
	for _, key := range keys {
		if sleep(ctx, programPause) != nil {
			break
		}
		info, err := a.cloud.API.Setting(ctx, a.haID, key)
		switch {
		case err == nil:
			refused = 0
		case errors.Is(err, hcapi.ErrBadResponse):
			// Ответ не изменится от повтора: настройка остаётся без пределов.
			a.cloud.log.Warn("пределы настройки Home Connect не разобраны", "ha_id", a.haID, "setting", key, "err", err)
			info = hcapi.OptionInfo{}
		case settingRefused(err):
			// Облако не описывает настройку: она остаётся без пределов.
			a.cloud.log.Info("облако не отдало пределы настройки", "ha_id", a.haID, "setting", key, "err", err)
			refused++
		default:
			a.cloud.log.Warn("пределы настроек Home Connect не загружены", "ha_id", a.haID, "setting", key, "err", err)
			break loop
		}
		a.mu.Lock()
		a.desc.Options[key] = withSetting(a.desc.Options[key], info)
		a.mu.Unlock()
		changed = true
		if refused >= maxRefused {
			a.cloud.log.Warn("пределы настроек Home Connect: отказы подряд, продолжу при следующем подключении", "ha_id", a.haID)
			break
		}
	}
	if changed {
		a.mu.Lock()
		a.saveLocked()
		a.mu.Unlock()
		a.cloud.changed()
	}
}

// settingRefused - облако ответило, что не описывает эту настройку. Прибор не на связи (409), 429 и
// 5xx сюда не относятся: тогда пределы запрашиваются снова.
func settingRefused(err error) bool {
	var ae *hcapi.APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == 400 || ae.Status == 403 || ae.Status == 404
}

// withSetting отмечает, что пределы настройки запрашивали, и дополняет её ответом облака.
func withSetting(o option, info hcapi.OptionInfo) option {
	c := info.Constraints
	o.Loaded = true
	if c.Min != nil {
		o.Min = *c.Min
	}
	if c.Max != nil {
		o.Max = *c.Max
	}
	if c.StepSize != nil {
		o.Step = *c.StepSize
	}
	if len(c.AllowedValues) > 0 {
		o.Values = c.AllowedValues
	}
	if info.Unit != "" {
		o.Unit = info.Unit
	}
	if c.Access != "" {
		o.Access = c.Access
	}
	return o
}

func (a *Appliance) saveLocked() {
	data, err := json.MarshalIndent(a.desc, "", "  ")
	if err == nil {
		err = atomicfile.Write(a.path, data)
	}
	if err != nil {
		a.cloud.log.Error("описание прибора Home Connect не сохранено", "ha_id", a.haID, "err", err)
	}
}

// apply применяет событие потока: состояние, событие или изменение программы и параметров.
func (a *Appliance) apply(ev hcapi.Event) {
	a.mu.Lock()
	var events []string
	for _, it := range ev.Items {
		switch it.Key {
		case rootSelected:
			a.state.SelectedProgram, _ = it.Value.(string)
		case rootActive:
			a.state.ActiveProgram, _ = it.Value.(string)
		default:
			a.state.Values[it.Key] = it.Value
		}
		if ev.Type == hcapi.EventEvent && it.Value == appliance.EventPresent {
			events = append(events, it.Key)
		}
		if it.Key == appliance.StatusOperationState {
			switch it.Value {
			case appliance.OperationReady, appliance.OperationFinished, "BSH.Common.EnumType.OperationState.Inactive":
				a.state.ActiveProgram = ""
			}
		}
	}
	a.changedLocked(events)
}

func (a *Appliance) Select(ctx context.Context, program string) error {
	if err := mapErr(a.cloud.API.SelectProgram(ctx, a.haID, program)); err != nil {
		return err
	}
	a.mu.Lock()
	a.state.SelectedProgram = program
	a.changedLocked(nil)
	return nil
}

// Start запускает программу. Прибор под местным управлением или не в Ready команду не получает.
// Если шлюз только что остановил прежнюю программу, новая запускается в фоне, когда прибор
// вернётся в Ready. Состояния Run шлюз ждёт в фоне и пишет в журнал, если оно не наступило.
func (a *Appliance) Start(ctx context.Context, program string, options map[string]any) error {
	st := a.State()
	switch {
	case !st.Online:
		return appliance.ErrUnreachable
	case st.Values[statusLocalControl] == true:
		return appliance.ErrBusy
	case st.Values[appliance.StatusRemoteStartAllowed] == false:
		return appliance.ErrRemoteDisabled
	case st.Values[appliance.StatusDoorState] == appliance.DoorOpen:
		return appliance.ErrDoorOpen
	}
	opts := a.options(options)
	if st.Values[appliance.StatusOperationState] != appliance.OperationReady {
		a.mu.Lock()
		stopping := time.Since(a.stoppedAt) < readyWait
		a.mu.Unlock()
		if !stopping {
			return appliance.ErrWrongMode
		}
		go a.startWhenReady(program, opts)
		return nil
	}
	if err := mapErr(a.cloud.API.StartProgram(ctx, a.haID, program, opts)); err != nil {
		return err
	}
	go a.waitRun(program)
	return nil
}

// options - параметры запуска для облака, с единицами из описания программ.
func (a *Appliance) options(options map[string]any) []hcapi.Value {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]hcapi.Value, 0, len(options))
	for _, k := range slices.Sorted(maps.Keys(options)) {
		out = append(out, hcapi.Value{Key: k, Value: options[k], Unit: a.desc.Options[k].Unit})
	}
	return out
}

func (a *Appliance) startWhenReady(program string, opts []hcapi.Value) {
	ctx, cancel := context.WithTimeout(context.Background(), readyWait)
	defer cancel()
	for a.State().Values[appliance.StatusOperationState] != appliance.OperationReady {
		if sleep(ctx, time.Second) != nil {
			a.cloud.log.Warn("новая программа не запущена: прибор не вернулся в Ready", "ha_id", a.haID,
				"program", program, "waited", readyWait)
			return
		}
	}
	if err := a.cloud.API.StartProgram(ctx, a.haID, program, opts); err != nil {
		a.cloud.log.Warn("новая программа не запущена", "ha_id", a.haID, "program", program, "err", err)
		return
	}
	a.waitRun(program)
}

func (a *Appliance) waitRun(program string) {
	ctx, cancel := context.WithTimeout(context.Background(), runWait)
	defer cancel()
	for {
		switch a.State().Values[appliance.StatusOperationState] {
		case appliance.OperationRun, appliance.OperationDelayed:
			return
		}
		if sleep(ctx, 500*time.Millisecond) != nil {
			a.cloud.log.Warn("программа не перешла в Run", "ha_id", a.haID, "program", program, "waited", runWait)
			return
		}
	}
}

func (a *Appliance) Stop(ctx context.Context) error {
	if err := mapErr(a.cloud.API.StopProgram(ctx, a.haID)); err != nil {
		return err
	}
	a.mu.Lock()
	a.stoppedAt = time.Now()
	a.mu.Unlock()
	return nil
}

func (a *Appliance) Command(ctx context.Context, key string) error {
	return mapErr(a.cloud.API.Command(ctx, a.haID, key))
}

func (a *Appliance) Set(ctx context.Context, key string, value any) error {
	if strings.Contains(key, ".Setting.") {
		return mapErr(a.cloud.API.SetSetting(ctx, a.haID, key, value))
	}
	return mapErr(a.cloud.API.SetOption(ctx, a.haID, key, value))
}

// mapErr переводит ошибку облака в ошибку прибора: из неё hcdevice получает код Яндекса.
func mapErr(err error) error {
	var ae *hcapi.APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hcapi.ErrBusy), errors.Is(err, hcapi.ErrPaused):
		return fmt.Errorf("%w: %w", appliance.ErrBusy, err)
	case errors.Is(err, hcapi.ErrNotLoggedIn):
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
	case errors.As(err, &ae):
		switch {
		case ae.Status == 400:
			return fmt.Errorf("%w: %w", appliance.ErrInvalidValue, err)
		case ae.Status == 403, ae.Status == 404:
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
	// Сбой сети или таймаут.
	return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
