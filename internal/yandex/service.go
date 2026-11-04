package yandex

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/metrics"
)

const (
	// UserID - идентификатор единственного пользователя шлюза в ответах Яндексу.
	UserID = "owner"
	// ActionTimeout - за сколько шлюз отвечает на команду Алисы.
	ActionTimeout = 3 * time.Second
	// maxBody - предел тела запроса Яндекса.
	maxBody = 64 << 10
	// maxRequestDevices - предел устройств в одном запросе.
	maxRequestDevices = 10
	// maxCommands - предел команд одному устройству в одном запросе.
	maxCommands = 10
	// maxField - предел длины идентификаторов, типов и instance в запросе.
	maxField = 128
	// debounce - сколько ждать после изменения, чтобы отправить несколько изменений одним уведомлением.
	debounce = 500 * time.Millisecond
)

// Tokens проверяет и отзывает токены, выданные Яндексу при привязке.
type Tokens interface {
	Valid(access string) bool
	RevokeAll() error
	Linked() bool
}

// Service - провайдер умного дома: запросы платформы и уведомления.
// Список приборов меняется на ходу через SetDevices.
type Service struct {
	tokens   Tokens
	notifier *Notifier
	log      *slog.Logger
	changes  chan change

	// setMu - замены списка по одной: остановка прежних приборов и запуск новых не перемешиваются.
	setMu sync.Mutex

	mu      sync.RWMutex
	devices []alice.Device
	// byID - прибор по идентификатору его устройства Алисы.
	byID map[string]alice.Device
	// ctx - контекст Run; nil, пока Run не запущен.
	ctx     context.Context
	stopped bool
	running map[alice.Device]*runner
	wg      sync.WaitGroup
	// onAction - куда сообщать о командах; задаётся до первой команды.
	onAction ActionFunc
}

// Откуда пришла команда.
const (
	InitiatorAlice   = "alice"
	InitiatorGateway = "gateway"
)

// ActionFunc получает команду устройству из настроек: когда шлюз её получил, откуда, что и код
// ошибки Яндекса, пустой у выполненной команды.
type ActionFunc func(id, initiator string, at time.Time, a *alice.Action, code string)

// OnAction задаёт, куда сообщать о командах. Вызывается до первой команды.
func (s *Service) OnAction(f ActionFunc) {
	s.onAction = f
}

// runner - связь с одним прибором.
type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type change struct {
	dev    alice.Device
	events []alice.DeviceState
}

// NewService создаёт провайдера. notifier может быть nil: тогда уведомления не отправляются.
func NewService(devices []alice.Device, tokens Tokens, notifier *Notifier, log *slog.Logger) *Service {
	s := &Service{tokens: tokens, notifier: notifier, log: log, changes: make(chan change, 64),
		running: map[alice.Device]*runner{}}
	s.SetDevices(devices)
	return s
}

// SetDevices заменяет список приборов. Прибор, которого нет в новом списке, останавливается,
// новый - запускается, а тот же объект продолжает работать без переподключения.
func (s *Service) SetDevices(devices []alice.Device) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	keep := map[alice.Device]bool{}
	byID := map[string]alice.Device{}
	for _, d := range devices {
		keep[d] = true
		for _, id := range d.IDs() {
			byID[id] = d
		}
	}
	s.mu.Lock()
	var stopped []*runner
	for d, r := range s.running {
		if !keep[d] {
			r.cancel()
			stopped = append(stopped, r)
			delete(s.running, d)
		}
	}
	s.devices, s.byID = slices.Clone(devices), byID
	s.mu.Unlock()
	// Новый прибор запускается после остановки прежнего: два соединения с одним прибором не нужны.
	for _, r := range stopped {
		<-r.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil && !s.stopped {
		for _, d := range s.devices {
			if s.running[d] == nil {
				s.startLocked(d)
			}
		}
	}
}

func (s *Service) startLocked(d alice.Device) {
	ctx, cancel := context.WithCancel(s.ctx)
	r := &runner{cancel: cancel, done: make(chan struct{})}
	s.running[d] = r
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(r.done)
		d.Run(ctx, func(events []alice.DeviceState) { s.changed(d, events) })
	}()
}

// Run держит связь с приборами и отправляет уведомления, пока ctx не отменён.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	for _, d := range s.devices {
		s.startLocked(d)
	}
	s.mu.Unlock()
	if s.notifier != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.notifier.Run(ctx)
		}()
		s.Discovery()
	}
	s.collect(ctx)
	// Контексты приборов - дочерние от ctx: приборы останавливаются сами, новые уже не запускаются.
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.wg.Wait()
}

// Discovery сообщает Яндексу, что список устройств мог измениться.
func (s *Service) Discovery() {
	if s.ready() {
		s.notifier.Discovery()
	}
}

// snapshot - текущий список приборов и индекс по идентификаторам.
func (s *Service) snapshot() ([]alice.Device, map[string]alice.Device) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devices, s.byID
}

// current сообщает, работает ли прибор: изменение от заменённого прибора уже не нужно.
func (s *Service) current(d alice.Device) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running[d] != nil
}

// changed вызывается прибором после изменения.
func (s *Service) changed(d alice.Device, events []alice.DeviceState) {
	select {
	case s.changes <- change{dev: d, events: events}:
	default:
		s.log.Warn("очередь изменений переполнена", "device", d.IDs()[0])
	}
}

// collect собирает изменения за debounce и отправляет состояние каждого изменившегося
// прибора своим уведомлением: если Яндекс отвергнет состояние одного прибора, изменения
// остальных всё равно дойдут. Нажатия кнопки уходят сразу и по одному: два нажатия подряд
// не должны слиться в одно.
func (s *Service) collect(ctx context.Context) {
	dirty := map[alice.Device]bool{}
	timer := time.NewTimer(debounce)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case c := <-s.changes:
			if !s.current(c.dev) {
				continue
			}
			if s.ready() {
				for _, e := range c.events {
					s.notifier.Event(e)
				}
			}
			if len(dirty) == 0 {
				timer.Reset(debounce)
			}
			dirty[c.dev] = true
		case <-timer.C:
			if s.ready() {
				devices, _ := s.snapshot()
				for _, d := range devices {
					if dirty[d] {
						s.notifier.State(d.States())
					}
				}
			}
			clear(dirty)
		}
	}
}

// ready сообщает, есть ли кому отправлять уведомления: навык настроен и аккаунт привязан.
func (s *Service) ready() bool {
	return s.notifier != nil && s.notifier.Enabled() && s.tokens.Linked()
}

// Register добавляет обработчики запросов платформы в mux.
// Каждый обработчик считается в метриках под своим именем.
func (s *Service) Register(mux *http.ServeMux) {
	head := metrics.YandexHandler("head", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	mux.Handle("HEAD /v1.0", head)
	mux.Handle("HEAD /v1.0/{$}", head)
	mux.Handle("POST /v1.0/user/unlink", metrics.YandexHandler("unlink", s.auth(s.unlink)))
	mux.Handle("GET /v1.0/user/devices", metrics.YandexHandler("devices", s.auth(s.list)))
	mux.Handle("POST /v1.0/user/devices/query", metrics.YandexHandler("query", s.auth(s.query)))
	mux.Handle("POST /v1.0/user/devices/action", metrics.YandexHandler("action", s.auth(s.action)))
}

// auth пропускает только запросы с действующим токеном. Отказ - 401 без подробностей.
func (s *Service) auth(next func(w http.ResponseWriter, r *http.Request, requestID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !s.tokens.Valid(token) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id := r.Header.Get("X-Request-Id")
		if len(id) > maxField {
			id = id[:maxField]
		}
		next(w, r, id)
	}
}

func (s *Service) unlink(w http.ResponseWriter, r *http.Request, requestID string) {
	if err := s.tokens.RevokeAll(); err != nil {
		s.log.Error("отвязка аккаунта", "request_id", requestID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.log.Info("аккаунт Яндекса отвязан", "request_id", requestID)
	writeJSON(w, map[string]any{"request_id": requestID})
}

func (s *Service) list(w http.ResponseWriter, r *http.Request, requestID string) {
	// Яндекс перечитывает список после привязки и после уведомления: следующие состояния полные.
	if s.notifier != nil {
		s.notifier.Reset()
	}
	var devices []alice.DeviceInfo
	list, _ := s.snapshot()
	for _, d := range list {
		devices = append(devices, d.Info()...)
	}
	writeJSON(w, map[string]any{
		"request_id": requestID,
		"payload":    map[string]any{"user_id": UserID, "devices": devices},
	})
}

func (s *Service) query(w http.ResponseWriter, r *http.Request, requestID string) {
	var req queryRequest
	if !s.decode(w, r, requestID, &req) {
		return
	}
	if len(req.Devices) > maxRequestDevices {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	out := make([]alice.DeviceState, 0, len(req.Devices))
	for _, rd := range req.Devices {
		if len(rd.ID) > maxField {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		out = append(out, s.stateOf(rd.ID))
	}
	writeJSON(w, map[string]any{"request_id": requestID, "payload": map[string]any{"devices": out}})
}

// stateOf - состояние одного устройства Алисы.
func (s *Service) stateOf(id string) alice.DeviceState {
	_, byID := s.snapshot()
	if d := byID[id]; d != nil {
		for _, st := range d.States() {
			if st.ID == id {
				st.Properties = withoutButton(st.Properties)
				return st
			}
		}
	}
	return alice.DeviceState{ID: id, ErrorCode: alice.CodeNotFound}
}

func (s *Service) action(w http.ResponseWriter, r *http.Request, requestID string) {
	var req actionRequest
	if !s.decode(w, r, requestID, &req) {
		return
	}
	devices := req.Payload.Devices
	if len(devices) > maxRequestDevices {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, d := range devices {
		if len(d.ID) > maxField || len(d.Capabilities) > maxCommands {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, c := range d.Capabilities {
			if len(c.Type) > maxField || len(c.State.Instance) > maxField {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), ActionTimeout)
	defer cancel()
	out := make([]actionDeviceResult, len(devices))
	var wg sync.WaitGroup
	for i, rd := range devices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = s.actDevice(ctx, requestID, &rd)
		}()
	}
	wg.Wait()
	writeJSON(w, map[string]any{"request_id": requestID, "payload": map[string]any{"devices": out}})
}

func (s *Service) actDevice(ctx context.Context, requestID string, rd *actionDevice) actionDeviceResult {
	res := actionDeviceResult{ID: rd.ID}
	// В метки метрик попадают только известные устройства и умения: число рядов не растёт от чужих запросов.
	_, byID := s.snapshot()
	device := rd.ID
	if byID[device] == nil {
		device = "unknown"
	}
	for _, c := range rd.Capabilities {
		result := actionResult{Status: "DONE"}
		if ycode := s.act(ctx, InitiatorAlice, requestID, rd.ID, &c); ycode != "" {
			result = actionResult{Status: "ERROR", ErrorCode: ycode}
			metrics.Action(device, capabilityLabel(c.Type), ycode)
		} else {
			metrics.Action(device, capabilityLabel(c.Type), "DONE")
		}
		res.Capabilities = append(res.Capabilities, alice.CapState{Type: c.Type,
			State: resultState{Instance: c.State.Instance, ActionResult: result}})
	}
	return res
}

// capabilityLabel - умение для метрик: on_off, mode, range, toggle или other.
func capabilityLabel(t string) string {
	switch t {
	case alice.CapOnOff, alice.CapMode, alice.CapRange, alice.CapToggle:
		return strings.TrimPrefix(t, "devices.capabilities.")
	}
	return "other"
}

// Command выполняет команду из веб-интерфейса тем же путём, что команду Алисы,
// и с тем же сроком ActionTimeout. Возвращает код ошибки Яндекса или пустую строку.
func (s *Service) Command(ctx context.Context, id string, a *alice.Action) string {
	ctx, cancel := context.WithTimeout(ctx, ActionTimeout)
	defer cancel()
	return s.act(ctx, InitiatorGateway, "", id, a)
}

// act выполняет одну команду и пишет её в журнал. Команду устройству, которого нет в настройках, в
// журнал действий не записывает: иначе чужие запросы с выдуманными именами раздували бы базу.
func (s *Service) act(ctx context.Context, initiator, requestID, id string, a *alice.Action) string {
	what := "команда из интерфейса"
	if initiator == InitiatorAlice {
		what = "команда Алисы"
	}
	at := time.Now()
	_, byID := s.snapshot()
	ycode := alice.CodeNotFound
	if d := byID[id]; d != nil {
		ycode = d.Act(ctx, id, a)
		if s.onAction != nil {
			s.onAction(id, initiator, at, a, ycode)
		}
		// Яндекс мог запомнить значение из команды, а прибор его не принял: следующее
		// уведомление об этом устройстве сообщит все умения.
		if s.notifier != nil {
			s.notifier.Forget(id)
		}
	}
	attrs := []any{"device", id, "type", a.Type, "instance", a.State.Instance}
	if requestID != "" {
		attrs = append([]any{"request_id", requestID}, attrs...)
	}
	if ycode != "" {
		s.log.Info(what+" не выполнена", append(attrs, "error_code", ycode)...)
	} else {
		s.log.Info(what, attrs...)
	}
	return ycode
}

func (s *Service) decode(w http.ResponseWriter, r *http.Request, requestID string, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		s.log.Info("неверный запрос Яндекса", "request_id", requestID, "path", r.URL.Path)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
