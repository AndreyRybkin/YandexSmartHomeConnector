package delonghi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/metrics"
)

// Имена прибора по профилю: тип, программы, настройка питания, состояние и события.
const (
	// Type - тип прибора для встроенного профиля.
	Type = "DeLonghiCoffeeMaker"
	// ProgramPrefix - программа - рецепт машины: DeLonghi.Recipe.6 - американо.
	ProgramPrefix = "DeLonghi.Recipe."
	// SettingPower - питание: PowerOn включает, PowerOff выключает (TurnOnOff машины).
	SettingPower = "DeLonghi.Setting.TurnOnOff"
	PowerOn      = "2"
	PowerOff     = "1"
	// StatusMode - режим машины CurrMode: 0 выключена, 1 включается, 2 выключается, 7 готова.
	StatusMode = "DeLonghi.Status.CurrMode"
	// StatusState - Idle, Busy или Standby.
	StatusState = "DeLonghi.Status.Status"
	// StatusStep - этап: Grinding, Pre Infusion, Coffee Delivery, Water Heating Up.
	StatusStep = "DeLonghi.Status.StepDescr"
	// StatusDispensing - сколько процентов напитка налито.
	StatusDispensing = "DeLonghi.Status.BeverageDispensing"
	// StatusPouring - то же значение, но только во время налива: после него машина не сбрасывает
	// StatusDispensing сразу.
	StatusPouring = "DeLonghi.Status.Pouring"
	// AlarmPrefix - тревоги: DeLonghi.Alarm.TankLevel = EventPresent, пока нет воды.
	AlarmPrefix = "DeLonghi.Alarm."
	// PeripheralPrefix - датчики: DeLonghi.Peripheral.WaterTankLow = true.
	PeripheralPrefix = "DeLonghi.Peripheral."

	// StatusBrews - сколько напитков машина сварила за всё время, из тени BrewingStatisticsTotals.
	StatusBrews = "DeLonghi.Status.Brews"
	// EventBeverageFinished - напиток готов.
	EventBeverageFinished = "DeLonghi.Event.BeverageFinished"
	// EventLoginRequired - JWT не продлевается: нужен новый вход в аккаунт.
	EventLoginRequired = "DeLonghi.Event.LoginRequired"
)

const (
	// maxRecipe - самый большой номер рецепта.
	maxRecipe = 255
	// reconnectMin и reconnectMax - пауза перед переподключением, удваивается после неудачи.
	reconnectMin = 5 * time.Second
	reconnectMax = time.Minute
	// stableSession - соединение, которое проработало столько, сбрасывает паузу.
	stableSession = time.Minute
	// lanFresh - столько состояние по домашней сети считается известным.
	lanFresh = 10 * time.Minute
	// cloudPoll - как часто читать тень, пока состояние по домашней сети неизвестно.
	cloudPoll = 5 * time.Minute
	// pinRetry - как часто снимать отпечаток, пока его не подтвердили.
	pinRetry = 5 * time.Minute
)

// fastPoll и fastWindow - после смены питания тень читается каждые fastPoll в течение
// fastWindow: при включении машина рвёт соединение по домашней сети. Тесты их сокращают.
var (
	fastPoll   = 1500 * time.Millisecond
	fastWindow = 30 * time.Second
	// brewsWait - пауза перед чтением счётчика напитков, brewsPoll - как часто он читается сам,
	// brewsTimeout - предел одного чтения.
	brewsWait    = 30 * time.Second
	brewsPoll    = time.Hour
	brewsTimeout = 30 * time.Second
)

// offModes - CurrMode выключенной и выключающейся машины: переход между ними и остальными -
// смена питания.
var offModes = []float64{0, 2}

// Режимы CurrMode, в которых машина не наливает напиток: выключена, включается, выключается.
var serviceModes = []float64{0, 1, 2}

var _ appliance.Appliance = (*Appliance)(nil)

// Config - машина в настройках.
type Config struct {
	// ID - идентификатор прибора в настройках, для журнала и метрик.
	ID     string
	Host   string
	Serial string
	// Pin - подтверждённый отпечаток SHA-256 сертификата; пусто - ещё не подтверждён.
	Pin       string
	ProfileID int
}

// Appliance - кофемашина: команды и состояние по домашней сети, состояние в простое - из облака.
type Appliance struct {
	acct *Account
	conf Config
	pin  []byte
	log  *slog.Logger
	wake chan struct{}
	// boosted будит частый опрос тени после смены питания.
	boosted chan struct{}
	// brewed будит чтение счётчика напитков: напиток готов.
	brewed chan struct{}

	mu sync.Mutex
	// watchUntil - до какого времени тень читается часто.
	watchUntil time.Time
	values     map[string]any
	// lanAt - когда машина в последний раз прислала состояние по домашней сети.
	lanAt time.Time
	// have - состояние получено хотя бы раз.
	have bool
	conn *lanConn
	// observed - отпечаток сертификата, который снял шлюз, пока владелец его не подтвердил.
	observed string
	// brewing - машина наливает напиток; recipe - его рецепт, если напиток запустил шлюз.
	brewing bool
	recipe  int
	notify  appliance.Notify
	runs    int
}

// Appliance - машина из настроек. Отпечаток проверяется в config, здесь он уже 64 знака.
func (a *Account) Appliance(c Config) (*Appliance, error) {
	pin, err := parsePin(c.Pin)
	if err != nil {
		return nil, err
	}
	return &Appliance{acct: a, conf: c, pin: pin, log: a.log, wake: make(chan struct{}, 1), boosted: make(chan struct{}, 1), brewed: make(chan struct{}, 1),
		values: map[string]any{}}, nil
}

// parsePin разбирает отпечаток: 64 шестнадцатеричных знака, двоеточия допускаются.
// Пустой отпечаток - не подтверждён.
func parsePin(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := hexDecode(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) != 32 {
		return nil, errors.New("отпечаток сертификата: нужно 64 шестнадцатеричных знака")
	}
	return b, nil
}

// Observed - отпечаток сертификата машины, который ждёт подтверждения; пусто - не снят
// или уже подтверждён.
func (ap *Appliance) Observed() string {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.observed
}

// Describe - рецепты 1-255 как программы, питание как настройка. Какие рецепты видит
// Алиса, решает профиль.
func (ap *Appliance) Describe() appliance.Description {
	d := appliance.Description{Type: Type, Model: ap.acct.modelName(ap.conf.Serial), Options: map[string]appliance.Option{
		SettingPower: {Key: SettingPower, Values: []string{PowerOff, PowerOn}, Setting: true},
	}, Events: []string{EventBeverageFinished, EventLoginRequired}}
	for i := 1; i <= maxRecipe; i++ {
		d.Programs = append(d.Programs, ProgramPrefix+strconv.Itoa(i))
	}
	return d
}

func (ap *Appliance) State() appliance.State {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	st := appliance.State{Online: ap.conn != nil && ap.have, Values: maps.Clone(ap.values)}
	if ap.brewing && ap.recipe > 0 {
		st.ActiveProgram = ProgramPrefix + strconv.Itoa(ap.recipe)
	}
	if disp, _ := ap.values[StatusDispensing].(float64); ap.brewing && disp > 0 && disp < 100 {
		st.Values[StatusPouring] = disp
	}
	return st
}

// Run держит соединение с машиной и читает тень, пока состояние по домашней сети
// неизвестно. Возвращается, когда ctx отменён.
func (ap *Appliance) Run(ctx context.Context, notify appliance.Notify) {
	ap.mu.Lock()
	ap.runs++
	gen := ap.runs
	ap.notify = notify
	ap.mu.Unlock()
	ap.acct.attach(ap)
	defer func() {
		ap.acct.detach(ap)
		ap.mu.Lock()
		if ap.runs == gen {
			ap.notify = nil
		}
		ap.mu.Unlock()
	}()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		ap.pollCloud(ctx)
	}()
	go func() {
		defer wg.Done()
		ap.pollBrews(ctx)
	}()
	go func() {
		defer wg.Done()
		ap.fastCloud(ctx)
	}()
	ap.connectLoop(ctx)
	wg.Wait()
}

func (ap *Appliance) connectLoop(ctx context.Context) {
	pause := reconnectMin
	for ctx.Err() == nil {
		wait := pause
		if ap.pin == nil {
			ap.observe(ctx)
			wait = pinRetry
		} else if started, err := ap.session(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			ap.log.Warn("De'Longhi: нет связи с машиной", "device", ap.conf.ID, "err", err)
			metrics.SourceReconnect(Source, ap.conf.ID)
			if !started.IsZero() && time.Since(started) >= stableSession {
				pause = reconnectMin
			}
			wait = pause
			pause = min(pause*2, reconnectMax)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-ap.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// observe снимает отпечаток сертификата для подтверждения в интерфейсе. Токен не уходит.
func (ap *Appliance) observe(ctx context.Context) {
	fp, err := Fingerprint(ctx, ap.conf.Host)
	if err != nil {
		ap.log.Warn("De'Longhi: отпечаток сертификата не снят", "device", ap.conf.ID, "err", err)
		return
	}
	ap.mu.Lock()
	ap.observed = fp
	ap.mu.Unlock()
}

// session - одно соединение: вход и чтение до разрыва. started - когда вход удался.
func (ap *Appliance) session(ctx context.Context) (started time.Time, err error) {
	token, err := ap.acct.Token(ctx)
	if err != nil {
		return time.Time{}, err
	}
	dctx, cancel := context.WithTimeout(ctx, lanTimeout)
	c, err := dialLAN(dctx, ap.conf.Host, ap.pin, ap.conf.Serial, token)
	cancel()
	if err != nil {
		return time.Time{}, err
	}
	started = time.Now()
	stop := context.AfterFunc(ctx, c.close)
	defer stop()
	ap.setConn(c)
	metrics.SourceConnected(Source, ap.conf.ID, true)
	ap.log.Info("De'Longhi: машина на связи", "device", ap.conf.ID, "connection", c.id)
	err = c.read(func(m map[string]any) { ap.handle(m) })
	ap.setConn(nil)
	metrics.SourceConnected(Source, ap.conf.ID, false)
	c.close()
	return started, err
}

func (ap *Appliance) setConn(c *lanConn) {
	ap.mu.Lock()
	ap.conn = c
	ap.mu.Unlock()
	ap.changed(nil)
}

// handle разбирает сообщение машины без RequestId: состояние StatusService.
func (ap *Appliance) handle(m map[string]any) {
	if m["Message"] != "StatusService" {
		return
	}
	raw, ok := m["Response"].(string)
	if !ok {
		return
	}
	var reported map[string]any
	if json.Unmarshal([]byte(raw), &reported) != nil {
		return
	}
	ap.apply(reported, true, time.Time{})
}

// apply переносит состояние машины в значения прибора. Домашняя сеть главнее: тень с
// временем ts берётся, только если она новее последнего сообщения по домашней сети, а без
// времени - только пока по домашней сети состояние неизвестно.
func (ap *Appliance) apply(r map[string]any, lan bool, ts time.Time) {
	ap.mu.Lock()
	switch {
	case lan:
		ap.lanAt = time.Now()
	case !ts.IsZero() && !ap.lanAt.IsZero() && !ts.After(ap.lanAt.Truncate(time.Second)):
		ap.mu.Unlock()
		return
	case ts.IsZero() && ap.lanKnownLocked():
		ap.mu.Unlock()
		return
	}
	v := ap.values
	prevMode, hadMode := v[StatusMode].(float64)
	if f, ok := r["CurrMode"].(float64); ok {
		v[StatusMode] = f
	}
	for key, name := range map[string]string{"Status": StatusState, "StepDescr": StatusStep} {
		if s, ok := r[key].(string); ok {
			v[name] = s
		}
	}
	if f, ok := r["BeverageDispensing"].(float64); ok {
		v[StatusDispensing] = f
	}
	if alarms, ok := r["alarms"].(map[string]any); ok {
		for k, b := range alarms {
			if on, ok := b.(bool); ok && validName(k) {
				v[AlarmPrefix+k] = map[bool]string{true: appliance.EventPresent, false: appliance.EventOff}[on]
			}
		}
	}
	if per, ok := r["peripherals"].(map[string]any); ok {
		for k, b := range per {
			if on, ok := b.(bool); ok && validName(k) {
				v[PeripheralPrefix+k] = on
			}
		}
	}
	ap.have = true
	events := ap.trackBrewLocked()
	mode, _ := v[StatusMode].(float64)
	switched := hadMode && slices.Contains(offModes, prevMode) != slices.Contains(offModes, mode)
	ap.mu.Unlock()
	if switched {
		ap.boost()
	}
	if slices.Contains(events, EventBeverageFinished) {
		signal(ap.brewed)
	}
	ap.changed(events)
}

// pollBrews читает счётчик напитков при запуске, после каждого напитка и раз в brewsPoll: напиток могли
// сварить, пока шлюз не был на связи. Облако обновляет тень не сразу, поэтому чтение идёт с паузой.
func (ap *Appliance) pollBrews(ctx context.Context) {
	t := time.NewTimer(brewsWait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ap.brewed:
			t.Reset(brewsWait)
			continue
		case <-t.C:
		}
		ap.readBrews(ctx)
		t.Reset(brewsPoll)
	}
}

// readBrews записывает сумму варок. Без входа в облако или при ошибке прежнее значение остаётся.
func (ap *Appliance) readBrews(ctx context.Context) {
	if ap.conf.Serial == "" || !ap.acct.Status().LoggedIn {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, brewsTimeout)
	defer cancel()
	stats, err := ap.acct.RecipeStats(ctx, ap.conf.Serial)
	if err != nil {
		if ctx.Err() == nil {
			ap.log.Warn("De'Longhi: счётчик напитков не прочитан", "device", ap.conf.ID, "err", err)
		}
		return
	}
	total := 0
	for _, s := range stats {
		total += s.Brew
	}
	ap.mu.Lock()
	ap.values[StatusBrews] = float64(total)
	ap.mu.Unlock()
	ap.changed(nil)
}

// boost включает частый опрос тени на fastWindow; повторная смена питания продлевает окно.
func (ap *Appliance) boost() {
	ap.mu.Lock()
	ap.watchUntil = time.Now().Add(fastWindow)
	ap.mu.Unlock()
	signal(ap.boosted)
}

func (ap *Appliance) until() time.Time {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.watchUntil
}

// fastCloud после смены питания держит одно соединение с брокером и читает тень
// MachineStatus каждые fastPoll, пока окно не закончится.
func (ap *Appliance) fastCloud(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ap.boosted:
		}
		for ctx.Err() == nil && time.Now().Before(ap.until()) {
			thing := ap.thing(ctx)
			if thing == "" {
				break
			}
			err := ap.acct.watchShadow(ctx, thing, "MachineStatus", fastPoll, ap.until,
				func(r map[string]any, ts time.Time) { ap.apply(r, false, ts) })
			if err == nil || ctx.Err() != nil {
				break
			}
			ap.log.Warn("De'Longhi: частый опрос облака прервался", "device", ap.conf.ID, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// thing - имя машины в AWS IoT; при первом обращении шлюз загружает список машин.
func (ap *Appliance) thing(ctx context.Context) string {
	if !ap.acct.Status().LoggedIn {
		return ""
	}
	thing := ap.acct.machineName(ap.conf.Serial)
	if thing != "" {
		return thing
	}
	if err := ap.acct.RefreshDevices(ctx); err != nil {
		ap.log.Warn("De'Longhi: список машин не загружен", "device", ap.conf.ID, "err", err)
		return ""
	}
	if thing = ap.acct.machineName(ap.conf.Serial); thing == "" {
		ap.log.Warn("De'Longhi: машины с этим серийным номером нет в аккаунте", "device", ap.conf.ID)
	}
	return thing
}

// validName - имя поля машины, которое шлюз берёт в значения: буквы и цифры, до 64 знаков.
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// trackBrewLocked следит за напитком: он налит, когда налито 100 % или машина перестала
// быть занятой. Промывка при включении и выключении тоже «наливает», но в служебных режимах.
func (ap *Appliance) trackBrewLocked() []string {
	mode, _ := ap.values[StatusMode].(float64)
	busy := ap.values[StatusState] == "Busy"
	disp, _ := ap.values[StatusDispensing].(float64)
	service := slices.Contains(serviceModes, mode)
	if busy && !service && disp > 0 && disp < 100 {
		ap.brewing = true
	}
	if ap.brewing && (disp >= 100 || !busy || service) {
		ap.brewing, ap.recipe = false, 0
		if !service {
			return []string{EventBeverageFinished}
		}
	}
	return nil
}

func (ap *Appliance) lanKnownLocked() bool {
	return ap.conn != nil && !ap.lanAt.IsZero() && time.Since(ap.lanAt) < lanFresh
}

// pollCloud читает тень MachineStatus при запуске и каждые cloudPoll, пока состояние по
// домашней сети неизвестно.
func (ap *Appliance) pollCloud(ctx context.Context) {
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ap.readCloud(ctx)
		t.Reset(cloudPoll)
	}
}

func (ap *Appliance) readCloud(ctx context.Context) {
	ap.mu.Lock()
	known := ap.lanKnownLocked()
	ap.mu.Unlock()
	if known {
		return
	}
	thing := ap.thing(ctx)
	if thing == "" {
		return
	}
	r, err := ap.acct.readShadow(ctx, thing, "MachineStatus")
	if err != nil {
		ap.log.Warn("De'Longhi: состояние из облака не прочитано", "device", ap.conf.ID, "err", err)
		return
	}
	ap.apply(r, false, time.Time{})
}

func (ap *Appliance) changed(events []string) {
	ap.mu.Lock()
	n := ap.notify
	ap.mu.Unlock()
	if n != nil {
		n(events)
	}
}

// event сообщает о событии аккаунта, например о нужном входе.
func (ap *Appliance) event(name string) {
	ap.changed([]string{name})
}

func (ap *Appliance) Select(context.Context, string) error {
	return appliance.ErrNotSupported
}

// Start наливает напиток: program - DeLonghi.Recipe.<номер>, параметры не поддерживаются.
func (ap *Appliance) Start(ctx context.Context, program string, _ map[string]any) error {
	n, err := strconv.Atoi(strings.TrimPrefix(program, ProgramPrefix))
	if err != nil || !strings.HasPrefix(program, ProgramPrefix) || n < 1 || n > maxRecipe {
		return appliance.ErrInvalidValue
	}
	ap.mu.Lock()
	c, mode, state := ap.conn, ap.values[StatusMode], ap.values[StatusState]
	ap.mu.Unlock()
	if c == nil {
		return appliance.ErrUnreachable
	}
	if f, ok := mode.(float64); ok && slices.Contains(serviceModes, f) {
		return appliance.ErrWrongMode
	}
	if state == "Busy" {
		return appliance.ErrBusy
	}
	r, err := c.command(ctx, map[string]any{"Message": "BeverageRequest", "ProfileId": ap.conf.ProfileID, "RecipeId": n})
	if err := ap.result("BeverageRequest", r, err); err != nil {
		return err
	}
	ap.mu.Lock()
	ap.brewing, ap.recipe = true, n
	ap.mu.Unlock()
	ap.changed(nil)
	return nil
}

func (ap *Appliance) Stop(context.Context) error {
	return appliance.ErrNotSupported
}

func (ap *Appliance) Command(context.Context, string) error {
	return appliance.ErrNotSupported
}

// Set включает и выключает машину: TurnOnOff 2 и 1.
func (ap *Appliance) Set(ctx context.Context, key string, value any) error {
	if key != SettingPower {
		return appliance.ErrNotSupported
	}
	s := fmt.Sprint(value)
	if s != PowerOn && s != PowerOff {
		return appliance.ErrInvalidValue
	}
	ap.mu.Lock()
	c := ap.conn
	ap.mu.Unlock()
	if c == nil {
		return appliance.ErrUnreachable
	}
	n, _ := strconv.Atoi(s)
	r, err := c.command(ctx, map[string]any{"Message": "MachineUI", "TurnOnOff": n})
	if err := ap.result("MachineUI", r, err); err != nil {
		return err
	}
	ap.boost()
	return nil
}

// result переводит ответ машины в ошибку прибора; текст машины - только в журнал.
func (ap *Appliance) result(cmd string, r map[string]any, err error) error {
	if err != nil {
		ap.log.Warn("De'Longhi: команда не выполнена", "device", ap.conf.ID, "command", cmd, "err", err)
		return appliance.ErrUnreachable
	}
	if r["Response"] == "OK" {
		return nil
	}
	ap.log.Warn("De'Longhi: машина отказала", "device", ap.conf.ID, "command", cmd,
		"response", fmt.Sprint(r["Response"]), "error", fmt.Sprint(r["ErrorDescription"]))
	if cmd == "BeverageRequest" {
		return ap.refusal()
	}
	return appliance.ErrBusy
}

// refusal - причина, по которой машина отказала варить, по её тревогам. Заранее варку шлюз не
// запрещает: смысл тревог известен по именам, а машина знает лучше.
func (ap *Appliance) refusal() error {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	active := func(alarm string) bool { return ap.values[AlarmPrefix+alarm] == appliance.EventPresent }
	switch {
	case active("TankLevel"), active("TankPresence"):
		return appliance.ErrNotEnoughWater
	case active("NoCoffee"):
		return appliance.ErrContainerEmpty
	case active("Waste"):
		return appliance.ErrContainerFull
	case active("Drain"):
		return appliance.ErrDripTrayFull
	}
	for k, v := range ap.values {
		if strings.HasPrefix(k, AlarmPrefix) && v == appliance.EventPresent {
			return appliance.ErrHumanNeeded
		}
	}
	return appliance.ErrBusy
}
