package yandex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/metrics"
)

const (
	// DefaultNotifyBase - сервер уведомлений Яндекса.
	DefaultNotifyBase = "https://dialogs.yandex.net"
	// notifyTimeout - таймаут одного уведомления.
	notifyTimeout = 10 * time.Second
	// notifyRetries - сколько раз повторять уведомление после 5xx или сбоя сети.
	notifyRetries = 2
	// notifyQueue - сколько уведомлений ждёт отправки; лишние отбрасываются.
	notifyQueue = 64
	// maxNotifyResponse - сколько байт ответа читать для журнала.
	maxNotifyResponse = 4 << 10
	// maxErrorMessage - сколько знаков текста ошибки Яндекса попадает в журнал.
	maxErrorMessage = 300
	// statusOnline и statusOffline - связь устройства в уведомлении.
	statusOnline  = "online"
	statusOffline = "offline"
)

// reply - ответ Яндекса на уведомление. RequestID документация велит записывать для разбора
// сбоев, ErrorMessage объясняет отказ.
type reply struct {
	RequestID    string `json:"request_id"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

type notifyJob struct {
	discovery bool
	// press - нажатие кнопки: уходит как есть и память о принятом не меняет.
	press   bool
	devices []alice.DeviceState
}

// notifyDevice - устройство в уведомлении: только то, что изменилось.
type notifyDevice struct {
	ID           string           `json:"id"`
	Status       string           `json:"status,omitempty"`
	Capabilities []alice.CapState `json:"capabilities,omitempty"`
	Properties   []alice.CapState `json:"properties,omitempty"`
}

// accepted - что Яндекс уже принял об устройстве: без связи или значения умений и свойств в JSON.
type accepted struct {
	offline bool
	values  map[string]string
}

// Notifier отправляет Яндексу уведомления об изменениях по одному, в порядке событий.
type Notifier struct {
	base   string
	userID string
	http   *http.Client
	log    *slog.Logger
	sleep  func(ctx context.Context, d time.Duration) error
	queue  chan notifyJob

	mu      sync.Mutex
	skillID string
	token   string
	// sent - что Яндекс принял по устройствам. Нет записи - следующее уведомление полное.
	sent map[string]*accepted
	// forgot и forgotAll - что забыто, пока идёт отправка: её итог запоминать нельзя.
	forgot    map[string]bool
	forgotAll bool
}

// NewNotifier создаёт отправителя уведомлений навыка skillID с токеном владельца навыка.
func NewNotifier(base, skillID, token, userID string, log *slog.Logger) *Notifier {
	return &Notifier{
		base: base, skillID: skillID, token: token, userID: userID,
		http:  &http.Client{Timeout: notifyTimeout},
		log:   log,
		sleep: sleepCtx,
		queue: make(chan notifyJob, notifyQueue),
		sent:  map[string]*accepted{}, forgot: map[string]bool{},
	}
}

// Configure задаёт навык и токен без перезапуска. Пустой skillID выключает уведомления.
func (n *Notifier) Configure(skillID, token string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.skillID != skillID || n.token != token {
		n.resetLocked()
	}
	n.skillID, n.token = skillID, token
}

// Reset забывает, что принял Яндекс: следующие уведомления полные. Так после нового списка устройств
// и новой привязки у Яндекса не остаётся пустых значений.
func (n *Notifier) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.resetLocked()
}

func (n *Notifier) resetLocked() {
	clear(n.sent)
	n.forgotAll = true
}

// Forget забывает, что принял Яндекс об устройстве id: следующее уведомление о нём полное.
func (n *Notifier) Forget(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.sent, id)
	n.forgot[id] = true
}

// Enabled сообщает, задан ли навык.
func (n *Notifier) Enabled() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.skillID != ""
}

func (n *Notifier) skill() (string, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.skillID, n.token
}

// Run отправляет уведомления из очереди, пока ctx не отменён.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-n.queue:
			n.send(ctx, j)
		}
	}
}

// State ставит в очередь уведомление о состоянии устройств: уйдёт только то, что изменилось.
func (n *Notifier) State(devices []alice.DeviceState) {
	n.enqueue(notifyJob{devices: devices})
}

// Event ставит в очередь нажатие кнопки: каждое уходит отдельно, даже с тем же значением.
func (n *Notifier) Event(device alice.DeviceState) {
	n.enqueue(notifyJob{press: true, devices: []alice.DeviceState{device}})
}

// Discovery ставит в очередь уведомление о том, что список устройств мог измениться.
func (n *Notifier) Discovery() {
	n.enqueue(notifyJob{discovery: true})
}

func (n *Notifier) enqueue(j notifyJob) {
	select {
	case n.queue <- j:
	default:
		metrics.Notification(j.kind(), metrics.NotifyDropped)
		n.log.Warn("очередь уведомлений Яндексу переполнена, уведомление отброшено")
	}
}

// kind - вид уведомления для метрик.
func (j notifyJob) kind() string {
	if j.discovery {
		return "discovery"
	}
	return "state"
}

func (n *Notifier) send(ctx context.Context, j notifyJob) {
	skillID, token := n.skill()
	if skillID == "" {
		return
	}
	path := "/callback/state"
	payload := map[string]any{"user_id": n.userID}
	var remember map[string]*accepted
	switch {
	case j.discovery:
		path = "/callback/discovery"
		n.Reset()
	case j.press:
		payload["devices"] = j.devices
	default:
		var devices []notifyDevice
		devices, remember = n.changes(j.devices)
		if len(devices) == 0 {
			return
		}
		payload["devices"] = devices
	}
	body, err := json.Marshal(map[string]any{
		"ts":      float64(time.Now().UnixMilli()) / 1000,
		"payload": payload,
	})
	if err != nil {
		n.log.Error("уведомление Яндексу", "err", err)
		return
	}
	endpoint := n.base + "/api/v1/skills/" + url.PathEscape(skillID) + path
	for attempt := 0; ; attempt++ {
		status, rep, err := n.post(ctx, endpoint, token, body)
		retry := err != nil || status >= 500
		if !retry {
			if status != http.StatusAccepted && status != http.StatusOK {
				metrics.Notification(j.kind(), metrics.NotifyRejected)
				n.log.Warn("Яндекс не принял уведомление", "path", path, "status", status, "error_code", rep.ErrorCode,
					"error_message", rep.ErrorMessage, "request_id", rep.RequestID)
				return
			}
			metrics.Notification(j.kind(), metrics.NotifyOK)
			n.log.Debug("уведомление Яндексу принято", "path", path, "request_id", rep.RequestID)
			n.remember(remember)
			return
		}
		if attempt >= notifyRetries || ctx.Err() != nil {
			metrics.Notification(j.kind(), metrics.NotifyFailed)
			n.log.Warn("уведомление Яндексу не отправлено", "path", path, "status", status, "err", err)
			return
		}
		if n.sleep(ctx, time.Duration(attempt+1)*time.Second) != nil {
			return
		}
	}
}

// changes - что отправить об устройствах и что запомнить, если Яндекс это примет. Устройство без
// изменений в уведомление не попадает. Нажатие кнопки уходит только своим уведомлением.
func (n *Notifier) changes(devices []alice.DeviceState) ([]notifyDevice, map[string]*accepted) {
	n.mu.Lock()
	defer n.mu.Unlock()
	clear(n.forgot)
	n.forgotAll = false
	var out []notifyDevice
	next := map[string]*accepted{}
	for _, st := range devices {
		prev := n.sent[st.ID]
		if st.ErrorCode != "" {
			if prev == nil || !prev.offline {
				out = append(out, notifyDevice{ID: st.ID, Status: statusOffline})
				next[st.ID] = &accepted{offline: true}
			}
			continue
		}
		cur := &accepted{values: map[string]string{}}
		d := notifyDevice{ID: st.ID}
		if prev == nil || prev.offline {
			// Связь могла вернуться, пока уведомление не отправлялось: Яндекс узнаёт об этом сразу.
			d.Status, prev = statusOnline, &accepted{}
		}
		for k, v := range prev.values {
			cur.values[k] = v
		}
		d.Capabilities = changed(st.Capabilities, prev, cur)
		d.Properties = changed(withoutButton(st.Properties), prev, cur)
		if d.Status != "" || len(d.Capabilities) > 0 || len(d.Properties) > 0 {
			out = append(out, d)
			next[st.ID] = cur
		}
	}
	return out, next
}

// changed - состояния, значение которых отличается от prev. Новые значения записываются в cur.
func changed(states []alice.CapState, prev, cur *accepted) []alice.CapState {
	var out []alice.CapState
	for _, c := range states {
		data, err := json.Marshal(c.State)
		if err != nil {
			continue
		}
		key := stateKey(c)
		if prev.values[key] != string(data) {
			out = append(out, c)
		}
		cur.values[key] = string(data)
	}
	return out
}

// stateKey - имя состояния в памяти о принятом. У цвета значение одно, а instance меняется: сравнить
// можно только с тем, что ушло последним, а не с прошлым значением того же instance.
func stateKey(c alice.CapState) string {
	if sv, ok := c.State.(alice.StateValue); ok && c.Type != alice.CapColor {
		return c.Type + "/" + sv.Instance
	}
	return c.Type
}

// withoutButton - свойства без нажатия кнопки: оно мгновенное, и прошлое нажатие нельзя показывать
// Яндексу как новое.
func withoutButton(props []alice.CapState) []alice.CapState {
	var out []alice.CapState
	for _, c := range props {
		if sv, ok := c.State.(alice.StateValue); ok && c.Type == alice.PropEvent && sv.Instance == alice.InstanceButton {
			continue
		}
		out = append(out, c)
	}
	return out
}

// remember запоминает то, что Яндекс принял, кроме забытого за время отправки.
func (n *Notifier) remember(next map[string]*accepted) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.forgotAll {
		return
	}
	for id, a := range next {
		if !n.forgot[id] {
			n.sent[id] = a
		}
	}
}

// post отправляет уведомление и возвращает код ответа и ответ Яндекса, если он есть. Текст
// ошибки обрезается до maxErrorMessage знаков.
func (n *Notifier) post(ctx context.Context, endpoint, token string, body []byte) (int, reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, reply{}, err
	}
	req.Header.Set("Authorization", "OAuth "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		return 0, reply{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxNotifyResponse))
	var rep reply
	json.Unmarshal(data, &rep)
	if r := []rune(rep.ErrorMessage); len(r) > maxErrorMessage {
		rep.ErrorMessage = string(r[:maxErrorMessage]) + "…"
	}
	return resp.StatusCode, rep, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
