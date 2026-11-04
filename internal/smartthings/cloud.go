package smartthings

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/atomicfile"
	"hcgateway/internal/metrics"
)

const (
	// Пауза перед новым потоком событий: от 10 с до 10 минут, удваивается после каждой неудачи.
	reconnectMin = 10 * time.Second
	reconnectMax = 10 * time.Minute
	// stableStream - поток, который проработал столько, сбрасывает паузу.
	stableStream = time.Minute
	// streamIdle - поток, из которого столько ничего не пришло, даже welcome, оборван.
	// Как часто SmartThings повторяет welcome, документация не называет.
	streamIdle = 5 * time.Minute
	// maxLine - предел строки потока событий.
	maxLine = 1 << 20
	// loginTTL и maxLogins - сколько действует state входа и сколько входов ждут сразу.
	loginTTL  = 10 * time.Minute
	maxLogins = 5
	// maxDevices - сколько устройств аккаунта шлюз берёт и сколько страниц списка читает.
	maxDevices = 200
	maxPages   = 10
	// maxListFile и maxCapFile - пределы кэша списка устройств и описания capability.
	maxListFile = 4 << 20
	maxCapFile  = 256 << 10
	// listRetry - через сколько снова загружать список, если устройства из настроек в нём нет.
	listRetry = 5 * time.Minute
)

// errGoodbye - SmartThings закрывает поток: нужна новая подписка.
var errGoodbye = errors.New("SmartThings закрыл поток событий")

// login - вход, начатый владельцем: адрес возврата и срок state.
type login struct {
	state, redirect string
	expires         time.Time
}

// Cloud - аккаунт SmartThings: вход, список устройств, описания capability, один поток
// событий и приборы.
type Cloud struct {
	API *Client
	// dir - каталог data_dir/smartthings: кэш списка, описания capability и расход запросов.
	dir string
	log *slog.Logger
	// wake - вход или настройки изменились; changes - узнали тип или capability приборов;
	// fetch - нужен список устройств.
	wake, changes, fetch chan struct{}

	mu         sync.Mutex
	devices    []Device
	defs       map[string]*CapDef
	appliances map[string]*Appliance
	attached   map[*Appliance]bool
	health     map[string]bool
	connected  bool
	logins     []login
	// lastErr - почему нет потока событий, для страницы «Подключения».
	lastErr string
}

// New открывает аккаунт: токены - в tokensPath, остальное - в dir.
func New(tokensPath, dir string, log *slog.Logger) (*Cloud, error) {
	api, err := NewClient(tokensPath, filepath.Join(dir, "usage.json"), log)
	if err != nil {
		return nil, err
	}
	c := &Cloud{API: api, dir: dir, log: log, wake: make(chan struct{}, 1), changes: make(chan struct{}, 1),
		fetch: make(chan struct{}, 1), defs: map[string]*CapDef{}, appliances: map[string]*Appliance{},
		attached: map[*Appliance]bool{}, health: map[string]bool{}}
	if err := os.MkdirAll(filepath.Join(dir, "capabilities"), 0o700); err != nil {
		return nil, err
	}
	if err := readJSON(c.listPath(), maxListFile, &c.devices); err != nil {
		log.Warn("SmartThings: кэш списка устройств не прочитан, загружу заново", "err", err)
		c.devices = nil
	}
	return c, nil
}

func (c *Cloud) listPath() string {
	return filepath.Join(c.dir, "devices.json")
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func drain(ch chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

// Configure задаёт приложение и адрес облака. Новые значения подключают поток заново.
func (c *Cloud) Configure(clientID, clientSecret, base string) {
	c.API.mu.Lock()
	same := c.API.clientID == clientID && c.API.clientSecret == clientSecret && c.API.base == strings.TrimSuffix(base, "/")
	c.API.mu.Unlock()
	if same {
		return
	}
	c.API.Configure(clientID, clientSecret, base)
	signal(c.wake)
}

// Changes сообщает, что узнали тип или capability приборов: их можно собрать заново.
func (c *Cloud) Changes() <-chan struct{} {
	return c.changes
}

// Connected сообщает, открыт ли поток событий.
func (c *Cloud) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// LastError - почему нет потока событий; пусто - ошибки нет.
func (c *Cloud) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// BeginLogin начинает вход: адрес страницы входа SmartThings со state. redirect - адрес
// возврата на шлюз, тот же, что в приложении API Access.
func (c *Cloud) BeginLogin(redirect string) (string, error) {
	state, err := NewState()
	if err != nil {
		return "", err
	}
	u, err := c.API.AuthorizeURL(redirect, state)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.logins = slices.DeleteFunc(c.logins, func(l login) bool { return now.After(l.expires) })
	if len(c.logins) >= maxLogins {
		c.logins = c.logins[1:]
	}
	c.logins = append(c.logins, login{state: state, redirect: redirect, expires: now.Add(loginTTL)})
	return u, nil
}

// ErrUnknownState - state входа неизвестен, использован или истёк.
var ErrUnknownState = errors.New("вход SmartThings не начат или истёк")

// FinishLogin завершает вход: state сверяется за постоянное время и действует один раз,
// код меняется на токены.
func (c *Cloud) FinishLogin(ctx context.Context, state, code string) error {
	c.mu.Lock()
	var found *login
	now := time.Now()
	for i := range c.logins {
		l := c.logins[i]
		if subtle.ConstantTimeCompare([]byte(l.state), []byte(state)) == 1 && now.Before(l.expires) {
			found = &l
			c.logins = slices.Delete(c.logins, i, i+1)
			break
		}
	}
	c.mu.Unlock()
	if found == nil || code == "" {
		return ErrUnknownState
	}
	if err := c.API.Exchange(ctx, code, found.redirect); err != nil {
		return err
	}
	c.log.Info("SmartThings: вход выполнен")
	signal(c.wake)
	signal(c.fetch)
	return nil
}

// Logout удаляет токены: приборы SmartThings теряют связь до нового входа.
func (c *Cloud) Logout() error {
	err := c.API.Logout()
	signal(c.wake)
	return err
}

// Account - устройства аккаунта, как их последний раз сообщило облако.
func (c *Cloud) Account() []Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.devices)
}

// device - устройство из списка аккаунта или nil.
func (c *Cloud) device(id string) *Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.devices {
		if c.devices[i].ID == id {
			d := c.devices[i]
			return &d
		}
	}
	return nil
}

// RefreshAccount загружает список устройств аккаунта и описания их capability.
func (c *Cloud) RefreshAccount(ctx context.Context) error {
	var list []Device
	next := "/v1/devices"
	for page := 0; next != "" && page < maxPages && len(list) < maxDevices; page++ {
		var resp struct {
			Items []Device `json:"items"`
			Links struct {
				Next *struct {
					Href string `json:"href"`
				} `json:"next"`
			} `json:"_links"`
		}
		if err := c.API.do(ctx, http.MethodGet, next, nil, &resp); err != nil {
			return err
		}
		list = append(list, resp.Items...)
		next = ""
		if resp.Links.Next != nil {
			// Следующая страница - только с того же облака: иначе токен ушёл бы чужому сайту.
			if c.sameHost(resp.Links.Next.Href) {
				next = resp.Links.Next.Href
			}
		}
	}
	if len(list) > maxDevices {
		list = list[:maxDevices]
	}
	c.loadDisabled(ctx, list)
	data, err := json.Marshal(list)
	if err == nil {
		err = atomicfile.Write(c.listPath(), data)
	}
	if err != nil {
		c.log.Warn("SmartThings: список устройств не сохранён", "err", err)
	}
	c.mu.Lock()
	c.devices = list
	c.mu.Unlock()
	var refs []CapRef
	for _, d := range list {
		for _, comp := range d.Components {
			refs = append(refs, comp.Capabilities...)
		}
	}
	c.loadDefs(ctx, refs)
	signal(c.changes)
	return nil
}

// loadDisabled заполняет отключённые компоненты в новом списке: страница «Подключения» не должна
// предлагать камеру, которой у устройства нет. Запрос - только к устройствам с этим списком, а при
// сбое остаётся прежнее значение.
func (c *Cloud) loadDisabled(ctx context.Context, list []Device) {
	for i := range list {
		d := &list[i]
		if prev := c.device(d.ID); prev != nil {
			d.DisabledComponents = prev.DisabledComponents
		}
		if !d.hasDisabledList() {
			continue
		}
		st, err := c.status(ctx, d.ID)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("SmartThings: отключённые компоненты не загружены", "device_id", d.ID, "err", err)
			}
			continue
		}
		d.DisabledComponents = disabledComponents(st)
	}
}

// setDisabled запоминает отключённые компоненты устройства. Новый список меняет приборы, которые
// видит Алиса, поэтому шлюз собирает их заново.
func (c *Cloud) setDisabled(id string, list []string) {
	c.mu.Lock()
	changed := false
	for i := range c.devices {
		if c.devices[i].ID == id && !slices.Equal(c.devices[i].DisabledComponents, list) {
			c.devices[i].DisabledComponents = list
			changed = true
		}
	}
	c.mu.Unlock()
	if changed {
		signal(c.changes)
	}
}

// status - состояние устройства.
func (c *Cloud) status(ctx context.Context, id string) (Status, error) {
	var st struct {
		Components Status `json:"components"`
	}
	err := c.API.do(ctx, http.MethodGet, "/v1/devices/"+url.PathEscape(id)+"/status", nil, &st)
	return st.Components, err
}

// sameHost сообщает, что адрес - на облаке SmartThings из настроек.
func (c *Cloud) sameHost(raw string) bool {
	u, err := url.Parse(raw)
	c.API.mu.Lock()
	base, berr := url.Parse(c.API.base)
	c.API.mu.Unlock()
	return err == nil && berr == nil && u.Scheme == base.Scheme && u.Host == base.Host
}

func defKey(r CapRef) string {
	return fmt.Sprintf("%s-%d", r.ID, max(r.Version, 1))
}

// def - описание capability из памяти, а при первом обращении - с диска; nil - не загружено.
func (c *Cloud) def(r CapRef) *CapDef {
	key := defKey(r)
	c.mu.Lock()
	d, ok := c.defs[key]
	c.mu.Unlock()
	if ok {
		return d
	}
	var cd CapDef
	path := filepath.Join(c.dir, "capabilities", key+".json")
	if err := readJSON(path, maxCapFile, &cd); err != nil || cd.ID == "" {
		return nil
	}
	c.mu.Lock()
	c.defs[key] = &cd
	c.mu.Unlock()
	return &cd
}

// loadDefs загружает описания capability, которых ещё нет: каждое - один запрос, а потом
// лежит на диске. Описание capability не меняется в пределах версии.
func (c *Cloud) loadDefs(ctx context.Context, refs []CapRef) bool {
	loaded := false
	seen := map[string]bool{}
	for _, r := range refs {
		key := defKey(r)
		if seen[key] || c.def(r) != nil || !safeCapID(r.ID) {
			continue
		}
		seen[key] = true
		var cd CapDef
		err := c.API.do(ctx, http.MethodGet, fmt.Sprintf("/v1/capabilities/%s/%d", url.PathEscape(r.ID), max(r.Version, 1)), nil, &cd)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("SmartThings: описание capability не загружено", "capability", r.ID, "err", err)
			}
			continue
		}
		data, err := json.Marshal(&cd)
		if err == nil && len(data) <= maxCapFile {
			err = atomicfile.Write(filepath.Join(c.dir, "capabilities", key+".json"), data)
		}
		if err != nil {
			c.log.Warn("SmartThings: описание capability не сохранено", "capability", r.ID, "err", err)
		}
		c.mu.Lock()
		c.defs[key] = &cd
		c.mu.Unlock()
		loaded = true
	}
	return loaded
}

// safeCapID - имя capability, которое можно вписать в адрес и в имя файла.
func safeCapID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return !strings.Contains(id, "..")
}

// Appliance - прибор: компонент component устройства deviceID; пусто - main. Для пары -
// всегда один объект: он переживает смену имени и профиля прибора в настройках.
func (c *Cloud) Appliance(deviceID, component string) *Appliance {
	if component == "" {
		component = mainComponent
	}
	key := deviceID + "/" + component
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.appliances[key]
	if a == nil {
		a = &Appliance{cloud: c, deviceID: deviceID, component: component, values: map[string]any{},
			units: map[string]string{}, lists: map[string]map[string][]string{}, limits: map[string]limit{},
			disabled: map[string][]string{}}
		c.appliances[key] = a
	}
	return a
}

func (c *Cloud) attach(a *Appliance) {
	c.mu.Lock()
	c.attached[a] = true
	connected := c.connected
	c.mu.Unlock()
	signal(c.fetch)
	if connected {
		go c.prepare(context.Background(), a.deviceID)
	}
}

func (c *Cloud) detach(a *Appliance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.attached, a)
}

// running - работающие приборы: все или устройства id.
func (c *Cloud) running(id string) []*Appliance {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Appliance
	for a := range c.attached {
		if id == "" || a.deviceID == id {
			out = append(out, a)
		}
	}
	return out
}

func (c *Cloud) runningIDs() []string {
	var ids []string
	for _, a := range c.running("") {
		if !slices.Contains(ids, a.deviceID) {
			ids = append(ids, a.deviceID)
		}
	}
	slices.Sort(ids)
	return ids
}

// watchList загружает список устройств, если в нём нет устройства из настроек или нет
// описаний capability: при входе, при новом приборе и раз в 5 минут, пока чего-то не хватает.
func (c *Cloud) watchList(ctx context.Context) {
	for {
		drain(c.fetch)
		if c.API.Configured() && c.API.LoggedIn() {
			c.loadMissing(ctx)
		}
		t := time.NewTimer(listRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-c.fetch:
		case <-t.C:
		}
		t.Stop()
	}
}

func (c *Cloud) loadMissing(ctx context.Context) {
	var missing bool
	var refs []CapRef
	for _, a := range c.running("") {
		d := c.device(a.deviceID)
		if d == nil {
			missing = true
			continue
		}
		refs = append(refs, d.capabilities(a.component)...)
	}
	if missing {
		if err := c.RefreshAccount(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("SmartThings: список устройств не загружен", "err", err)
		}
		return
	}
	if c.loadDefs(ctx, refs) {
		signal(c.changes)
	}
}

// Run держит поток событий и переподключается после обрыва, пока ctx не отменён.
func (c *Cloud) Run(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchList(ctx)
	}()
	defer func() { <-done }()
	drain(c.wake)
	pause := reconnectMin
	var subID string
	for {
		if !c.API.Configured() || !c.API.LoggedIn() {
			c.setConnected(false, "")
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}
		started := time.Now()
		id, err := c.stream(ctx, subID)
		subID = id
		c.setConnected(false, errText(err))
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errGoodbye) {
			// Поток закрыло облако: новая подписка сразу, старая уже не действует.
			subID = ""
			continue
		}
		if time.Since(started) >= stableStream {
			pause = reconnectMin
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.RetryAfter > pause {
			pause = min(ae.RetryAfter, reconnectMax)
		}
		c.log.Warn("SmartThings: нет потока событий, переподключение", "err", err, "after", pause)
		metrics.SourceReconnect(SourceName, "")
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			pause = reconnectMin
		case <-time.After(pause):
			pause = min(pause*2, reconnectMax)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *Cloud) setConnected(up bool, lastErr string) {
	c.mu.Lock()
	changed := c.connected != up
	c.connected = up
	if !up && lastErr != "" {
		c.lastErr = lastErr
	}
	if up {
		c.lastErr = ""
	}
	c.mu.Unlock()
	metrics.SourceConnected(SourceName, "", up)
	if changed {
		for _, a := range c.running("") {
			a.changed(nil)
		}
	}
}

// subscription - подписка SSE: адрес потока.
type subscription struct {
	ID  string `json:"subscriptionId"`
	URL string `json:"registrationUrl"`
}

// stream - один поток событий: подписка на устройства всех мест, поток SSE и, после welcome,
// состояние каждого прибора: события за время обрыва SmartThings не досылает. Прежнюю
// подписку шлюз удаляет: подписок у приложения - не больше двух.
func (c *Cloud) stream(ctx context.Context, prev string) (string, error) {
	if prev != "" {
		if err := c.API.do(ctx, http.MethodDelete, "/v1/sse/subscriptions/"+url.PathEscape(prev), nil, nil); err != nil {
			c.log.Debug("SmartThings: прежняя подписка не удалена", "err", err)
		}
	}
	var sub subscription
	body := map[string]any{"name": "hcgateway", "subscriptionFilters": []map[string]any{{"type": "LOCATIONIDS",
		"value": []string{"ALL"}, "eventType": []string{"DEVICE_EVENT", "DEVICE_HEALTH_EVENT"}}}}
	if err := c.API.do(ctx, http.MethodPost, "/v1/sse/subscriptions", body, &sub); err != nil {
		return "", fmt.Errorf("подписка SSE: %w", err)
	}
	if !c.sameHost(sub.URL) && !smartThingsHost(sub.URL) {
		return sub.ID, errors.New("адрес потока SSE не на облаке SmartThings")
	}
	token, err := c.API.token(ctx, false)
	if err != nil {
		return sub.ID, err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, sub.URL, nil)
	if err != nil {
		return sub.ID, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	// Поток не закрывается: у клиента нет общего таймаута, обрыв ловит streamIdle.
	resp, err := (&http.Client{}).Do(req)
	c.API.count()
	if err != nil {
		metrics.SourceRequest(SourceName, "error")
		return sub.ID, err
	}
	defer resp.Body.Close()
	metrics.SourceRequest(SourceName, fmt.Sprint(resp.StatusCode))
	if resp.StatusCode != http.StatusOK {
		return sub.ID, &APIError{Status: resp.StatusCode}
	}
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-sctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()
	idle := time.NewTimer(streamIdle)
	defer idle.Stop()
	var event string
	var data []string
	welcomed := false
	for {
		select {
		case <-ctx.Done():
			return sub.ID, nil
		case <-c.wake:
			return sub.ID, errors.New("настройки или вход SmartThings изменились")
		case err := <-readErr:
			if err == nil {
				err = errors.New("поток SSE закрыт")
			}
			return sub.ID, err
		case <-idle.C:
			return sub.ID, errors.New("из потока SSE 5 минут ничего не пришло")
		case line := <-lines:
			idle.Reset(streamIdle)
			switch {
			case line == "":
				if len(data) == 0 {
					continue
				}
				payload := strings.Join(data, "\n")
				data = data[:0]
				if strings.Contains(payload, "goodbye") || strings.Contains(payload, "goobye") {
					return sub.ID, errGoodbye
				}
				if !welcomed && strings.Contains(payload, "welcome") {
					welcomed = true
					c.setConnected(true, "")
					c.log.Info("SmartThings: поток событий открыт")
					go func() {
						for _, id := range c.runningIDs() {
							c.prepare(sctx, id)
						}
					}()
					continue
				}
				c.handle(event, payload)
				event = ""
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
	}
}

// smartThingsHost - адрес на домене smartthings.com по https.
func smartThingsHost(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && (u.Hostname() == "smartthings.com" || strings.HasSuffix(u.Hostname(), ".smartthings.com"))
}

// event - событие потока SSE.
type event struct {
	EventType   string `json:"eventType"`
	DeviceEvent *struct {
		DeviceID    string          `json:"deviceId"`
		ComponentID string          `json:"componentId"`
		Capability  string          `json:"capability"`
		Attribute   string          `json:"attribute"`
		Value       json.RawMessage `json:"value"`
		Unit        string          `json:"unit"`
		// StateChange - новое ли значение; у нажатия кнопки false значит, что нажатия не было.
		StateChange *bool `json:"stateChange"`
	} `json:"deviceEvent"`
	// Формат события связи документация не описывает: шлюз берёт deviceId и state или status.
	HealthEvent *struct {
		DeviceID string `json:"deviceId"`
		State    string `json:"state"`
		Status   string `json:"status"`
	} `json:"deviceHealthEvent"`
}

// handle разбирает событие: новое значение атрибута или связь устройства.
func (c *Cloud) handle(name, payload string) {
	var e event
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		c.log.Debug("SmartThings: событие не разобрано", "event", name, "err", err)
		return
	}
	switch {
	case e.DeviceEvent != nil:
		d := e.DeviceEvent
		v := AttrValue{Value: d.Value, Unit: d.Unit, StateChange: d.StateChange}
		if d.ComponentID == mainComponent && d.Capability == capDisabledComponents {
			c.setDisabled(d.DeviceID, listValues(v))
		}
		for _, a := range c.running(d.DeviceID) {
			a.apply(d.ComponentID, d.Capability, d.Attribute, v)
		}
	case e.HealthEvent != nil:
		h := e.HealthEvent
		state := h.State
		if state == "" {
			state = h.Status
		}
		c.setHealth(h.DeviceID, state != "OFFLINE")
	}
}

func (c *Cloud) setHealth(id string, online bool) {
	c.mu.Lock()
	changed := c.health[id] != online
	c.health[id] = online
	c.mu.Unlock()
	if changed {
		for _, a := range c.running(id) {
			a.changed(nil)
		}
	}
}

func (c *Cloud) online(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health[id]
}

// prepare загружает состояние и связь устройства.
func (c *Cloud) prepare(ctx context.Context, id string) {
	var health struct {
		State string `json:"state"`
	}
	if err := c.API.do(ctx, http.MethodGet, "/v1/devices/"+url.PathEscape(id)+"/health", nil, &health); err != nil {
		if ctx.Err() == nil {
			c.log.Warn("SmartThings: связь устройства не загружена", "device_id", id, "err", err)
		}
	} else {
		c.setHealth(id, health.State != "OFFLINE")
	}
	st, err := c.status(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("SmartThings: состояние устройства не загружено", "device_id", id, "err", err)
		}
		return
	}
	c.setDisabled(id, disabledComponents(st))
	for _, a := range c.running(id) {
		a.load(st)
	}
}

// command отправляет команду устройству.
func (c *Cloud) command(ctx context.Context, deviceID, component, capability, name string, args []any) error {
	if args == nil {
		args = []any{}
	}
	body := map[string]any{"commands": []map[string]any{{"component": component, "capability": capability,
		"command": name, "arguments": args}}}
	return c.API.do(ctx, http.MethodPost, "/v1/devices/"+url.PathEscape(deviceID)+"/commands", body, nil)
}
