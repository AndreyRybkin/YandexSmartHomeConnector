// Package mqttbroker - встроенный в шлюз брокер MQTT на comqtt: к нему подключаются приборы и
// другие клиенты домашней сети. Брокер общего назначения: топики не ограничены, вход - по логину и
// паролю из настроек. Шифрования нет, поэтому порт нельзя публиковать в интернет.
// Страница /mqtt показывает подключения и сообщения: их хранит этот пакет, в памяти.
package mqttbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	mqtt "github.com/wind-c/comqtt/v2/mqtt"
	"github.com/wind-c/comqtt/v2/mqtt/hooks/storage"
	"github.com/wind-c/comqtt/v2/mqtt/listeners"
	"github.com/wind-c/comqtt/v2/mqtt/packets"

	"hcgateway/internal/metrics"
	"hcgateway/internal/statedb"
)

const (
	// maxConns - предел соединений: до входа они тоже занимают память.
	maxConns = 64
	// maxPacket - предел сообщения MQTT.
	maxPacket = 64 << 10
	// journalSize и failuresSize - сколько последних сообщений и неудачных входов показывает страница.
	journalSize  = 200
	failuresSize = 20
	// maxTopics - сколько топиков с текущим значением помнит страница.
	maxTopics = 2000
	// maxShown - сколько байт значения показывает страница.
	maxShown = 1024
	// MaxTopicBytes и MaxPayloadBytes - пределы сообщения из интерфейса.
	MaxTopicBytes   = 256
	MaxPayloadBytes = statedb.MaxRetainedBytes
	// sysEvery - как часто брокер обновляет свои топики $SYS, в секундах.
	sysEvery = 60
)

// connectWait - сколько соединение может молчать до входа: иначе соединения без входа заняли бы все
// места. Тесты его сокращают.
var connectWait = 10 * time.Second

// Направления сообщения.
const (
	// In - сообщение от клиента.
	In = "in"
	// Out - сообщение от самого шлюза.
	Out = "out"
)

// ErrClosed - брокер не запущен.
var ErrClosed = errors.New("брокер MQTT не запущен")

// Config - настройки брокера. Credentials спрашивается при каждом входе: новый пароль действует сразу.
type Config struct {
	Listen      string
	Credentials func() (user, password string)
}

// Client - подключённый клиент.
type Client struct {
	ID, Remote, User string
	// Version - версия протокола: 3 - MQTT 3.1, 4 - 3.1.1, 5 - 5.0.
	Version byte
	Since   time.Time
}

// Entry - сообщение в журнале.
type Entry struct {
	Time               time.Time
	Dir, Client        string
	Topic, Payload     string
	QoS                byte
	Retain, PayloadCut bool
}

// Value - последнее значение топика.
type Value struct {
	Topic, Payload, Client string
	Time                   time.Time
	Retain                 bool
	// Saved - значение из базы: с запуска шлюза его никто не присылал.
	Saved bool
}

// Failure - неудачный вход.
type Failure struct {
	Time         time.Time
	Remote, User string
}

// Status - состояние брокера для страницы /mqtt.
type Status struct {
	// Open - порт открыт; Err - почему не открылся.
	Open     bool
	Err      string
	Clients  []Client
	Journal  []Entry
	Values   []Value
	Failures []Failure
	// Saved - сколько сохранённых сообщений лежит в базе; Unsaved - скольких не сохранили из-за предела.
	Saved, Unsaved int
}

// Broker - встроенный брокер.
type Broker struct {
	conf  Config
	state *statedb.DB
	log   *slog.Logger
	srv   *mqtt.Server
	// wait - сколько соединение может молчать до входа.
	wait time.Duration

	mu       sync.Mutex
	open     bool
	failure  string
	clients  map[*mqtt.Client]Client
	journal  []Entry
	values   map[string]Value
	failures []Failure
	unsaved  int
}

// New создаёт брокер. Порт открывает Run.
func New(conf Config, state *statedb.DB, log *slog.Logger) *Broker {
	b := &Broker{conf: conf, state: state, log: log, wait: connectWait, clients: map[*mqtt.Client]Client{}, values: map[string]Value{}}
	for _, r := range state.Retained() {
		b.values[r.Topic] = Value{Topic: r.Topic, Payload: shown(r.Payload), Time: r.Time, Retain: true, Saved: true}
	}
	// Журнал comqtt подробен для домашнего шлюза: важное пишет этот пакет.
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	caps := *mqtt.DefaultServerCapabilities
	caps.MaximumPacketSize = maxPacket
	b.srv = mqtt.New(&mqtt.Options{Capabilities: &caps, InlineClient: true, Logger: quiet, SysTopicResendInterval: sysEvery})
	return b
}

// Run открывает порт и держит брокер, пока ctx не отменён. Ошибка порта не мешает шлюзу: Алиса и
// остальные приборы работают, а страница /mqtt показывает причину.
func (b *Broker) Run(ctx context.Context) {
	if err := b.start(); err != nil {
		b.log.Error("брокер MQTT не запущен", "listen", b.conf.Listen, "err", err)
		b.mu.Lock()
		b.failure = err.Error()
		b.mu.Unlock()
		return
	}
	b.log.Info("брокер MQTT запущен", "listen", b.conf.Listen)
	<-ctx.Done()
	b.mu.Lock()
	b.open = false
	b.mu.Unlock()
	b.srv.Close()
}

func (b *Broker) start() error {
	if err := b.srv.AddHook(&hook{b: b}, nil); err != nil {
		return err
	}
	l, err := net.Listen("tcp", b.conf.Listen)
	if err != nil {
		return err
	}
	if err := b.srv.AddListener(listeners.NewNet("hcgateway", &limitListener{Listener: l, slots: make(chan struct{}, maxConns), wait: b.wait})); err != nil {
		l.Close()
		return err
	}
	if err := b.srv.Serve(); err != nil {
		return err
	}
	b.mu.Lock()
	b.open = true
	b.mu.Unlock()
	return nil
}

// Status - состояние брокера; топики с prefix, если он задан.
func (b *Broker) Status(prefix string) Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{Open: b.open, Err: b.failure, Saved: b.state.RetainedCount(), Unsaved: b.unsaved, Failures: slices.Clone(b.failures)}
	for _, c := range b.clients {
		st.Clients = append(st.Clients, c)
	}
	slices.SortFunc(st.Clients, func(x, y Client) int { return strings.Compare(x.ID, y.ID) })
	for i := len(b.journal) - 1; i >= 0; i-- {
		if strings.HasPrefix(b.journal[i].Topic, prefix) {
			st.Journal = append(st.Journal, b.journal[i])
		}
	}
	for _, t := range slices.Sorted(maps.Keys(b.values)) {
		if strings.HasPrefix(t, prefix) {
			st.Values = append(st.Values, b.values[t])
		}
	}
	return st
}

// Publish отправляет сообщение от имени шлюза.
func (b *Broker) Publish(topic string, payload []byte, retain bool, qos byte) error {
	b.mu.Lock()
	open := b.open
	b.mu.Unlock()
	if !open {
		return ErrClosed
	}
	if err := b.srv.Publish(topic, payload, retain, qos); err != nil {
		return err
	}
	metrics.MQTTMessage(Out)
	b.remember(Entry{Dir: Out, Client: "шлюз", Topic: topic, Payload: shown(payload), QoS: qos, Retain: retain, PayloadCut: len(payload) > maxShown})
	return nil
}

// remember записывает сообщение в журнал и в текущие значения.
func (b *Broker) remember(e Entry) {
	e.Time = time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.journal = append(b.journal, e)
	if n := len(b.journal) - journalSize; n > 0 {
		b.journal = slices.Clone(b.journal[n:])
	}
	if _, known := b.values[e.Topic]; known || len(b.values) < maxTopics {
		b.values[e.Topic] = Value{Topic: e.Topic, Payload: e.Payload, Client: e.Client, Time: e.Time, Retain: e.Retain}
	}
}

// shown - значение для страницы: действительный UTF-8, не длиннее maxShown.
func shown(payload []byte) string {
	if len(payload) > maxShown {
		payload = payload[:maxShown]
	}
	return strings.ToValidUTF8(string(payload), "�")
}

// equal сравнивает строки за постоянное время. Пустое ожидаемое значение ничему не равно.
func equal(got []byte, want string) bool {
	if want == "" {
		return false
	}
	a, b := sha256.Sum256(got), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// hook подключает брокер к событиям comqtt.
type hook struct {
	mqtt.HookBase
	b *Broker
}

func (h *hook) ID() string { return "hcgateway" }

func (h *hook) Provides(event byte) bool {
	return bytes.IndexByte([]byte{mqtt.OnConnectAuthenticate, mqtt.OnACLCheck, mqtt.OnSessionEstablished, mqtt.OnDisconnect,
		mqtt.OnPublished, mqtt.OnRetainMessage, mqtt.StoredRetainedMessages}, event) >= 0
}

// OnConnectAuthenticate проверяет логин и пароль. Причин отказа клиент не узнаёт.
func (h *hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	user, password := h.b.conf.Credentials()
	// Проверяются оба значения: время ответа не выдаёт, что логин верный.
	ok := equal(pk.Connect.Username, user)
	ok = equal(pk.Connect.Password, password) && ok
	if ok {
		return true
	}
	metrics.MQTTAuthFailure()
	f := Failure{Time: time.Now(), Remote: cl.Net.Remote, User: shown(pk.Connect.Username)}
	h.b.log.Warn("брокер MQTT: неудачный вход", "addr", f.Remote)
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	h.b.failures = append([]Failure{f}, h.b.failures...)
	if len(h.b.failures) > failuresSize {
		h.b.failures = h.b.failures[:failuresSize]
	}
	return false
}

// OnACLCheck разрешает вошедшему клиенту все топики: брокер общего назначения.
func (h *hook) OnACLCheck(*mqtt.Client, string, bool) bool { return true }

func (h *hook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	c := Client{ID: cl.ID, Remote: cl.Net.Remote, User: shown(cl.Properties.Username), Version: cl.Properties.ProtocolVersion, Since: time.Now()}
	h.b.mu.Lock()
	h.b.clients[cl] = c
	n := len(h.b.clients)
	h.b.mu.Unlock()
	metrics.MQTTClients(n)
	h.b.log.Info("брокер MQTT: клиент подключён", "client", c.ID, "addr", c.Remote, "protocol", c.Version)
}

func (h *hook) OnDisconnect(cl *mqtt.Client, _ error, _ bool) {
	h.b.mu.Lock()
	delete(h.b.clients, cl)
	n := len(h.b.clients)
	h.b.mu.Unlock()
	metrics.MQTTClients(n)
}

func (h *hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	// Сообщения шлюза записывает Publish.
	if cl.Net.Inline || strings.HasPrefix(pk.TopicName, "$") {
		return
	}
	metrics.MQTTMessage(In)
	h.b.remember(Entry{Dir: In, Client: cl.ID, Topic: pk.TopicName, Payload: shown(pk.Payload), QoS: pk.FixedHeader.Qos,
		Retain: pk.FixedHeader.Retain, PayloadCut: len(pk.Payload) > maxShown})
}

// OnRetainMessage передаёт сохранённое сообщение в базу. Пустое сообщение стирает топик.
func (h *hook) OnRetainMessage(_ *mqtt.Client, pk packets.Packet, _ int64) {
	if strings.HasPrefix(pk.TopicName, "$") {
		return
	}
	if len(pk.Payload) == 0 {
		h.b.state.DeleteRetained(pk.TopicName)
		h.b.mu.Lock()
		delete(h.b.values, pk.TopicName)
		h.b.mu.Unlock()
		return
	}
	if !h.b.state.SetRetained(pk.TopicName, pk.Payload, pk.FixedHeader.Qos, time.Now()) {
		h.b.mu.Lock()
		h.b.unsaved++
		h.b.mu.Unlock()
		h.b.log.Warn("брокер MQTT: сообщение не сохранено, превышен предел", "topic", shown([]byte(pk.TopicName)))
	}
}

// StoredRetainedMessages отдаёт брокеру сохранённые сообщения при запуске.
func (h *hook) StoredRetainedMessages() ([]storage.Message, error) {
	saved := h.b.state.Retained()
	out := make([]storage.Message, 0, len(saved))
	for _, r := range saved {
		out = append(out, storage.Message{ID: "retained_" + r.Topic, T: "retained", TopicName: r.Topic, Payload: r.Payload,
			Created: r.Time.Unix(), FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true, Qos: r.QoS}})
	}
	return out, nil
}

// limitListener ограничивает число соединений и время до входа.
type limitListener struct {
	net.Listener
	slots chan struct{}
	wait  time.Duration
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	// После входа comqtt сам обновляет срок чтения по keep-alive клиента.
	c.SetReadDeadline(time.Now().Add(l.wait))
	return &limitConn{Conn: c, release: sync.OnceFunc(func() { <-l.slots })}, nil
}

type limitConn struct {
	net.Conn
	release func()
}

func (c *limitConn) Close() error {
	c.release()
	return c.Conn.Close()
}
