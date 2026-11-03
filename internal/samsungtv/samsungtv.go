// Package samsungtv - телевизоры Samsung на Tizen в домашней сети, без облака. Состояние
// питания шлюз берёт из REST телевизора, кнопки пульта отправляет по WebSocket с токеном,
// который телевизор выдаёт после «Разрешить» на экране, а громкость и звук меняет по UPnP,
// как интеграция Home Assistant. Телевизор для шлюза - прибор по профилю с функциями TV.*.
package samsungtv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
const SourceName = "samsung-tv"

// Type - тип прибора для профиля.
const Type = "SamsungTV"

const (
	// Порты телевизора: REST и WebSocket без шифрования, WebSocket с токеном и UPnP.
	restPort = "8001"
	wssPort  = "8002"
	upnpPort = "9197"
	// appName - имя шлюза в списке устройств телевизора.
	appName = "hcgateway"
	// requestTimeout - предел запроса к телевизору.
	requestTimeout = 5 * time.Second
	// pairTimeout - сколько шлюз ждёт «Разрешить» на экране.
	pairTimeout = time.Minute
	// poll - как часто шлюз опрашивает телевизор; после включения по сети - чаще, минуту.
	poll       = 10 * time.Second
	pollWake   = 2 * time.Second
	wakeWindow = time.Minute
	// retryPair - пауза после отказа на экране: телевизор не должен спрашивать каждые 10 с.
	retryPair = 10 * time.Minute
	// keyGap - пауза между кнопками: телевизор пропускает слишком частые нажатия.
	keyGap = 150 * time.Millisecond
	// maxBody - предел ответа REST и UPnP.
	maxBody = 1 << 20
	// maxPairingFile - предел файла телевизора.
	maxPairingFile = 64 << 10
	// renderingControl - служба UPnP громкости.
	renderingControl = "urn:schemas-upnp-org:service:RenderingControl:1"
)

// inputKeys - источники сигнала и кнопки пульта, которые их включают.
var inputKeys = map[string]string{"HDMI1": "KEY_HDMI1", "HDMI2": "KEY_HDMI2", "HDMI3": "KEY_HDMI3",
	"HDMI4": "KEY_HDMI4", tv.LiveTV: "KEY_TV"}

// Inputs - источники сигнала телевизора Samsung по порядку.
var Inputs = []string{tv.LiveTV, "HDMI1", "HDMI2", "HDMI3", "HDMI4"}

// Ошибки подключения: их видно на странице прибора.
var (
	errPairRefused = errors.New("телевизор отклонил подключение шлюза")
	errPairTimeout = errors.New("на телевизоре не нажали «Разрешить» за минуту")
)

// pairing - что шлюз помнит о телевизоре: токен, MAC и модель. Лежит в
// data_dir/samsung-tv/<id>.json.
type pairing struct {
	// Paired - телевизор разрешил подключение; у старых моделей токена нет.
	Paired bool   `json:"paired,omitempty"`
	Token  string `json:"token,omitempty"`
	MAC    string `json:"mac,omitempty"`
	Model  string `json:"model,omitempty"`
	// Cert - отпечаток сертификата, с которым шлюз отправляет токен.
	Cert string `json:"cert,omitempty"`
}

// Config - телевизор в настройках.
type Config struct {
	ID string
	// Host - IP телевизора.
	Host string
	// MAC - MAC для включения по сети; пусто - тот, что сообщил телевизор.
	MAC string
	// Broadcast - широковещательный адрес для Wake-on-LAN; пусто - 255.255.255.255.
	Broadcast string
	// Dir - каталог файлов телевизоров, data_dir/samsung-tv.
	Dir string
}

// Status - связь с телевизором для страницы прибора.
type Status struct {
	Paired, Prompt, Connected bool
	Model, MAC                string
	// UPnP - громкость и звук доступны по UPnP.
	UPnP bool
	Err  string
	// Cert - запомненный отпечаток сертификата; NewCert - телевизор предъявил другой и ждёт решения
	// владельца; Plain - соединение без шифрования: у модели нет wss.
	Cert, NewCert string
	Plain         bool
}

var _ appliance.Appliance = (*TV)(nil)
var _ appliance.Stepper = (*TV)(nil)

// TV - телевизор Samsung как прибор по профилю.
type TV struct {
	cfg  Config
	log  *slog.Logger
	http *http.Client
	// restURL, wsURLs и upnpURL - адреса телевизора; тесты подставляют свои.
	restURL string
	wsURLs  []string
	upnpURL string
	retry   chan struct{}

	mu       sync.Mutex
	pair     pairing
	status   Status
	values   map[string]any
	control  string
	ws       *websocket.Conn
	wsMu     sync.Mutex
	wokeAt   time.Time
	notify   appliance.Notify
	runs     int
	pairWait time.Time
}

// New открывает телевизор: токен и MAC - из data_dir/samsung-tv/<id>.json.
func New(cfg Config, log *slog.Logger) (*TV, error) {
	host := strings.Trim(cfg.Host, "[]")
	if h, _, err := net.SplitHostPort(cfg.Host); err == nil {
		host = h
	}
	t := &TV{cfg: cfg, log: log, http: &http.Client{Timeout: requestTimeout}, retry: make(chan struct{}, 1),
		values:  map[string]any{tv.Power: tv.Off},
		restURL: "http://" + net.JoinHostPort(host, restPort) + "/api/v2/",
		wsURLs: []string{"wss://" + net.JoinHostPort(host, wssPort) + "/api/v2/channels/samsung.remote.control",
			"ws://" + net.JoinHostPort(host, restPort) + "/api/v2/channels/samsung.remote.control"},
		upnpURL: "http://" + net.JoinHostPort(host, upnpPort) + "/dmr"}
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
	t.status = Status{Paired: t.pair.Paired, Model: t.pair.Model, MAC: t.mac(), Cert: t.pair.Cert}
	return t, nil
}

func (t *TV) path() string {
	return filepath.Join(t.cfg.Dir, t.cfg.ID+".json")
}

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

// Retry опрашивает телевизор заново сразу.
func (t *TV) Retry() {
	select {
	case t.retry <- struct{}{}:
	default:
	}
}

// Forget забывает токен: при следующем подключении телевизор снова спросит разрешение.
func (t *TV) Forget() error {
	t.mu.Lock()
	t.pair.Paired, t.pair.Token, t.pair.Cert = false, "", ""
	t.status.Paired, t.status.Cert, t.status.NewCert = false, "", ""
	t.pairWait = time.Time{}
	err := t.saveLocked()
	t.mu.Unlock()
	t.closeWS()
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
	return appliance.Description{Type: Type, Options: map[string]appliance.Option{
		tv.Power:    {Key: tv.Power, Values: []string{tv.On, tv.Off}, Setting: true},
		tv.Volume:   {Key: tv.Volume, Min: 0, Max: 100, Step: 1, Setting: true},
		tv.Mute:     {Key: tv.Mute, Setting: true},
		tv.Channel:  {Key: tv.Channel, Min: 1, Max: tv.MaxChannel, Step: 1, Setting: true},
		tv.Input:    {Key: tv.Input, Values: Inputs, Setting: true},
		tv.Playback: {Key: tv.Playback, Values: []string{tv.Playing, tv.Paused}, Setting: true},
	}, Reports: []string{tv.Power, tv.Volume, tv.Mute, tv.Channel, tv.Input, tv.Playback}}
}

// State - питание, громкость и звук. Источник и воспроизведение телевизор по домашней сети не
// сообщает, а номер канала шлюз не читает.
func (t *TV) State() appliance.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	values := map[string]any{}
	for k, v := range t.values {
		values[k] = v
	}
	return appliance.State{Online: t.status.Paired, Values: values}
}

// Run опрашивает телевизор, пока ctx не отменён: раз в 10 с, а минуту после включения по
// сети - раз в 2 с. Включённый телевизор без токена шлюз просит разрешить подключение.
func (t *TV) Run(ctx context.Context, notify appliance.Notify) {
	t.mu.Lock()
	t.runs++
	gen := t.runs
	t.notify = notify
	t.mu.Unlock()
	defer func() {
		t.closeWS()
		t.mu.Lock()
		if t.runs == gen {
			t.notify = nil
		}
		t.mu.Unlock()
	}()
	for {
		t.refresh(ctx)
		if ctx.Err() != nil {
			return
		}
		pause := poll
		t.mu.Lock()
		if time.Since(t.wokeAt) < wakeWindow {
			pause = pollWake
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.retry:
		case <-time.After(pause):
		}
	}
}

func (t *TV) changed() {
	t.mu.Lock()
	notify := t.notify
	t.mu.Unlock()
	if notify != nil {
		notify(nil)
	}
}

// info - сведения REST о телевизоре.
type info struct {
	Device struct {
		Name       string `json:"name"`
		PowerState string `json:"PowerState"`
		WifiMac    string `json:"wifiMac"`
		ModelName  string `json:"modelName"`
		TokenAuth  string `json:"TokenAuthSupport"`
	} `json:"device"`
}

// probeTimeout - предел проверки адреса при поиске в сети.
const probeTimeout = 2 * time.Second

// Info - что телевизор Samsung сообщает о себе по REST без разрешения: имя, модель и MAC.
type Info struct {
	Name, Model, MAC string
}

// Probe проверяет, что по адресу host - телевизор Samsung: он отвечает на GET /api/v2/ по
// порту 8001. Разрешения Probe не просит: на экране телевизора ничего не появляется.
func Probe(ctx context.Context, client *http.Client, host string) (Info, error) {
	return probe(ctx, client, "http://"+net.JoinHostPort(host, restPort)+"/api/v2/")
}

func probe(ctx context.Context, client *http.Client, restURL string) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	t := &TV{http: client, restURL: restURL}
	in, err := t.info(ctx)
	if err != nil {
		return Info{}, err
	}
	if in.Device.ModelName == "" && in.Device.WifiMac == "" {
		return Info{}, errors.New("не телевизор Samsung")
	}
	mac := ""
	if hw, err := tv.ParseMAC(in.Device.WifiMac); err == nil {
		mac = strings.ToUpper(hw.String())
	}
	return Info{Name: in.Device.Name, Model: in.Device.ModelName, MAC: mac}, nil
}

// refresh опрашивает телевизор: питание из REST, громкость и звук по UPnP. Телевизор без
// ответа REST выключен.
func (t *TV) refresh(ctx context.Context) {
	in, err := t.info(ctx)
	if err != nil {
		t.closeWS()
		t.set(func(v map[string]any, s *Status) {
			clear(v)
			v[tv.Power] = tv.Off
			s.Connected = false
		})
		return
	}
	on := in.Device.PowerState == "" || in.Device.PowerState == "on"
	t.mu.Lock()
	save := false
	if mac := in.Device.WifiMac; mac != "" && !strings.EqualFold(mac, t.pair.MAC) {
		if _, err := tv.ParseMAC(mac); err == nil {
			t.pair.MAC, save = strings.ToUpper(mac), true
		}
	}
	if m := in.Device.ModelName; m != "" && m != t.pair.Model {
		t.pair.Model, save = m, true
	}
	if save && t.pair.Paired {
		if err := t.saveLocked(); err != nil {
			t.log.Warn("Samsung: сведения о телевизоре не сохранены", "device", t.cfg.ID, "err", err)
		}
	}
	t.status.MAC, t.status.Model, t.status.Connected = t.mac(), t.pair.Model, true
	paired, waitUntil := t.pair.Paired, t.pairWait
	t.mu.Unlock()
	if !on {
		t.closeWS()
		t.set(func(v map[string]any, _ *Status) { clear(v); v[tv.Power] = tv.Off })
		return
	}
	if !paired && time.Now().After(waitUntil) {
		// Первое подключение: телевизор спрашивает разрешение на экране.
		if _, err := t.conn(ctx); err != nil {
			t.log.Warn("Samsung: телевизор не разрешил подключение", "device", t.cfg.ID, "err", err)
		}
	}
	values := map[string]any{tv.Power: tv.On}
	upnp := false
	if ctl, err := t.renderingControl(ctx); err == nil {
		if vol, err := t.soap(ctx, ctl, "GetVolume", "CurrentVolume", nil); err == nil {
			if f, err := strconv.ParseFloat(vol, 64); err == nil {
				values[tv.Volume], upnp = f, true
			}
		}
		if mute, err := t.soap(ctx, ctl, "GetMute", "CurrentMute", nil); err == nil {
			values[tv.Mute] = mute == "1" || mute == "true"
		}
	}
	t.set(func(v map[string]any, s *Status) {
		clear(v)
		for k, x := range values {
			v[k] = x
		}
		s.UPnP = upnp
	})
}

// set меняет значения и связь и сообщает об изменении.
func (t *TV) set(f func(v map[string]any, s *Status)) {
	t.mu.Lock()
	f(t.values, &t.status)
	t.mu.Unlock()
	t.changed()
}

func (t *TV) info(ctx context.Context) (*info, error) {
	body, err := t.get(ctx, t.restURL)
	if err != nil {
		return nil, err
	}
	var in info
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("ответ REST телевизора не разобран: %w", err)
	}
	return &in, nil
}

func (t *TV) get(ctx context.Context, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("телевизор ответил HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// conn - соединение для кнопок пульта: открытое или новое. Новое соединение без токена
// телевизор показывает на экране и ждёт «Разрешить» до минуты; токен шлюз сохраняет.
func (t *TV) conn(ctx context.Context) (*websocket.Conn, error) {
	t.wsMu.Lock()
	defer t.wsMu.Unlock()
	if t.ws != nil {
		return t.ws, nil
	}
	t.mu.Lock()
	token, paired, pin := t.pair.Token, t.pair.Paired, t.pair.Cert
	t.mu.Unlock()
	name := base64.StdEncoding.EncodeToString([]byte(appName))
	check := tv.NewCertCheck(pin)
	d := websocket.Dialer{HandshakeTimeout: requestTimeout, TLSClientConfig: check.TLSConfig()}
	last := errors.New("у телевизора нет адреса wss")
	for _, base := range t.wsURLs {
		// С запомненным отпечатком - только wss: иначе токен ушёл бы открытым текстом.
		if pin != "" && !tv.Encrypted(base) {
			continue
		}
		q := url.Values{"name": {name}}
		if token != "" {
			q.Set("token", token)
		}
		ws, resp, err := d.DialContext(ctx, base+"?"+q.Encode(), nil)
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if errors.Is(err, tv.ErrCertChanged) {
			t.certChanged(check.Seen())
			return nil, err
		}
		if err != nil {
			last = err
			continue
		}
		ws.SetReadLimit(maxBody)
		t.certSeen(base, check)
		if !paired {
			t.mu.Lock()
			t.status.Prompt = true
			t.mu.Unlock()
			t.changed()
		}
		newToken, err := t.handshake(ws, paired)
		t.mu.Lock()
		t.status.Prompt = false
		t.mu.Unlock()
		if err != nil {
			ws.Close()
			t.mu.Lock()
			t.status.Err = err.Error()
			t.pairWait = time.Now().Add(retryPair)
			t.mu.Unlock()
			t.changed()
			return nil, err
		}
		t.mu.Lock()
		if !t.pair.Paired || (newToken != "" && newToken != t.pair.Token) {
			t.pair.Paired = true
			if newToken != "" {
				t.pair.Token = newToken
			}
			if err := t.saveLocked(); err != nil {
				t.log.Warn("Samsung: токен телевизора не сохранён", "device", t.cfg.ID, "err", err)
			}
		}
		t.status.Paired, t.status.Err = true, ""
		t.mu.Unlock()
		metrics.SourceConnected(SourceName, t.cfg.ID, true)
		t.ws = ws
		go t.drain(ws)
		t.changed()
		return ws, nil
	}
	return nil, last
}

// handshake ждёт ms.channel.connect: с новым токеном, если телевизор его выдал.
func (t *TV) handshake(ws *websocket.Conn, paired bool) (string, error) {
	wait := requestTimeout
	if !paired {
		wait = pairTimeout
	}
	_ = ws.SetReadDeadline(time.Now().Add(wait))
	defer ws.SetReadDeadline(time.Time{})
	for {
		var m struct {
			Event string `json:"event"`
			Data  struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		if err := ws.ReadJSON(&m); err != nil {
			if !paired {
				return "", errPairTimeout
			}
			return "", err
		}
		switch m.Event {
		case "ms.channel.connect":
			return m.Data.Token, nil
		case "ms.channel.unauthorized", "ms.channel.timeOut":
			return "", errPairRefused
		}
	}
}

// drain читает сообщения телевизора, пока соединение открыто: без чтения не дойдут ответы
// на ping.
func (t *TV) drain(ws *websocket.Conn) {
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			t.wsMu.Lock()
			if t.ws == ws {
				t.ws = nil
				metrics.SourceConnected(SourceName, t.cfg.ID, false)
			}
			t.wsMu.Unlock()
			ws.Close()
			return
		}
	}
}

func (t *TV) closeWS() {
	t.wsMu.Lock()
	ws := t.ws
	t.ws = nil
	t.wsMu.Unlock()
	if ws != nil {
		ws.Close()
	}
}

// keys нажимает кнопки пульта по одной.
func (t *TV) keys(ctx context.Context, keys ...string) error {
	ws, err := t.conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
	}
	for i, k := range keys {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(keyGap):
			}
		}
		msg := map[string]any{"method": "ms.remote.control", "params": map[string]any{"Cmd": "Click", "DataOfCmd": k,
			"Option": "false", "TypeOfRemote": "SendRemoteKey"}}
		t.wsMu.Lock()
		_ = ws.SetWriteDeadline(time.Now().Add(requestTimeout))
		err := ws.WriteJSON(msg)
		t.wsMu.Unlock()
		if err != nil {
			t.closeWS()
			return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
		}
	}
	return nil
}

func (t *TV) on() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.values[tv.Power] == tv.On
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
			if t.on() {
				return nil
			}
			return t.wake()
		case tv.Off:
			if !t.on() {
				return nil
			}
			// KEY_POWER переключает питание: шлюз нажимает её только у включённого телевизора.
			if err := t.keys(ctx, "KEY_POWER"); err != nil {
				return err
			}
			t.set(func(v map[string]any, _ *Status) { clear(v); v[tv.Power] = tv.Off })
			return nil
		}
		return fmt.Errorf("%w: питание %v", appliance.ErrInvalidValue, value)
	}
	if !t.on() {
		return fmt.Errorf("%w: телевизор выключен", appliance.ErrWrongMode)
	}
	switch key {
	case tv.Volume:
		f, ok := value.(float64)
		if !ok || math.IsNaN(f) || f < 0 || f > 100 {
			return fmt.Errorf("%w: громкость %v", appliance.ErrInvalidValue, value)
		}
		n := strconv.Itoa(int(math.Round(f)))
		return t.setUPnP(ctx, "SetVolume", map[string]string{"DesiredVolume": n}, tv.Volume, math.Round(f))
	case tv.Mute:
		b, ok := value.(bool)
		if !ok {
			return fmt.Errorf("%w: звук %v", appliance.ErrInvalidValue, value)
		}
		return t.setUPnP(ctx, "SetMute", map[string]string{"DesiredMute": map[bool]string{true: "1", false: "0"}[b]}, tv.Mute, b)
	case tv.Channel:
		f, ok := value.(float64)
		if !ok || f != math.Trunc(f) || f < 1 || f > tv.MaxChannel {
			return fmt.Errorf("%w: канал %v", appliance.ErrInvalidValue, value)
		}
		var keys []string
		for _, d := range strconv.Itoa(int(f)) {
			keys = append(keys, "KEY_"+string(d))
		}
		return t.keys(ctx, append(keys, "KEY_ENTER")...)
	case tv.Input:
		k, ok := inputKeys[fmt.Sprint(value)]
		if !ok {
			return fmt.Errorf("%w: источник %v", appliance.ErrInvalidValue, value)
		}
		return t.keys(ctx, k)
	case tv.Playback:
		switch value {
		case tv.Playing:
			return t.keys(ctx, "KEY_PLAY")
		case tv.Paused:
			return t.keys(ctx, "KEY_PAUSE")
		}
		return fmt.Errorf("%w: воспроизведение %v", appliance.ErrInvalidValue, value)
	}
	return fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
}

// setUPnP меняет громкость или звук по UPnP и сразу меняет известное значение.
func (t *TV) setUPnP(ctx context.Context, action string, args map[string]string, key string, v any) error {
	ctl, err := t.renderingControl(ctx)
	if err != nil {
		return fmt.Errorf("%w: громкость по UPnP недоступна: %w", appliance.ErrNotSupported, err)
	}
	if _, err := t.soap(ctx, ctl, action, "", args); err != nil {
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
	}
	t.set(func(values map[string]any, _ *Status) { values[key] = v })
	return nil
}

// Step меняет громкость или канал кнопками пульта: так «громче» и «следующий канал»
// работают без текущего значения.
func (t *TV) Step(ctx context.Context, key string, delta float64) error {
	if !t.on() {
		return fmt.Errorf("%w: телевизор выключен", appliance.ErrWrongMode)
	}
	var up, down string
	switch key {
	case tv.Volume:
		up, down = "KEY_VOLUP", "KEY_VOLDOWN"
	case tv.Channel:
		up, down = "KEY_CHUP", "KEY_CHDOWN"
	default:
		return fmt.Errorf("%w: %s", appliance.ErrNotSupported, key)
	}
	k := up
	if delta < 0 {
		k = down
	}
	n := min(int(math.Abs(math.Round(delta))), tv.MaxSteps)
	keys := make([]string, n)
	for i := range keys {
		keys[i] = k
	}
	return t.keys(ctx, keys...)
}

// wake включает телевизор пакетом Wake-on-LAN и минуту опрашивает его чаще.
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
