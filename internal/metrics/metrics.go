// Package metrics - метрики шлюза для Prometheus. Все метрики лежат в одном реестре Registry,
// его отдаёт Handler на отдельном порту.
package metrics

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Registry - реестр метрик шлюза: свои метрики, метрики Go и процесса.
var Registry = prometheus.NewRegistry()

// Итоги входа: ok, неверный логин или пароль, адрес в бане, проверки паролей заняты.
const (
	LoginOK     = "ok"
	LoginWrong  = "wrong"
	LoginBanned = "banned"
	LoginBusy   = "busy"
)

// Итоги сохранения настроек: сохранено, ошибка во введённом, правка по старой версии
// или поверх ручной правки, ошибка записи или применения.
const (
	SaveOK       = "ok"
	SaveInvalid  = "invalid"
	SaveConflict = "conflict"
	SaveError    = "error"
)

// Итоги продления токена облака: продлён, сбой сети или ответа, облако отвергло токен.
const (
	RefreshOK       = "ok"
	RefreshError    = "error"
	RefreshRejected = "rejected"
)

// Итоги уведомления Яндексу: принято, отклонено ответом 4xx, не отправлено после повторов,
// отброшено из переполненной очереди.
const (
	NotifyOK       = "ok"
	NotifyRejected = "rejected"
	NotifyFailed   = "failed"
	NotifyDropped  = "dropped"
)

var (
	yandexRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_yandex_requests_total",
		Help: "Запросы Яндекса по обработчику и статусу HTTP.",
	}, []string{"handler", "code"})
	yandexDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hcgateway_yandex_request_duration_seconds",
		Help: "Время ответа Яндексу.",
		// Отметка 3 с - столько шлюз ждёт прибор при команде Алисы.
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2, 3, 5, 10},
	}, []string{"handler"})
	actions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_yandex_actions_total",
		Help: "Команды Алисы: DONE или код ошибки.",
	}, []string{"device", "capability", "result"})
	notifications = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_yandex_notifications_total",
		Help: "Уведомления Яндексу.",
	}, []string{"kind", "result"})
	deviceEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_device_events_total",
		Help: "События приборов.",
	}, []string{"device", "event"})
	webLogins = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_web_logins_total",
		Help: "Входы в веб-интерфейс.",
	}, []string{"result"})
	linkingLogins = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_linking_logins_total",
		Help: "Входы на странице привязки аккаунта Яндекса.",
	}, []string{"result"})
	bans = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hcgateway_auth_bans_total",
		Help: "Выданные баны.",
	})
	configSaves = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_config_saves_total",
		Help: "Сохранения настроек из веб-интерфейса.",
	}, []string{"result"})
	sourceRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_source_requests_total",
		Help: "Запросы к облакам источников по статусу HTTP; error - сбой сети.",
	}, []string{"source", "code"})
	sourceConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hcgateway_source_connected",
		Help: "1, если поток событий источника подключён.",
	}, []string{"source", "device"})
	sourceReconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_source_reconnects_total",
		Help: "Переподключения потоков событий источников.",
	}, []string{"source", "device"})
	tokenRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hcgateway_source_token_refreshes_total",
		Help: "Продления токена облака по итогу.",
	}, []string{"source", "result"})
	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hcgateway_build_info",
		Help: "Версия шлюза.",
	}, []string{"version"})
	state = &stateCollector{}

	// own - свои метрики шлюза, без метрик Go и процесса.
	own = []prometheus.Collector{yandexRequests, yandexDuration, actions, notifications, deviceEvents,
		webLogins, linkingLogins, bans, configSaves, sourceRequests, sourceConnected, sourceReconnects, tokenRefreshes, buildInfo, state}
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	Registry.MustRegister(own...)
	// Счётчики с известными метками начинаются с нуля: иначе rate() не видит первое событие.
	for _, r := range []string{LoginOK, LoginWrong, LoginBanned, LoginBusy} {
		webLogins.WithLabelValues(r)
		linkingLogins.WithLabelValues(r)
	}
	for _, r := range []string{SaveOK, SaveInvalid, SaveConflict, SaveError} {
		configSaves.WithLabelValues(r)
	}
}

// YandexHandler считает запросы Яндекса к обработчику name и время ответа.
func YandexHandler(name string, h http.Handler) http.Handler {
	labels := prometheus.Labels{"handler": name}
	return promhttp.InstrumentHandlerDuration(yandexDuration.MustCurryWith(labels),
		promhttp.InstrumentHandlerCounter(yandexRequests.MustCurryWith(labels), h))
}

// Action считает команду Алисы. result - DONE или код ошибки Яндекса.
func Action(device, capability, result string) {
	actions.WithLabelValues(device, capability, result).Inc()
}

// Notification считает уведомление Яндексу. kind - state или discovery.
func Notification(kind, result string) {
	notifications.WithLabelValues(kind, result).Inc()
}

// DeviceEvent считает событие прибора, например BSH.Common.Event.ProgramFinished.
func DeviceEvent(device, event string) {
	deviceEvents.WithLabelValues(device, event).Inc()
}

// Login считает вход: в веб-интерфейс, если web, иначе на странице привязки.
func Login(web bool, result string) {
	if web {
		webLogins.WithLabelValues(result).Inc()
	} else {
		linkingLogins.WithLabelValues(result).Inc()
	}
}

// Ban считает выданный бан.
func Ban() {
	bans.Inc()
}

// ConfigSave считает сохранение настроек.
func ConfigSave(result string) {
	configSaves.WithLabelValues(result).Inc()
}

// SourceRequest считает запрос к облаку источника. code - статус HTTP или error при сбое сети.
func SourceRequest(source, code string) {
	sourceRequests.WithLabelValues(source, code).Inc()
}

// SourceConnected отмечает, подключён ли поток событий источника. device пустой, если поток
// один на все приборы источника.
func SourceConnected(source, device string, up bool) {
	sourceConnected.WithLabelValues(source, device).Set(flag(up))
}

// SourceReconnect считает переподключение потока событий.
func SourceReconnect(source, device string) {
	sourceReconnects.WithLabelValues(source, device).Inc()
}

// TokenRefreshSource заводит счётчики продлений источника с нуля.
func TokenRefreshSource(source string) {
	for _, r := range []string{RefreshOK, RefreshError, RefreshRejected} {
		tokenRefreshes.WithLabelValues(source, r)
	}
}

// TokenRefresh считает продление токена облака.
func TokenRefresh(source, result string) {
	tokenRefreshes.WithLabelValues(source, result).Inc()
}

// SetBuildInfo задаёт версию шлюза.
func SetBuildInfo(version string) {
	buildInfo.Reset()
	buildInfo.WithLabelValues(version).Set(1)
}

// Device - состояние прибора для метрик.
type Device struct {
	// ID - идентификатор прибора в настройках, Source - его источник.
	ID, Source string
	Online     bool
	// Operation - состояние программы, например Run; пусто - у прибора его нет.
	Operation string
}

// State - то, что метрики спрашивают у работающего шлюза при каждом сборе.
type State interface {
	Linked() bool
	Banned() int
	Devices() []Device
	// TokenExpires - когда истекает токен облака, по источникам; нет входа - источника нет.
	TokenExpires() map[string]time.Time
}

// Watch задаёт, у кого метрики спрашивают состояние шлюза.
func Watch(s State) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.src = s
}

var (
	linkedDesc = prometheus.NewDesc("hcgateway_yandex_linked", "1, если аккаунт Яндекса привязан.", nil, nil)
	bannedDesc = prometheus.NewDesc("hcgateway_auth_banned", "Адреса в бане сейчас.", nil, nil)
	onlineDesc = prometheus.NewDesc("hcgateway_device_online", "1, если прибор на связи.", []string{"device", "source"}, nil)
	tokenDesc  = prometheus.NewDesc("hcgateway_source_token_expires_seconds", "Сколько секунд осталось до конца срока токена облака.",
		[]string{"source"}, nil)
	operationDesc = prometheus.NewDesc("hcgateway_device_operation_state", "1 у текущего состояния программы прибора.",
		[]string{"device", "state"}, nil)
)

// stateCollector собирает метрики состояния в момент сбора: удалённый прибор
// пропадает из метрик сразу, без устаревших рядов.
type stateCollector struct {
	mu  sync.Mutex
	src State
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- linkedDesc
	ch <- bannedDesc
	ch <- onlineDesc
	ch <- operationDesc
	ch <- tokenDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	src := c.src
	c.mu.Unlock()
	if src == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(linkedDesc, prometheus.GaugeValue, flag(src.Linked()))
	ch <- prometheus.MustNewConstMetric(bannedDesc, prometheus.GaugeValue, float64(src.Banned()))
	for source, expires := range src.TokenExpires() {
		ch <- prometheus.MustNewConstMetric(tokenDesc, prometheus.GaugeValue, time.Until(expires).Seconds(), source)
	}
	for _, d := range src.Devices() {
		ch <- prometheus.MustNewConstMetric(onlineDesc, prometheus.GaugeValue, flag(d.Online), d.ID, d.Source)
		if d.Online && d.Operation != "" {
			ch <- prometheus.MustNewConstMetric(operationDesc, prometheus.GaugeValue, 1, d.ID, d.Operation)
		}
	}
}

func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Handler отдаёт метрики на GET /metrics, только с заголовком Authorization: Bearer <token>:
// по метрикам видно, когда владелец дома и готовит. token спрашивается при каждом запросе:
// новый токен действует без перезапуска. Без токена или с неверным - 401 без тела.
func Handler(token func() string) http.Handler {
	mux := http.NewServeMux()
	metrics := promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !equal(got, token()) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		metrics.ServeHTTP(w, r)
	})
	return mux
}

// equal сравнивает строки за постоянное время. Пустое ожидаемое значение ничему не равно.
func equal(got, want string) bool {
	if want == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// Value - значение счётчика или датчика name с метками labels (имя, значение, ...).
// Нужна тестам пакетов, которые считают метрики. Нет такой метрики - 0.
func Value(name string, labels ...string) float64 {
	families, _ := Registry.Gather()
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if hasLabels(m, labels) {
				return m.GetCounter().GetValue() + m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func hasLabels(m *dto.Metric, labels []string) bool {
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	for _, l := range m.GetLabel() {
		if v, ok := want[l.GetName()]; ok && v != l.GetValue() {
			return false
		}
		delete(want, l.GetName())
	}
	return len(want) == 0
}
