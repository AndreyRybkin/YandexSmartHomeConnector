// Package hclocal - прибор Home Connect в домашней сети, без облака. Прибор принимает
// WebSocket /homeconnect: по TLS-PSK на порту 443 или на порту 80 с сообщениями,
// зашифрованными AES. Ключи и описание прибора шлюз загружает из облака и сохраняет в
// data_dir/appliances/<haId>. Сообщения и порядок рукопожатия - как у открытого клиента hcpy.
package hclocal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/hcdesc"
	"hcgateway/internal/metrics"
)

// SourceName - источник в метриках.
const SourceName = "homeconnect-local"

const (
	// Пауза перед переподключением: от 1 до 60 с, удваивается после каждой неудачи.
	reconnectMin = time.Second
	reconnectMax = time.Minute
	// stableSession - соединение, которое проработало столько, сбрасывает паузу.
	stableSession = time.Minute
	// dialTimeout - предел подключения и рукопожатия протокола.
	dialTimeout = 20 * time.Second
	// requestTimeout - сколько ждать ответа прибора на запрос.
	requestTimeout = 10 * time.Second
	// runWait - сколько шлюз в фоне ждёт состояния Run после запуска программы.
	runWait = 15 * time.Second
	// readyWait - сколько новая программа в фоне ждёт Ready после остановки прежней.
	readyWait = 2 * time.Minute
)

// pingEvery и readTimeout - проверка соединения: шлюз шлёт ping, прибор отвечает pong.
// Соединение, из которого readTimeout ничего не пришло, оборвано. Тесты их сокращают.
var (
	pingEvery   = 30 * time.Second
	readTimeout = 70 * time.Second
)

// Имена Home Connect, которые шлюз разбирает сам.
const (
	rootSelected       = "BSH.Common.Root.SelectedProgram"
	rootActive         = "BSH.Common.Root.ActiveProgram"
	statusLocalControl = "BSH.Common.Status.LocalControlActive"
	commandAbort       = "BSH.Common.Command.AbortProgram"
	operationInactive  = "BSH.Common.EnumType.OperationState.Inactive"
)

// Шлюз в ответе на /ei/initialValues: прибор запоминает подключённые приложения.
const (
	deviceName = "hcgateway"
	// deviceID - 8 шестнадцатеричных знаков, как у клиента hcpy.
	deviceID = "68636777"
)

var _ appliance.Appliance = (*Appliance)(nil)

// Appliance - прибор Home Connect в домашней сети.
type Appliance struct {
	// id - прибор в настройках, для журнала и метрик.
	id   string
	haID string
	// host - адрес прибора: имя или IP, порт по желанию.
	host     string
	keys     keys
	desc     appliance.Description
	features map[uint32]*hcdesc.Feature
	byName   map[string]*hcdesc.Feature
	log      *slog.Logger

	mu     sync.Mutex
	state  appliance.State
	notify appliance.Notify
	// runs - поколение Run: уведомления получает последний.
	runs int
	// sess - соединение после рукопожатия; nil - связи нет.
	sess *session
	// stoppedAt - когда шлюз остановил программу: новая программа подождёт Ready.
	stoppedAt time.Time
}

// Open загружает ключи и описание прибора haID из каталога dir. id - прибор в настройках.
func Open(id, dir, haID, host string, log *slog.Logger) (*Appliance, error) {
	info, k, d, err := load(dir, haID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", errNoKeys, err)
	}
	if err != nil {
		return nil, err
	}
	a := &Appliance{id: id, haID: haID, host: host, keys: k, features: d.Features, byName: map[string]*hcdesc.Feature{},
		log: log, state: appliance.State{Values: map[string]any{}}}
	for _, uid := range slices.Sorted(maps.Keys(d.Features)) {
		if f := d.Features[uid]; f.Name != "" && a.byName[f.Name] == nil {
			a.byName[f.Name] = f
		}
	}
	a.desc = describe(info.HaType, a.byName)
	return a, nil
}

// describe - что умеет прибор по его описанию: программы, команды, параметры и настройки.
func describe(haType string, byName map[string]*hcdesc.Feature) appliance.Description {
	d := appliance.Description{Type: haType, Options: map[string]appliance.Option{}}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		f := byName[name]
		switch f.Kind {
		case "program":
			d.Programs = append(d.Programs, name)
		case "command":
			d.Commands = append(d.Commands, name)
		case "option", "setting":
			o := appliance.Option{Key: name, Min: attr(f, "min"), Max: attr(f, "max"), Step: attr(f, "stepSize")}
			for _, n := range slices.Sorted(maps.Keys(f.Values)) {
				o.Values = append(o.Values, f.EnumValue(f.Values[n]))
			}
			d.Options[name] = o
		}
	}
	return d
}

func attr(f *hcdesc.Feature, name string) float64 {
	v, _ := strconv.ParseFloat(f.Attrs[name], 64)
	return v
}

// flag - функция-флаг: значение 0 или 1 прибор может прислать числом.
func flag(f *hcdesc.Feature) bool {
	return f.Attrs["refCID"] == "01" && f.Attrs["refDID"] == "00"
}

// HaID - haId прибора.
func (a *Appliance) HaID() string {
	return a.haID
}

func (a *Appliance) Describe() appliance.Description {
	d := a.desc
	d.Programs, d.Commands = slices.Clone(d.Programs), slices.Clone(d.Commands)
	d.Options = map[string]appliance.Option{}
	for k, o := range a.desc.Options {
		o.Values = slices.Clone(o.Values)
		d.Options[k] = o
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

// Run держит соединение с прибором, пока ctx не отменён. Уведомления отключаются раньше,
// чем закрывается соединение: прибор, который шлюз пересоздаёт, не сообщает Яндексу об обрыве.
func (a *Appliance) Run(ctx context.Context, notify appliance.Notify) {
	a.mu.Lock()
	a.runs++
	gen := a.runs
	a.notify = notify
	a.mu.Unlock()
	lctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.loop(lctx)
	}()
	<-ctx.Done()
	a.mu.Lock()
	if a.runs == gen {
		a.notify = nil
	}
	a.mu.Unlock()
	cancel()
	<-done
}

// loop подключается к прибору и переподключается после обрыва, пока ctx не отменён.
func (a *Appliance) loop(ctx context.Context) {
	pause := reconnectMin
	for {
		started := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= stableSession {
			pause = reconnectMin
		}
		a.log.Warn("прибор Home Connect в домашней сети: нет соединения, переподключение", "device", a.id,
			"ha_id", a.haID, "host", a.host, "err", err, "after", pause)
		metrics.SourceReconnect(SourceName, a.id)
		if sleep(ctx, pause) != nil {
			return
		}
		pause = min(pause*2, reconnectMax)
	}
}

// session - одно соединение: рукопожатие протокола, снимок состояния и уведомления до обрыва.
func (a *Appliance) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	l, err := dial(dctx, a.host, a.keys, identity)
	cancel()
	if err != nil {
		return err
	}
	s := newSession(l)
	// Сессия берёт интервалы один раз: тесты меняют их между сессиями.
	every, idle := pingEvery, readTimeout
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		a.read(s, idle)
	}()
	defer func() {
		s.fail(errClosed)
		<-readDone
		a.offline(s)
	}()
	if err := a.handshake(ctx, s); err != nil {
		return err
	}
	metrics.SourceConnected(SourceName, a.id, true)
	defer metrics.SourceConnected(SourceName, a.id, false)
	a.log.Info("прибор Home Connect в домашней сети на связи", "device", a.id, "ha_id", a.haID)
	ping := time.NewTicker(every)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closed:
			return s.error()
		case <-ping.C:
			if err := s.link.ping(); err != nil {
				return err
			}
		}
	}
}

// read читает сообщения прибора, пока соединение живо: ответы отдаёт запросам, уведомления
// применяет к состоянию.
func (a *Appliance) read(s *session, idle time.Duration) {
	s.link.ws.SetPongHandler(func(string) error {
		return s.link.ws.SetReadDeadline(time.Now().Add(idle))
	})
	for {
		s.link.ws.SetReadDeadline(time.Now().Add(idle))
		data, err := s.link.recv()
		if err != nil {
			s.fail(err)
			return
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			a.log.Warn("сообщение прибора Home Connect не разобрано", "device", a.id, "err", err)
			continue
		}
		switch {
		case m.Action == actionResponse && s.deliver(&m):
		case m.Action == actionPost && m.Resource == "/ei/initialValues":
			select {
			case s.initial <- &m:
			default:
			}
		case m.Action == actionNotify && m.Resource == "/ro/values":
			a.mu.Lock()
			events := a.applyLocked(m.Data, false)
			a.changedLocked(events)
		}
	}
}

// handshake - начало протокола: прибор присылает /ei/initialValues, шлюз отвечает, узнаёт
// версии служб и забирает снимок состояния.
func (a *Appliance) handshake(ctx context.Context, s *session) error {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var init *message
	select {
	case init = <-s.initial:
	case <-s.closed:
		return s.error()
	case <-ctx.Done():
		return errors.New("прибор не прислал /ei/initialValues")
	}
	var iv struct {
		EdMsgID int64 `json:"edMsgID"`
	}
	if len(init.Data) == 0 || json.Unmarshal(init.Data[0], &iv) != nil {
		return errors.New("/ei/initialValues не разобран")
	}
	s.begin(init.SID, iv.EdMsgID)
	var devType any = "Application"
	if init.Version == 1 {
		devType = 2
	}
	if err := s.reply(init, map[string]any{"deviceType": devType, "deviceName": deviceName, "deviceID": deviceID}); err != nil {
		return err
	}
	resp, err := s.call(ctx, actionGet, "/ci/services", 1, nil)
	if err != nil {
		return err
	}
	s.setServices(resp.Data)
	// Сведения о приборе шлюзу не нужны, но приборы ждут этих запросов. Ответы на них не ждутся.
	if s.has("iz") {
		err = s.post(actionGet, "/iz/info", 1, nil)
	} else {
		nonce := make([]byte, 32)
		rand.Read(nonce)
		if err = s.post(actionGet, "/ci/authentication", 2, map[string]any{"nonce": base64.RawURLEncoding.EncodeToString(nonce)}); err == nil {
			err = s.post(actionGet, "/ci/info", 1, nil)
		}
	}
	if err == nil {
		err = s.post(actionNotify, "/ei/deviceReady", 2, nil)
	}
	if err == nil && s.has("ni") {
		err = s.post(actionGet, "/ni/info", 1, nil)
	}
	if err != nil {
		return err
	}
	if resp, err = s.call(ctx, actionGet, "/ro/allMandatoryValues", 1, nil); err != nil {
		return err
	}
	a.mu.Lock()
	a.applyLocked(resp.Data, true)
	a.sess, a.state.Online = s, true
	a.changedLocked(nil)
	return s.post(actionGet, "/ro/allDescriptionChanges", 1, nil)
}

// offline - соединение s закончилось.
func (a *Appliance) offline(s *session) {
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	a.sess, a.state.Online = nil, false
	a.changedLocked(nil)
}

// changedLocked отпускает блокировку и сообщает об изменении.
func (a *Appliance) changedLocked(events []string) {
	notify := a.notify
	a.mu.Unlock()
	if notify != nil {
		notify(events)
	}
}

// applyLocked применяет значения прибора: снимок целиком (full) или изменения из уведомления.
// Возвращает события, которые наступили: их значение стало Present.
func (a *Appliance) applyLocked(items []json.RawMessage, full bool) []string {
	if full {
		a.state.Values = map[string]any{}
		a.state.ActiveProgram, a.state.SelectedProgram = "", ""
	}
	var events []string
	for _, raw := range items {
		var it struct {
			UID   uint32 `json:"uid"`
			Value any    `json:"value"`
		}
		if json.Unmarshal(raw, &it) != nil || it.Value == nil {
			continue
		}
		f := a.features[it.UID]
		if f == nil || f.Name == "" {
			continue
		}
		v := a.value(f, it.Value)
		switch f.Name {
		case rootActive:
			a.state.ActiveProgram, _ = v.(string)
		case rootSelected:
			a.state.SelectedProgram, _ = v.(string)
		default:
			a.state.Values[f.Name] = v
		}
		if !full && f.Kind == "event" && v == appliance.EventPresent {
			events = append(events, f.Name)
		}
		if f.Name == appliance.StatusOperationState {
			switch v {
			case appliance.OperationReady, appliance.OperationFinished, operationInactive:
				a.state.ActiveProgram = ""
			}
		}
	}
	return events
}

// value переводит значение прибора в значение шлюза: число перечисления - в имя,
// номер программы - в её имя, 0 и 1 флага - в bool.
func (a *Appliance) value(f *hcdesc.Feature, v any) any {
	n, isNum := v.(float64)
	whole := isNum && n >= 0 && n <= math.MaxUint32 && n == math.Trunc(n)
	switch {
	case f.Name == rootActive || f.Name == rootSelected:
		if p := a.features[uint32(n)]; whole && p != nil && p.Kind == "program" {
			return p.Name
		}
		return ""
	case f.Values != nil:
		if member, ok := f.Values[int(n)]; whole && ok {
			return f.EnumValue(member)
		}
	case flag(f) && isNum:
		return n != 0
	}
	return v
}

// raw переводит значение шлюза в значение прибора: имя значения перечисления - в число.
func raw(f *hcdesc.Feature, v any) (any, error) {
	if f.Values != nil {
		if s, ok := v.(string); ok {
			for n, member := range f.Values {
				if f.EnumValue(member) == s {
					return n, nil
				}
			}
		}
		return nil, fmt.Errorf("%w: %v для %s", appliance.ErrInvalidValue, v, f.Name)
	}
	switch v.(type) {
	case bool, float64, int, string:
		return v, nil
	}
	return nil, fmt.Errorf("%w: %v для %s", appliance.ErrInvalidValue, v, f.Name)
}

func (a *Appliance) current() *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

// change отправляет прибору изменение и ждёт ответа.
func (a *Appliance) change(ctx context.Context, resource string, data any) error {
	s := a.current()
	if s == nil {
		return appliance.ErrUnreachable
	}
	_, err := s.call(ctx, actionPost, resource, 1, data)
	var ce *codeError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return fmt.Errorf("%w: %w", appliance.ErrWrongMode, err)
	case errors.Is(err, context.Canceled):
		return err
	}
	return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
}

func (a *Appliance) program(name string) (*hcdesc.Feature, error) {
	p := a.byName[name]
	if p == nil || p.Kind != "program" {
		return nil, fmt.Errorf("%w: программы %s нет", appliance.ErrNotSupported, name)
	}
	return p, nil
}

func (a *Appliance) Select(ctx context.Context, program string) error {
	p, err := a.program(program)
	if err != nil {
		return err
	}
	if err := a.change(ctx, "/ro/selectedProgram", map[string]any{"program": p.UID, "options": []any{}}); err != nil {
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
	p, err := a.program(program)
	if err != nil {
		return err
	}
	opts := []any{}
	for _, k := range slices.Sorted(maps.Keys(options)) {
		f := a.byName[k]
		if f == nil {
			return fmt.Errorf("%w: параметра %s нет", appliance.ErrInvalidValue, k)
		}
		v, err := raw(f, options[k])
		if err != nil {
			return err
		}
		opts = append(opts, map[string]any{"uid": f.UID, "value": v})
	}
	data := map[string]any{"program": p.UID, "options": opts}
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
	if st.Values[appliance.StatusOperationState] != appliance.OperationReady {
		a.mu.Lock()
		stopping := time.Since(a.stoppedAt) < readyWait
		a.mu.Unlock()
		if !stopping {
			return appliance.ErrWrongMode
		}
		go a.startWhenReady(program, data)
		return nil
	}
	if err := a.change(ctx, "/ro/activeProgram", data); err != nil {
		return err
	}
	go a.waitRun(program)
	return nil
}

func (a *Appliance) startWhenReady(program string, data map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), readyWait)
	defer cancel()
	for a.State().Values[appliance.StatusOperationState] != appliance.OperationReady {
		if sleep(ctx, time.Second) != nil {
			a.log.Warn("новая программа не запущена: прибор не вернулся в Ready", "device", a.id,
				"program", program, "waited", readyWait)
			return
		}
	}
	if err := a.change(ctx, "/ro/activeProgram", data); err != nil {
		a.log.Warn("новая программа не запущена", "device", a.id, "program", program, "err", err)
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
			a.log.Warn("программа не перешла в Run", "device", a.id, "program", program, "waited", runWait)
			return
		}
	}
}

// Stop останавливает программу командой BSH.Common.Command.AbortProgram.
func (a *Appliance) Stop(ctx context.Context) error {
	if err := a.Command(ctx, commandAbort); err != nil {
		return err
	}
	a.mu.Lock()
	a.stoppedAt = time.Now()
	a.mu.Unlock()
	return nil
}

func (a *Appliance) Command(ctx context.Context, key string) error {
	f := a.byName[key]
	if f == nil || f.Kind != "command" {
		return fmt.Errorf("%w: команды %s нет", appliance.ErrNotSupported, key)
	}
	return a.change(ctx, "/ro/values", map[string]any{"uid": f.UID, "value": true})
}

func (a *Appliance) Set(ctx context.Context, key string, value any) error {
	f := a.byName[key]
	if f == nil || strings.EqualFold(f.Attrs["access"], "read") {
		return fmt.Errorf("%w: %s нельзя изменить", appliance.ErrNotSupported, key)
	}
	v, err := raw(f, value)
	if err != nil {
		return err
	}
	return a.change(ctx, "/ro/values", map[string]any{"uid": f.UID, "value": v})
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
