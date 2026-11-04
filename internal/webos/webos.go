// Package webos - телевизоры LG webOS в домашней сети. Шлюз говорит с телевизором по
// протоколу SSAP через WebSocket так же, как приложение LG и библиотека aiowebostv, на которой
// работает интеграция Home Assistant: регистрация с подтверждением на экране, подписки на
// состояние, запросы ssap://. Телевизор для шлюза - прибор по профилю с функциями TV.*.
package webos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/appliance"
	"hcgateway/internal/atomicfile"
	"hcgateway/internal/metrics"
	"hcgateway/internal/tv"
)

// SourceName - источник в метриках.
const SourceName = "webos"

// Type - тип прибора для профиля.
const Type = "LGTV"

const (
	// Порты SSAP: wss - у новых телевизоров, ws - у старых.
	wssPort = "3001"
	wsPort  = "3000"
	// dialTimeout - предел подключения к телевизору.
	dialTimeout = 5 * time.Second
	// pairTimeout - сколько шлюз ждёт, пока владелец нажмёт «Разрешить» на экране.
	pairTimeout = time.Minute
	// requestTimeout - предел ответа на запрос, если у команды нет своего срока.
	requestTimeout = 10 * time.Second
	// pingEvery и readIdle - проверка соединения: соединение, из которого 70 с ничего не
	// пришло, оборвано.
	pingEvery = 30 * time.Second
	readIdle  = 70 * time.Second
	// retryOff - как часто проверять выключенный телевизор, как в Home Assistant.
	retryOff = 10 * time.Second
	// retryWake и wakeWindow - после включения по сети шлюз минуту проверяет телевизор чаще.
	retryWake  = 2 * time.Second
	wakeWindow = time.Minute
	// retryPair - пауза после отказа или неподтверждённого разрешения: телевизор не должен
	// спрашивать каждые 10 с. «Подключить заново» на странице прибора её прерывает.
	retryPair = 10 * time.Minute
	// maxMessage - предел сообщения телевизора: список каналов бывает большим.
	maxMessage = 8 << 20
	// maxPairingFile - предел файла с ключом телевизора.
	maxPairingFile = 64 << 10
	// liveTVApp - приложение эфирного телевидения.
	liveTVApp = "com.webos.app.livetv"
)

// URI запросов SSAP, как в aiowebostv.
const (
	uriSystemInfo  = "ssap://system/getSystemInfo"
	uriPowerState  = "ssap://com.webos.service.tvpower/power/getPowerState"
	uriForeground  = "ssap://com.webos.applicationManager/getForegroundAppInfo"
	uriVolume      = "ssap://audio/getVolume"
	uriAudio       = "ssap://audio/getStatus"
	uriInputs      = "ssap://tv/getExternalInputList"
	uriChannel     = "ssap://tv/getCurrentChannel"
	uriChannels    = "ssap://tv/getChannelList"
	uriMedia       = "ssap://com.webos.media/getForegroundAppInfo"
	uriConnection  = "ssap://com.webos.service.connectionmanager/getinfo"
	uriTurnOff     = "ssap://system/turnOff"
	uriScreenOn    = "ssap://com.webos.service.tvpower/power/turnOnScreen"
	uriScreenOff   = "ssap://com.webos.service.tvpower/power/turnOffScreen"
	uriSetVolume   = "ssap://audio/setVolume"
	uriVolumeUp    = "ssap://audio/volumeUp"
	uriVolumeDown  = "ssap://audio/volumeDown"
	uriSetMute     = "ssap://audio/setMute"
	uriOpenChannel = "ssap://tv/openChannel"
	uriChannelUp   = "ssap://tv/channelUp"
	uriChannelDown = "ssap://tv/channelDown"
	uriSwitchInput = "ssap://tv/switchInput"
	uriLaunch      = "ssap://com.webos.applicationManager/launch"
	uriPlay        = "ssap://media.controls/play"
	uriPause       = "ssap://media.controls/pause"
)

// permissions - права, которые шлюз просит при регистрации: как у aiowebostv.
var permissions = []string{"APP_TO_APP", "CLOSE", "CONTROL_AUDIO", "CONTROL_DISPLAY", "CONTROL_INPUT_JOYSTICK",
	"CONTROL_INPUT_MEDIA_PLAYBACK", "CONTROL_INPUT_MEDIA_RECORDING", "CONTROL_INPUT_TEXT", "CONTROL_INPUT_TV",
	"CONTROL_MOUSE_AND_KEYBOARD", "CONTROL_POWER", "CONTROL_TV_SCREEN", "LAUNCH", "LAUNCH_WEBAPP", "READ_APP_STATUS",
	"READ_COUNTRY_INFO", "READ_CURRENT_CHANNEL", "READ_INPUT_DEVICE_LIST", "READ_INSTALLED_APPS", "READ_LGE_SDX",
	"READ_LGE_TV_INPUT_EVENTS", "READ_NETWORK_STATE", "READ_NOTIFICATIONS", "READ_POWER_STATE", "READ_RUNNING_APPS",
	"READ_SETTINGS", "READ_TV_CHANNEL_LIST", "READ_TV_CURRENT_TIME", "READ_UPDATE_INFO", "SEARCH", "TEST_OPEN",
	"TEST_PROTECTED", "TEST_SECURE", "UPDATE_FROM_REMOTE_APP", "WRITE_NOTIFICATION_ALERT", "WRITE_NOTIFICATION_TOAST",
	"WRITE_SETTINGS"}

// offStates - состояния питания, в которых телевизор выключен, как в aiowebostv.
var offStates = []string{"", "Power Off", "Suspend", "Active Standby"}

// screenOffState - состояние питания включённого телевизора с погашенным экраном, как в aiowebostv.
const screenOffState = "Screen Off"

// Ошибки подключения: их видно на странице прибора.
var (
	errPairRefused = errors.New("телевизор отклонил подключение шлюза")
	errPairTimeout = errors.New("на телевизоре не нажали «Разрешить» за минуту")
)

// pairing - что шлюз помнит о телевизоре: ключ клиента, MAC, модель и входы. Лежит в
// data_dir/webos/<id>.json. Входы шлюз помнит, чтобы источники сигнала в Алисе не
// пропадали, пока телевизор выключен.
type pairing struct {
	ClientKey string   `json:"client_key,omitempty"`
	MAC       string   `json:"mac,omitempty"`
	Model     string   `json:"model,omitempty"`
	Inputs    []string `json:"inputs,omitempty"`
	// Cert - отпечаток сертификата, с которым шлюз отправляет ключ.
	Cert string `json:"cert,omitempty"`
}

// Config - телевизор в настройках.
type Config struct {
	// ID - идентификатор прибора: по нему называется файл ключа.
	ID string
	// Host - IP телевизора, порт по желанию: без порта - wss 3001, затем ws 3000.
	Host string
	// MAC - MAC для включения по сети; пусто - тот, что сообщил телевизор.
	MAC string
	// Broadcast - широковещательный адрес для Wake-on-LAN; пусто - 255.255.255.255.
	Broadcast string
	// Apps - идентификаторы приложений, которые Алиса может запускать; пусто - режима приложений нет.
	Apps []string
	// Dir - каталог файлов телевизоров, data_dir/webos.
	Dir string
	// Changed вызывается, когда шлюз узнал новые входы телевизора: Алисе нужен новый список
	// устройств. nil - не нужно.
	Changed func()
}

// Status - связь с телевизором для страницы прибора.
type Status struct {
	// Paired - ключ клиента получен; Prompt - телевизор спрашивает разрешение сейчас.
	Paired, Prompt, Connected bool
	Model, MAC                string
	// Err - почему нет связи, словами; пусто - ошибки нет.
	Err string
	// Cert - запомненный отпечаток сертификата; NewCert - телевизор предъявил другой и ждёт решения
	// владельца; Plain - соединение без шифрования: у модели нет wss.
	Cert, NewCert string
	Plain         bool
}

var _ appliance.Appliance = (*TV)(nil)
var _ appliance.Stepper = (*TV)(nil)

// TV - телевизор LG как прибор по профилю. Пока шлюз не подключён к телевизору, тот
// выключен: включает его пакет Wake-on-LAN.
type TV struct {
	cfg   Config
	log   *slog.Logger
	retry chan struct{}

	mu      sync.Mutex
	pair    pairing
	status  Status
	conn    *session
	values  map[string]any
	inputs  []input
	numbers map[int]string
	wokeAt  time.Time
	notify  appliance.Notify
	runs    int
}

// input - вход телевизора: id для tv/switchInput и appId приложения входа.
type input struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	AppID string `json:"appId"`
}

// New открывает телевизор: ключ и MAC - из data_dir/webos/<id>.json, если телевизор уже
// разрешил подключение.
func New(cfg Config, log *slog.Logger) (*TV, error) {
	t := &TV{cfg: cfg, log: log, retry: make(chan struct{}, 1), values: map[string]any{}}
	data, err := os.ReadFile(t.path())
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case len(data) > maxPairingFile:
		return nil, fmt.Errorf("%s больше %d байт", t.path(), maxPairingFile)
	default:
		if err := json.Unmarshal(data, &t.pair); err != nil {
			return nil, fmt.Errorf("%s: файл телевизора не разобран: %w", t.path(), err)
		}
	}
	t.status = Status{Paired: t.pair.ClientKey != "", Model: t.pair.Model, MAC: t.mac(), Cert: t.pair.Cert}
	return t, nil
}

func (t *TV) path() string {
	return filepath.Join(t.cfg.Dir, t.cfg.ID+".json")
}

// saveLocked пишет файл телевизора; t.mu захвачен.
func (t *TV) saveLocked() error {
	data, err := json.MarshalIndent(t.pair, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.cfg.Dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(t.path(), data)
}

// mac - MAC для включения: из настроек, иначе тот, что сообщил телевизор.
func (t *TV) mac() string {
	if t.cfg.MAC != "" {
		return t.cfg.MAC
	}
	return t.pair.MAC
}

// Status - связь с телевизором для страницы прибора.
func (t *TV) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// Retry подключается к телевизору заново сразу, не дожидаясь паузы.
func (t *TV) Retry() {
	select {
	case t.retry <- struct{}{}:
	default:
	}
}

// Forget забывает ключ: при следующем подключении телевизор снова спросит разрешение.
func (t *TV) Forget() error {
	t.mu.Lock()
	t.pair.ClientKey, t.pair.Cert = "", ""
	t.status.Paired, t.status.Cert, t.status.NewCert = false, "", ""
	err := t.saveLocked()
	conn := t.conn
	t.mu.Unlock()
	if conn != nil {
		conn.ws.Close()
	}
	t.Retry()
	return err
}

// AcceptCert запоминает новый отпечаток, который подтвердил владелец. Подойдёт только тот, что
// показан на странице: если сертификат успел смениться ещё раз, решение устарело.
func (t *TV) AcceptCert(fp string) error {
	t.mu.Lock()
	if fp == "" || fp != t.status.NewCert {
		t.mu.Unlock()
		return tv.ErrCertChanged
	}
	t.pair.Cert, t.status.Cert, t.status.NewCert = fp, fp, ""
	err := t.saveLocked()
	t.mu.Unlock()
	t.Retry()
	return err
}

// certSeen запоминает, как прошло соединение: первый отпечаток и шифрование.
func (t *TV) certSeen(url string, check *tv.CertCheck) {
	// Поиск в сети подключается без настроек прибора: ему запоминать нечего.
	if t.cfg.ID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Plain = !tv.Encrypted(url)
	if !tv.Encrypted(url) || t.pair.Cert != "" {
		return
	}
	t.pair.Cert, t.status.Cert = check.Seen(), check.Seen()
	if err := t.saveLocked(); err != nil {
		t.log.Warn("отпечаток сертификата телевизора не сохранён", "device", t.cfg.ID, "err", err)
	}
}

// certChanged запоминает чужой сертификат для решения владельца.
func (t *TV) certChanged(fp string) {
	t.mu.Lock()
	fresh := t.status.NewCert != fp
	t.status.NewCert = fp
	t.mu.Unlock()
	if fresh {
		t.log.Warn("сертификат телевизора изменился, ключ не отправлен", "device", t.cfg.ID)
		t.changed()
	}
}

func (t *TV) Describe() appliance.Description {
	t.mu.Lock()
	inputs := append([]string{tv.LiveTV}, t.pair.Inputs...)
	model := t.pair.Model
	t.mu.Unlock()
	d := appliance.Description{Type: Type, Model: model, Options: map[string]appliance.Option{
		tv.Power:    {Key: tv.Power, Values: []string{tv.On, tv.Off}, Setting: true},
		tv.Volume:   {Key: tv.Volume, Min: 0, Max: 100, Step: 1, Setting: true},
		tv.Mute:     {Key: tv.Mute, Setting: true},
		tv.Channel:  {Key: tv.Channel, Min: 1, Max: tv.MaxChannel, Step: 1, Setting: true},
		tv.Input:    {Key: tv.Input, Values: inputs, Setting: true},
		tv.Playback: {Key: tv.Playback, Values: []string{tv.Playing, tv.Paused}, Setting: true},
		tv.Screen:   {Key: tv.Screen, Values: []string{tv.On, tv.Off}, Setting: true},
	}, Reports: []string{tv.Power, tv.Volume, tv.Mute, tv.Channel, tv.Input, tv.Playback, tv.Screen, tv.App}}
	if len(t.cfg.Apps) > 0 {
		d.Options[tv.App] = appliance.Option{Key: tv.App, Values: slices.Clone(t.cfg.Apps), Setting: true}
	}
	return d
}

// State - состояние телевизора. Телевизор, который разрешил подключение, на связи и
// выключенным: «включи» отправит пакет Wake-on-LAN.
func (t *TV) State() appliance.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	values := maps.Clone(t.values)
	if t.conn == nil {
		values = map[string]any{tv.Power: tv.Off}
	}
	return appliance.State{Online: t.status.Paired, Values: values}
}

// Run держит связь с телевизором, пока ctx не отменён. Выключенный телевизор шлюз проверяет
// раз в 10 с, а минуту после включения по сети - раз в 2 с.
func (t *TV) Run(ctx context.Context, notify appliance.Notify) {
	t.mu.Lock()
	t.runs++
	gen := t.runs
	t.notify = notify
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		if t.runs == gen {
			t.notify = nil
		}
		t.mu.Unlock()
	}()
	for {
		err := t.session(ctx)
		if ctx.Err() != nil {
			return
		}
		pause := retryOff
		t.mu.Lock()
		if time.Since(t.wokeAt) < wakeWindow {
			pause = retryWake
		}
		t.mu.Unlock()
		if errors.Is(err, errPairRefused) || errors.Is(err, errPairTimeout) {
			pause = retryPair
		}
		select {
		case <-ctx.Done():
			return
		case <-t.retry:
		case <-time.After(pause):
		}
	}
}

// changed сообщает об изменении состояния.
func (t *TV) changed() {
	t.mu.Lock()
	notify := t.notify
	t.mu.Unlock()
	if notify != nil {
		notify(nil)
	}
}

// setStatus меняет связь для страницы прибора и сообщает об изменении.
func (t *TV) setStatus(f func(s *Status)) {
	t.mu.Lock()
	f(&t.status)
	t.mu.Unlock()
	t.changed()
}

// dial подключается к телевизору: без порта в адресе - wss 3001, затем ws 3000. Когда отпечаток
// сертификата запомнен, остаётся только wss с этим отпечатком (tv.CertCheck).
func (t *TV) dial(ctx context.Context) (*websocket.Conn, error) {
	host, port, err := net.SplitHostPort(t.cfg.Host)
	urls := []string{}
	if err == nil {
		urls = append(urls, "wss://"+net.JoinHostPort(host, port), "ws://"+net.JoinHostPort(host, port))
	} else {
		host = strings.Trim(t.cfg.Host, "[]")
		urls = append(urls, "wss://"+net.JoinHostPort(host, wssPort), "ws://"+net.JoinHostPort(host, wsPort))
	}
	t.mu.Lock()
	pin := t.pair.Cert
	t.mu.Unlock()
	check := tv.NewCertCheck(pin)
	d := websocket.Dialer{HandshakeTimeout: dialTimeout, TLSClientConfig: check.TLSConfig()}
	var last error
	for _, u := range urls {
		if pin != "" && !tv.Encrypted(u) {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		ws, resp, err := d.DialContext(dctx, u, http.Header{})
		cancel()
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if err == nil {
			ws.SetReadLimit(maxMessage)
			t.certSeen(u, check)
			return ws, nil
		}
		if errors.Is(err, tv.ErrCertChanged) {
			t.certChanged(check.Seen())
			return nil, err
		}
		last = err
	}
	return nil, last
}

// session - одно соединение с телевизором: регистрация, подписки и ожидание обрыва.
func (t *TV) session(ctx context.Context) error {
	ws, err := t.dial(ctx)
	if err != nil {
		// Выключенный телевизор не принимает соединение: это не ошибка.
		t.setStatus(func(s *Status) { s.Connected, s.Prompt = false, false })
		return err
	}
	s := newSession(ws)
	go s.read()
	defer func() {
		ws.Close()
		<-s.done
		t.mu.Lock()
		t.conn = nil
		t.values = map[string]any{}
		t.mu.Unlock()
		t.setStatus(func(st *Status) { st.Connected, st.Prompt = false, false })
	}()
	if err := t.register(ctx, s); err != nil {
		t.log.Warn("webOS: телевизор не подключён", "device", t.cfg.ID, "err", err)
		t.setStatus(func(st *Status) { st.Err = err.Error() })
		return err
	}
	metrics.SourceConnected(SourceName, t.cfg.ID, true)
	defer metrics.SourceConnected(SourceName, t.cfg.ID, false)
	t.mu.Lock()
	t.conn = s
	t.values = map[string]any{tv.Power: tv.On}
	t.mu.Unlock()
	t.setStatus(func(st *Status) { st.Connected, st.Prompt, st.Err = true, false, "" })
	t.log.Info("webOS: телевизор подключён", "device", t.cfg.ID)
	t.subscribe(ctx, s)
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			metrics.SourceReconnect(SourceName, t.cfg.ID)
			return s.err
		case <-ping.C:
			if err := s.ping(); err != nil {
				return err
			}
		}
	}
}

// register представляется телевизору. Без ключа телевизор спрашивает разрешение на экране,
// и шлюз ждёт до минуты; новый ключ шлюз сохраняет.
func (t *TV) register(ctx context.Context, s *session) error {
	if _, err := s.call(ctx, "hello", "hello", "", nil, dialTimeout); err != nil {
		t.log.Debug("webOS: телевизор не ответил на hello", "device", t.cfg.ID, "err", err)
	}
	if raw, err := s.call(ctx, "request", "get_sys_info", uriSystemInfo, nil, dialTimeout); err == nil {
		var info struct {
			ModelName string `json:"modelName"`
		}
		if json.Unmarshal(raw, &info) == nil && info.ModelName != "" {
			t.mu.Lock()
			t.pair.Model = info.ModelName
			t.status.Model = info.ModelName
			t.mu.Unlock()
		}
	}
	t.mu.Lock()
	key := t.pair.ClientKey
	t.mu.Unlock()
	payload := map[string]any{"forcePairing": false, "pairingType": "PROMPT",
		"manifest": map[string]any{"manifestVersion": 1, "appVersion": "1.1", "permissions": permissions}}
	if key != "" {
		payload["client-key"] = key
	}
	replies, err := s.start("register", "register_0", "", payload)
	if err != nil {
		return err
	}
	defer s.stop("register_0")
	timer := time.NewTimer(dialTimeout + requestTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return s.err
		case <-timer.C:
			t.mu.Lock()
			prompted := t.status.Prompt
			t.mu.Unlock()
			if prompted {
				return errPairTimeout
			}
			return errors.New("телевизор не ответил на регистрацию")
		case m := <-replies:
			switch m.Type {
			case "error":
				return fmt.Errorf("%w: %s", errPairRefused, m.Error)
			case "response":
				var p struct {
					PairingType string `json:"pairingType"`
				}
				if json.Unmarshal(m.Payload, &p) == nil && p.PairingType == "PROMPT" {
					t.log.Info("webOS: телевизор спрашивает разрешение на экране", "device", t.cfg.ID)
					t.setStatus(func(st *Status) { st.Prompt = true })
					timer.Reset(pairTimeout)
				}
			case "registered":
				var p struct {
					Key string `json:"client-key"`
				}
				if err := json.Unmarshal(m.Payload, &p); err != nil || p.Key == "" {
					return errors.New("телевизор не прислал ключ клиента")
				}
				t.mu.Lock()
				defer t.mu.Unlock()
				t.status.Paired, t.status.Prompt = true, false
				if p.Key == t.pair.ClientKey {
					return nil
				}
				t.pair.ClientKey = p.Key
				if err := t.saveLocked(); err != nil {
					return fmt.Errorf("ключ телевизора не сохранён: %w", err)
				}
				return nil
			}
		}
	}
}

// subscribe подписывается на состояние телевизора и узнаёт его MAC и каналы. Подписку,
// которую телевизор не поддерживает, шлюз пропускает: у старых webOS нет состояния питания.
func (t *TV) subscribe(ctx context.Context, s *session) {
	subs := []struct {
		uri    string
		handle func(json.RawMessage)
	}{
		{uriPowerState, t.onPower}, {uriForeground, t.onApp}, {uriVolume, t.onVolume}, {uriAudio, t.onAudio},
		{uriInputs, t.onInputs}, {uriChannel, t.onChannel}, {uriMedia, t.onMedia}, {uriChannels, t.onChannels},
	}
	for i, sub := range subs {
		if err := s.subscribe(ctx, "sub_"+strconv.Itoa(i), sub.uri, sub.handle); err != nil {
			t.log.Debug("webOS: подписка не оформлена", "device", t.cfg.ID, "uri", sub.uri, "err", err)
		}
	}
	if raw, err := s.call(ctx, "request", s.nextID(), uriConnection, nil, requestTimeout); err == nil {
		t.onConnection(raw)
	}
}

// update меняет значения телевизора и сообщает об изменении.
func (t *TV) update(f func(v map[string]any)) {
	t.mu.Lock()
	if t.conn == nil {
		t.mu.Unlock()
		return
	}
	f(t.values)
	t.mu.Unlock()
	t.changed()
}

func (t *TV) onPower(raw json.RawMessage) {
	var p struct {
		State string `json:"state"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	t.update(func(v map[string]any) {
		v[tv.Power] = tv.On
		if slices.Contains(offStates, p.State) {
			v[tv.Power] = tv.Off
			delete(v, tv.Screen)
			return
		}
		v[tv.Screen] = tv.On
		if p.State == screenOffState {
			v[tv.Screen] = tv.Off
		}
	})
}

func (t *TV) onApp(raw json.RawMessage) {
	var p struct {
		AppID string `json:"appId"`
	}
	if json.Unmarshal(raw, &p) != nil || p.AppID == "" {
		return
	}
	t.update(func(v map[string]any) {
		v[tv.App] = p.AppID
		delete(v, tv.Input)
		if p.AppID == liveTVApp {
			v[tv.Input] = tv.LiveTV
			return
		}
		for _, in := range t.inputs {
			if in.AppID == p.AppID {
				v[tv.Input] = in.ID
			}
		}
	})
}

func (t *TV) onVolume(raw json.RawMessage) {
	var p struct {
		Volume *float64 `json:"volume"`
		Status *struct {
			Volume *float64 `json:"volume"`
		} `json:"volumeStatus"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	vol := p.Volume
	if p.Status != nil && p.Status.Volume != nil {
		vol = p.Status.Volume
	}
	if vol == nil {
		return
	}
	t.update(func(v map[string]any) { v[tv.Volume] = *vol })
}

func (t *TV) onAudio(raw json.RawMessage) {
	var p struct {
		Mute *bool `json:"mute"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Mute == nil {
		return
	}
	t.update(func(v map[string]any) { v[tv.Mute] = *p.Mute })
}

// onInputs запоминает входы телевизора. Новый список входов - новые режимы Алисы.
func (t *TV) onInputs(raw json.RawMessage) {
	var p struct {
		Devices []input `json:"devices"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	var ids []string
	for _, in := range p.Devices {
		if in.ID != "" && len(in.ID) <= 64 {
			ids = append(ids, in.ID)
		}
	}
	t.mu.Lock()
	t.inputs = p.Devices
	changed := !slices.Equal(ids, t.pair.Inputs)
	if changed {
		t.pair.Inputs = ids
		if err := t.saveLocked(); err != nil {
			t.log.Warn("webOS: входы телевизора не сохранены", "device", t.cfg.ID, "err", err)
		}
	}
	t.mu.Unlock()
	if changed && t.cfg.Changed != nil {
		t.cfg.Changed()
	}
}

func (t *TV) onChannel(raw json.RawMessage) {
	var p struct {
		Number string `json:"channelNumber"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	n, ok := channelNumber(p.Number)
	t.update(func(v map[string]any) {
		delete(v, tv.Channel)
		if ok {
			v[tv.Channel] = float64(n)
		}
	})
}

// onChannels запоминает номера каналов: команда переключает канал по его channelId.
func (t *TV) onChannels(raw json.RawMessage) {
	var p struct {
		List []struct {
			ID     string `json:"channelId"`
			Number string `json:"channelNumber"`
		} `json:"channelList"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	numbers := map[int]string{}
	for _, c := range p.List {
		if n, ok := channelNumber(c.Number); ok && c.ID != "" {
			if _, dup := numbers[n]; !dup {
				numbers[n] = c.ID
			}
		}
	}
	t.mu.Lock()
	t.numbers = numbers
	t.mu.Unlock()
}

func (t *TV) onMedia(raw json.RawMessage) {
	var p struct {
		Apps []struct {
			PlayState string `json:"playState"`
		} `json:"foregroundAppInfo"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	t.update(func(v map[string]any) {
		delete(v, tv.Playback)
		for _, a := range p.Apps {
			switch a.PlayState {
			case "playing":
				v[tv.Playback] = tv.Playing
			case "paused":
				v[tv.Playback] = tv.Paused
			}
		}
	})
}

// onConnection запоминает MAC телевизора: им шлюз включает телевизор по сети.
func (t *TV) onConnection(raw json.RawMessage) {
	var p struct {
		Wired struct {
			MAC string `json:"macAddress"`
		} `json:"wiredInfo"`
		WiFi struct {
			MAC string `json:"macAddress"`
		} `json:"wifiInfo"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	mac := p.Wired.MAC
	if _, err := tv.ParseMAC(mac); err != nil {
		mac = p.WiFi.MAC
	}
	if _, err := tv.ParseMAC(mac); err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if strings.EqualFold(mac, t.pair.MAC) {
		return
	}
	t.pair.MAC = strings.ToUpper(mac)
	t.status.MAC = t.mac()
	if err := t.saveLocked(); err != nil {
		t.log.Warn("webOS: MAC телевизора не сохранён", "device", t.cfg.ID, "err", err)
	}
}

// channelNumber - номер канала из строки телевизора: «12» или «12-1» у цифрового канала.
func channelNumber(s string) (int, bool) {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "-")
	n, err := strconv.Atoi(head)
	return n, err == nil && n > 0 && n <= tv.MaxChannel
}

// active - соединение с включённым телевизором; nil - телевизор выключен.
func (t *TV) active() *session {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil || t.values[tv.Power] != tv.On {
		return nil
	}
	return t.conn
}

// wake включает телевизор пакетом Wake-on-LAN и минуту проверяет его чаще.
func (t *TV) wake() error {
	t.mu.Lock()
	mac := t.mac()
	t.mu.Unlock()
	if mac == "" {
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, tv.ErrNoMAC)
	}
	hw, err := tv.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
	}
	if err := tv.Wake(hw, t.cfg.Broadcast, t.cfg.Host); err != nil {
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
	}
	t.mu.Lock()
	t.wokeAt = time.Now()
	t.mu.Unlock()
	t.Retry()
	return nil
}

func (t *TV) Select(context.Context, string) error {
	return fmt.Errorf("%w: у телевизора нет программ", appliance.ErrNotSupported)
}

func (t *TV) Start(context.Context, string, map[string]any) error {
	return fmt.Errorf("%w: у телевизора нет программ", appliance.ErrNotSupported)
}

func (t *TV) Stop(context.Context) error {
	return fmt.Errorf("%w: у телевизора нет программ", appliance.ErrNotSupported)
}

func (t *TV) Command(context.Context, string) error {
	return fmt.Errorf("%w: команды телевизора - его функции TV.*", appliance.ErrNotSupported)
}

// Set выполняет команду телевизору. Кроме включения, команды доходят только до включённого
// телевизора.
func (t *TV) Set(ctx context.Context, key string, value any) error {
	if key == tv.Power {
		switch value {
		case tv.On:
			if t.active() != nil {
				return nil
			}
			return t.wake()
		case tv.Off:
			s := t.active()
			if s == nil {
				return nil
			}
			// Ответ на выключение телевизор присылает не всегда: шлюз его не ждёт.
			return mapErr(s.send(map[string]any{"type": "request", "id": s.nextID(), "uri": uriTurnOff}))
		}
		return fmt.Errorf("%w: питание %v", appliance.ErrInvalidValue, value)
	}
	s := t.active()
	if s == nil {
		return fmt.Errorf("%w: телевизор выключен", appliance.ErrWrongMode)
	}
	uri, payload, err := t.command(key, value)
	if err != nil {
		return err
	}
	_, err = s.call(ctx, "request", s.nextID(), uri, payload, requestTimeout)
	return mapErr(err)
}

// command - запрос телевизору для значения функции.
func (t *TV) command(key string, value any) (string, map[string]any, error) {
	switch key {
	case tv.Volume:
		f, ok := value.(float64)
		if !ok || math.IsNaN(f) || f < 0 || f > 100 {
			return "", nil, fmt.Errorf("%w: громкость %v", appliance.ErrInvalidValue, value)
		}
		return uriSetVolume, map[string]any{"volume": int(math.Round(f))}, nil
	case tv.Mute:
		b, ok := value.(bool)
		if !ok {
			return "", nil, fmt.Errorf("%w: звук %v", appliance.ErrInvalidValue, value)
		}
		return uriSetMute, map[string]any{"mute": b}, nil
	case tv.Channel:
		f, ok := value.(float64)
		if !ok || f != math.Trunc(f) || f < 1 || f > tv.MaxChannel {
			return "", nil, fmt.Errorf("%w: канала %v нет в списке телевизора", appliance.ErrInvalidValue, value)
		}
		t.mu.Lock()
		id := t.numbers[int(f)]
		t.mu.Unlock()
		if id == "" {
			return "", nil, fmt.Errorf("%w: канала %v нет в списке телевизора", appliance.ErrInvalidValue, value)
		}
		return uriOpenChannel, map[string]any{"channelId": id}, nil
	case tv.Input:
		v, _ := value.(string)
		if v == tv.LiveTV {
			return uriLaunch, map[string]any{"id": liveTVApp}, nil
		}
		t.mu.Lock()
		known := slices.ContainsFunc(t.inputs, func(in input) bool { return in.ID == v })
		t.mu.Unlock()
		if !known {
			return "", nil, fmt.Errorf("%w: входа %v нет у телевизора", appliance.ErrInvalidValue, value)
		}
		return uriSwitchInput, map[string]any{"inputId": v}, nil
	case tv.App:
		v, _ := value.(string)
		if !slices.Contains(t.cfg.Apps, v) {
			return "", nil, fmt.Errorf("%w: приложения %v нет в настройках", appliance.ErrInvalidValue, value)
		}
		return uriLaunch, map[string]any{"id": v}, nil
	case tv.Screen:
		switch value {
		case tv.On:
			return uriScreenOn, nil, nil
		case tv.Off:
			return uriScreenOff, nil, nil
		}
		return "", nil, fmt.Errorf("%w: экран %v", appliance.ErrInvalidValue, value)
	case tv.Playback:
		switch value {
		case tv.Playing:
			return uriPlay, nil, nil
		case tv.Paused:
			return uriPause, nil, nil
		}
		return "", nil, fmt.Errorf("%w: воспроизведение %v", appliance.ErrInvalidValue, value)
	}
	return "", nil, fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
}

// Step меняет громкость или канал на шаг кнопками телевизора: так «следующий канал»
// работает и тогда, когда номер канала телевизор не сообщает.
func (t *TV) Step(ctx context.Context, key string, delta float64) error {
	s := t.active()
	if s == nil {
		return fmt.Errorf("%w: телевизор выключен", appliance.ErrWrongMode)
	}
	var up, down string
	switch key {
	case tv.Volume:
		up, down = uriVolumeUp, uriVolumeDown
	case tv.Channel:
		up, down = uriChannelUp, uriChannelDown
	default:
		return fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
	}
	uri := up
	if delta < 0 {
		uri = down
	}
	n := min(int(math.Abs(math.Round(delta))), tv.MaxSteps)
	for range n {
		if _, err := s.call(ctx, "request", s.nextID(), uri, nil, requestTimeout); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// errTV - телевизор ответил на запрос ошибкой.
type errTV struct{ text string }

func (e *errTV) Error() string { return "телевизор отклонил запрос: " + e.text }

// mapErr переводит ошибку запроса в ошибку прибора: отказ телевизора - не то состояние,
// остальное - нет связи.
func mapErr(err error) error {
	var e *errTV
	switch {
	case err == nil:
		return nil
	case errors.As(err, &e):
		return fmt.Errorf("%w: %w", appliance.ErrWrongMode, err)
	case errors.Is(err, context.Canceled):
		return err
	}
	return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
}
