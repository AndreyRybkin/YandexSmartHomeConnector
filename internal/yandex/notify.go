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
	devices   []alice.DeviceState
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
}

// NewNotifier создаёт отправителя уведомлений навыка skillID с токеном владельца навыка.
func NewNotifier(base, skillID, token, userID string, log *slog.Logger) *Notifier {
	return &Notifier{
		base: base, skillID: skillID, token: token, userID: userID,
		http:  &http.Client{Timeout: notifyTimeout},
		log:   log,
		sleep: sleepCtx,
		queue: make(chan notifyJob, notifyQueue),
	}
}

// Configure задаёт навык и токен без перезапуска. Пустой skillID выключает уведомления.
func (n *Notifier) Configure(skillID, token string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.skillID, n.token = skillID, token
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

// State ставит в очередь уведомление о состоянии устройств.
func (n *Notifier) State(devices []alice.DeviceState) {
	n.enqueue(notifyJob{devices: devices})
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
	if j.discovery {
		path = "/callback/discovery"
	} else {
		payload["devices"] = j.devices
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
