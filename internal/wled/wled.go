// Package wled - лента WLED в Алисе. Шлюз держит одно соединение WebSocket на ленту:
// лента сама присылает состояние после каждого изменения, а команды уходят по тому же
// соединению в формате JSON API WLED.
package wled

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/alice"
	"hcgateway/internal/metrics"
)

// SourceName - источник в метриках.
const SourceName = "wled"

const (
	// Пауза перед переподключением: от 1 до 60 с, удваивается после каждой неудачи.
	reconnectMin = time.Second
	reconnectMax = time.Minute
	// stable - соединение, которое проработало столько, сбрасывает паузу.
	stable      = time.Minute
	dialTimeout = 5 * time.Second
	// writeTimeout - предел отправки одного сообщения.
	writeTimeout = 3 * time.Second
	// maxMessage - предел сообщения ленты.
	maxMessage = 64 << 10
	// Команда должна уместиться в один кадр: лента не собирает сообщение из кадров.
	frameESP32   = 1428
	frameESP8266 = 528
	// presetsTimeout и maxPresetsFile - пределы загрузки пресетов.
	presetsTimeout = 5 * time.Second
	maxPresetsFile = 256 << 10
	// MaxPreset - самый большой номер пресета WLED.
	MaxPreset = 250
)

// pingEvery - как часто шлюз проверяет соединение сообщением p: лента отвечает pong.
// Соединение, из которого за readTimeout ничего не пришло, считается оборванным. Тесты их сокращают.
var (
	pingEvery   = 30 * time.Second
	readTimeout = 2*pingEvery + 10*time.Second
)

var _ alice.Device = (*Device)(nil)

// errOffline - соединения с лентой нет.
var errOffline = errors.New("нет соединения с лентой WLED")

// Device - лента WLED как устройство Алисы.
type Device struct {
	ID, Name, Room string
	// Host - адрес ленты в домашней сети без схемы: 192.168.1.50 или wled.local:80.
	Host string
	// Presets - номер пресета WLED -> сцена Алисы.
	Presets map[int]string
	Log     *slog.Logger

	// writeMu - сообщения ленте по одному: у соединения один писатель.
	writeMu sync.Mutex

	mu   sync.Mutex
	conn *websocket.Conn
	st   status
	// have - состояние получено после подключения.
	have bool
}

// status - что лента сообщила о себе.
type status struct {
	On  bool
	Bri int
	// PS - текущий пресет, -1 - нет.
	PS int
	// Color - основной цвет главного сегмента, 0xRRGGBB.
	Color      int
	Name, Arch string
	// MAC - MAC ленты вида AA:BB:CC:DD:EE:FF: по нему поиск в сети узнаёт ленту по новому
	// адресу. Остаётся и после потери связи.
	MAC string
}

// Strip - связь с лентой и что она сообщила о себе: имя и платформу.
func (d *Device) Strip() (online bool, name, arch string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.have, d.st.Name, d.st.Arch
}

// Values - что лента сообщила о своём состоянии: те же instance, что у Алисы. ok - лента на связи.
func (d *Device) Values() (values map[string]any, ok bool) {
	d.mu.Lock()
	st, have := d.st, d.have
	d.mu.Unlock()
	if !have {
		return nil, false
	}
	values = map[string]any{"on": st.On, "brightness": float64(percent(st.Bri)), "rgb": fmt.Sprintf("#%06X", st.Color)}
	if scene, ok := d.Presets[st.PS]; ok {
		values["scene"] = scene
	}
	return values, true
}

// MAC - MAC ленты из её последнего сообщения о себе; пусто - лента его ещё не сообщала.
func (d *Device) MAC() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st.MAC
}

// probeTimeout и maxInfo - пределы проверки адреса при поиске в сети.
const (
	probeTimeout = 2 * time.Second
	maxInfo      = 64 << 10
)

// Info - что лента сообщает о себе по HTTP без входа.
type Info struct {
	Name, Arch, MAC string
}

// Probe проверяет, что по адресу host - лента WLED: она отвечает на GET /json/info.
func Probe(ctx context.Context, client *http.Client, host string) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/json/info", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var in struct {
		Brand, Name, Arch, MAC string
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxInfo)).Decode(&in); err != nil {
		return Info{}, err
	}
	if in.Brand != "WLED" {
		return Info{}, errors.New("не лента WLED")
	}
	return Info{Name: in.Name, Arch: in.Arch, MAC: colonMAC(in.MAC)}, nil
}

// colonMAC - MAC ленты aabbccddeeff в виде AA:BB:CC:DD:EE:FF; другой вид - пусто.
func colonMAC(s string) string {
	if len(s) != 12 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < 12; i += 2 {
		if _, err := strconv.ParseUint(s[i:i+2], 16, 8); err != nil {
			return ""
		}
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(strings.ToUpper(s[i : i+2]))
	}
	return b.String()
}

func (d *Device) IDs() []string {
	return []string{d.ID}
}

// scenes - сцены Алисы, которым сопоставлены пресеты, в порядке списка Яндекса.
func (d *Device) scenes() []alice.SceneItem {
	var out []alice.SceneItem
	for _, s := range alice.ColorScenes {
		for _, v := range d.Presets {
			if v == s {
				out = append(out, alice.SceneItem{ID: s})
				break
			}
		}
	}
	return out
}

// presetFor - пресет сцены или 0, если сцене пресет не сопоставлен.
func (d *Device) presetFor(scene string) int {
	for p, s := range d.Presets {
		if s == scene {
			return p
		}
	}
	return 0
}

func (d *Device) Info() []alice.DeviceInfo {
	color := alice.ColorParams{ColorModel: "rgb"}
	if scenes := d.scenes(); len(scenes) > 0 {
		color.ColorScene = &alice.SceneParams{Scenes: scenes}
	}
	return []alice.DeviceInfo{{
		ID: d.ID, Name: d.Name, Room: d.Room, Type: alice.TypeLight,
		DeviceInfo: alice.Manufacturer{Manufacturer: "WLED"},
		Capabilities: []alice.Capability{
			{Type: alice.CapOnOff, Retrievable: true, Reportable: true},
			{Type: alice.CapRange, Retrievable: true, Reportable: true, Parameters: alice.RangeParams{Instance: "brightness",
				Unit: "unit.percent", RandomAccess: true, Range: alice.RangeLimit{Min: 1, Max: 100, Precision: 1}}},
			{Type: alice.CapColor, Retrievable: true, Reportable: true, Parameters: color},
		},
	}}
}

func (d *Device) States() []alice.DeviceState {
	d.mu.Lock()
	st, have := d.st, d.have
	d.mu.Unlock()
	if !have {
		return []alice.DeviceState{{ID: d.ID, ErrorCode: alice.CodeUnreachable}}
	}
	color := alice.StateValue{Instance: "rgb", Value: st.Color}
	if scene, ok := d.Presets[st.PS]; ok {
		color = alice.StateValue{Instance: "scene", Value: scene}
	}
	return []alice.DeviceState{{ID: d.ID, Capabilities: []alice.CapState{
		{Type: alice.CapOnOff, State: alice.StateValue{Instance: "on", Value: st.On}},
		{Type: alice.CapRange, State: alice.StateValue{Instance: "brightness", Value: percent(st.Bri)}},
		{Type: alice.CapColor, State: color},
	}}}
}

// percent - яркость WLED 1-255 в процентах Алисы 1-100.
func percent(bri int) int {
	return max(1, int(math.Round(float64(bri)*100/255)))
}

// level - проценты Алисы 1-100 в яркость WLED 1-255.
func level(pct float64) int {
	return max(1, min(255, int(math.Round(pct*255/100))))
}

// Act выполняет команду Алисы: отправляет ленте команду JSON API и сразу меняет известное
// состояние. Лента подтвердит его своим состоянием не позже чем через секунду.
func (d *Device) Act(ctx context.Context, id string, a *alice.Action) string {
	if id != d.ID {
		return alice.CodeInvalidAction
	}
	cmd, update, code := d.command(a)
	if code != "" {
		return code
	}
	if err := d.send(ctx, cmd); err != nil {
		d.Log.Info("лента WLED: команда не отправлена", "device", d.ID, "err", err)
		return alice.CodeUnreachable
	}
	d.mu.Lock()
	update(&d.st)
	d.mu.Unlock()
	return ""
}

// command - команда ленте по команде Алисы и то, как она меняет состояние.
func (d *Device) command(a *alice.Action) (map[string]any, func(*status), string) {
	switch a.Type {
	case alice.CapOnOff:
		on, ok := a.State.Value.(bool)
		if !ok || a.State.Instance != "on" {
			return nil, nil, alice.CodeInvalidValue
		}
		return map[string]any{"on": on}, func(s *status) { s.On = on }, ""
	case alice.CapRange:
		if a.State.Instance != "brightness" {
			return nil, nil, alice.CodeInvalidAction
		}
		v, ok := a.State.Value.(float64)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, nil, alice.CodeInvalidValue
		}
		if a.State.Relative {
			// «Сделай ярче» не выходит за пределы, а не получает ошибку.
			d.mu.Lock()
			v = min(100, max(1, v+float64(percent(d.st.Bri))))
			d.mu.Unlock()
		}
		if v < 1 || v > 100 {
			return nil, nil, alice.CodeInvalidValue
		}
		bri := level(v)
		return map[string]any{"on": true, "bri": bri}, func(s *status) { s.On, s.Bri = true, bri }, ""
	case alice.CapColor:
		switch a.State.Instance {
		case "rgb":
			v, ok := a.State.Value.(float64)
			if !ok || v < 0 || v > 0xFFFFFF || v != math.Trunc(v) {
				return nil, nil, alice.CodeInvalidValue
			}
			c := int(v)
			// fx 0 - эффект «Solid»: иначе цвет менялся бы внутри эффекта пресета.
			seg := map[string]any{"col": [][]int{{c >> 16 & 0xFF, c >> 8 & 0xFF, c & 0xFF}}, "fx": 0}
			return map[string]any{"on": true, "seg": []any{seg}}, func(s *status) { s.On, s.Color, s.PS = true, c, -1 }, ""
		case "scene":
			scene, _ := a.State.Value.(string)
			p := d.presetFor(scene)
			if p == 0 {
				return nil, nil, alice.CodeInvalidValue
			}
			return map[string]any{"ps": p}, func(s *status) { s.On, s.PS = true, p }, ""
		}
	}
	return nil, nil, alice.CodeInvalidAction
}

// send отправляет команду по текущему соединению. Команда - один кадр: 528 байт на ESP8266, 1428 на ESP32.
func (d *Device) send(ctx context.Context, cmd map[string]any) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	d.mu.Lock()
	conn, arch := d.conn, d.st.Arch
	d.mu.Unlock()
	if conn == nil {
		return errOffline
	}
	limit := frameESP32
	if arch == "esp8266" {
		limit = frameESP8266
	}
	if len(data) > limit {
		return fmt.Errorf("команда %d байт не помещается в кадр %d байт", len(data), limit)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.write(conn, data)
}

func (d *Device) write(conn *websocket.Conn, data []byte) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// Run держит соединение с лентой и переподключается после обрыва, пока ctx не отменён.
func (d *Device) Run(ctx context.Context, changed alice.Changed) {
	pause := reconnectMin
	for {
		started := time.Now()
		err := d.session(ctx, changed)
		d.mu.Lock()
		had := d.have
		d.conn, d.have = nil, false
		d.mu.Unlock()
		metrics.SourceConnected(SourceName, d.ID, false)
		if had {
			changed(nil)
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= stable {
			pause = reconnectMin
		}
		d.Log.Warn("лента WLED: нет соединения, переподключение", "device", d.ID, "err", err, "after", pause)
		metrics.SourceReconnect(SourceName, d.ID)
		if sleep(ctx, pause) != nil {
			return
		}
		pause = min(pause*2, reconnectMax)
	}
}

// session - одно соединение: подключиться и читать состояние, пока соединение живо.
// При подключении лента сама присылает состояние и сведения о себе.
func (d *Device) session(ctx context.Context, changed alice.Changed) error {
	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, resp, err := dialer.DialContext(dctx, "ws://"+d.Host+"/ws", nil)
	cancel()
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessage)
	d.mu.Lock()
	d.conn = conn
	d.mu.Unlock()
	metrics.SourceConnected(SourceName, d.ID, true)
	every, idle := pingEvery, readTimeout
	// Сессия заканчивается, когда закончились и её горутины.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	defer func() {
		close(stop)
		wg.Wait()
	}()
	wg.Add(2)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()
	go func() {
		defer wg.Done()
		d.heartbeat(conn, stop, every)
	}()
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if d.apply(data) {
			changed(nil)
		}
	}
}

// heartbeat шлёт ленте p: ответ pong продлевает срок чтения, а молчащее соединение рвётся.
func (d *Device) heartbeat(conn *websocket.Conn, stop chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if d.write(conn, []byte("p")) != nil {
				conn.Close()
				return
			}
		}
	}
}

// message - сообщение ленты: состояние и сведения о ней или ошибка.
type message struct {
	State *struct {
		On      *bool     `json:"on"`
		Bri     *int      `json:"bri"`
		PS      *int      `json:"ps"`
		MainSeg int       `json:"mainseg"`
		Seg     []segment `json:"seg"`
	} `json:"state"`
	Info *struct {
		Name string `json:"name"`
		Arch string `json:"arch"`
		MAC  string `json:"mac"`
	} `json:"info"`
	Error *int `json:"error"`
}

type segment struct {
	ID  int               `json:"id"`
	Col []json.RawMessage `json:"col"`
}

// apply применяет сообщение ленты и сообщает, изменилось ли то, что видит Алиса.
func (d *Device) apply(data []byte) bool {
	if string(data) == "pong" {
		return false
	}
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		d.Log.Debug("лента WLED: сообщение не разобрано", "device", d.ID, "err", err)
		return false
	}
	if m.Error != nil {
		// 3 - у ленты кончились буферы JSON, 9 - сообщение пришло несколькими кадрами.
		d.Log.Warn("лента WLED ответила ошибкой", "device", d.ID, "error", *m.Error)
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	before, had := d.st, d.have
	if m.Info != nil {
		d.st.Name, d.st.Arch = m.Info.Name, m.Info.Arch
		if mac := colonMAC(m.Info.MAC); mac != "" {
			d.st.MAC = mac
		}
	}
	if s := m.State; s != nil {
		if s.On != nil {
			d.st.On = *s.On
		}
		if s.Bri != nil {
			d.st.Bri = *s.Bri
		}
		if s.PS != nil {
			d.st.PS = *s.PS
		}
		// Цвет - основной цвет главного сегмента, а если его нет в списке - первого.
		i := slices.IndexFunc(s.Seg, func(sg segment) bool { return sg.ID == s.MainSeg })
		if i < 0 && len(s.Seg) > 0 {
			i = 0
		}
		if i >= 0 && len(s.Seg[i].Col) > 0 {
			if c, ok := parseColor(s.Seg[i].Col[0]); ok {
				d.st.Color = c
			}
		}
		d.have = true
	}
	return d.st != before || d.have != had
}

// parseColor разбирает цвет WLED: массив [r,g,b] или [r,g,b,w] или строку "RRGGBB".
func parseColor(raw json.RawMessage) (int, bool) {
	var arr []int
	if json.Unmarshal(raw, &arr) == nil && len(arr) >= 3 {
		c := func(v int) int { return min(255, max(0, v)) }
		return c(arr[0])<<16 | c(arr[1])<<8 | c(arr[2]), true
	}
	var hex string
	if json.Unmarshal(raw, &hex) == nil && len(hex) >= 6 {
		if v, err := strconv.ParseUint(hex[:6], 16, 32); err == nil {
			return int(v), true
		}
	}
	return 0, false
}

// LoadPresets загружает номера и имена пресетов ленты из её файла presets.json.
func LoadPresets(ctx context.Context, host string) (map[int]string, error) {
	ctx, cancel := context.WithTimeout(ctx, presetsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/presets.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("лента WLED ответила %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPresetsFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPresetsFile {
		return nil, fmt.Errorf("presets.json больше %d байт", maxPresetsFile)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("presets.json: %w", err)
	}
	out := map[int]string{}
	for k, v := range raw {
		id, err := strconv.Atoi(k)
		if err != nil || id < 1 || id > MaxPreset {
			continue
		}
		var p struct {
			N string `json:"n"`
		}
		json.Unmarshal(v, &p)
		out[id] = p.N
	}
	return out, nil
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
