// Package gateway - работающий шлюз: приборы и подключение к Яндексу по текущим настройкам.
// Настройки применяются без перезапуска: Apply пересоздаёт только изменённые приборы.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/config"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/discovery"
	"hcgateway/internal/hcapi"
	"hcgateway/internal/hccloud"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/hclocal"
	"hcgateway/internal/hcsource"
	"hcgateway/internal/linking"
	"hcgateway/internal/metrics"
	"hcgateway/internal/polaris"
	"hcgateway/internal/profile"
	"hcgateway/internal/samsungtv"
	"hcgateway/internal/smartthings"
	"hcgateway/internal/statedb"
	"hcgateway/internal/thinq"
	"hcgateway/internal/webos"
	"hcgateway/internal/wled"
	"hcgateway/internal/yandex"
)

// DemoDuration - сколько идёт программа демо-прибора.
const DemoDuration = 2 * time.Minute

// Имена работающей и выбранной программы среди последних значений прибора.
const (
	ValueProgram  = "программа"
	ValueSelected = "выбранная программа"
)

// Gateway - приборы и подключение к Яндексу по применённым настройкам.
type Gateway struct {
	Service  *yandex.Service
	Notifier *yandex.Notifier
	Linking  *linking.Server
	Tokens   *linking.Store
	// Cloud - приборы из облака Home Connect и вход в него.
	Cloud *hcsource.Cloud
	// ThinQ - приборы LG из облака ThinQ Connect.
	ThinQ *thinq.Cloud
	// DeLonghi - аккаунт De'Longhi и его кофемашины.
	DeLonghi *delonghi.Account
	// SmartThings - устройства из облака SmartThings.
	SmartThings *smartthings.Cloud
	// Polaris - приборы Polaris на встроенном брокере MQTT.
	Polaris *polaris.Hub
	// Discovery - поиск приборов в домашней сети.
	Discovery *discovery.Search
	// Keys - загрузка ключей приборов Home Connect со страницы интерфейса.
	Keys *hccloud.Flow
	// CheckLocal проверяет связь с прибором Home Connect в домашней сети его ключами.
	CheckLocal func(ctx context.Context, haID, host string) error
	// appliances - ключи и описания приборов Home Connect для домашней сети, по каталогу на haId.
	appliances string
	// dataDir - каталог данных: в нём файлы телевизоров.
	dataDir string
	// tvChanges - телевизор сообщил новые входы: Алисе нужен новый список устройств.
	tvChanges chan struct{}
	log       *slog.Logger
	// demoDuration - длительность программы демо-приборов; тесты её сокращают.
	demoDuration time.Duration
	// links - с какого времени приборы не на связи; now - часы, nil - time.Now.
	links links
	now   func() time.Time
	// state - база состояния; nil - связь, значения и история только в памяти.
	state *statedb.DB

	// cfg - применённые настройки; читаются без блокировки.
	cfg atomic.Pointer[config.Config]

	mu      sync.Mutex
	devices map[string]*built
	// waiting - приборы облака, чей тип облако ещё не сообщило: в Алисе их пока нет.
	waiting map[string]bool
}

// errWaiting - тип прибора ещё не известен: прибор собирается, когда облако его сообщит.
var errWaiting = errors.New("тип прибора ещё не известен")

// built - прибор, собранный по настройкам.
type built struct {
	conf config.Device
	// haType и typeProfile - тип Home Connect и переопределение типа, по которым собран профиль.
	haType      string
	typeProfile *profile.Profile
	appliance   appliance.Appliance
	dev         alice.Device
}

// New создаёт шлюз без приборов и без настроек Яндекса: их задаёт Apply.
// login проверяет логин и пароль на странице привязки. В dataDir лежат токены облака
// Home Connect и описания его приборов.
func New(dataDir string, tokens *linking.Store, login linking.Login, log *slog.Logger) (*Gateway, error) {
	api, err := hcapi.New(filepath.Join(dataDir, "homeconnect-tokens.json"), log)
	if err != nil {
		return nil, err
	}
	lg, err := thinq.New(filepath.Join(dataDir, "thinq-client.json"), filepath.Join(dataDir, "thinq"), log)
	if err != nil {
		return nil, err
	}
	dl, err := delonghi.New(filepath.Join(dataDir, "delonghi", "session.json"), log)
	if err != nil {
		return nil, err
	}
	st, err := smartthings.New(filepath.Join(dataDir, "smartthings-tokens.json"), filepath.Join(dataDir, "smartthings"), log)
	if err != nil {
		return nil, err
	}
	n := yandex.NewNotifier(yandex.DefaultNotifyBase, "", "", yandex.UserID, log)
	cloud := hcsource.New(api, filepath.Join(dataDir, "homeconnect"), log)
	return &Gateway{
		ThinQ:       lg,
		DeLonghi:    dl,
		SmartThings: st,
		Polaris:     polaris.New(filepath.Join(dataDir, "polaris"), log),
		Discovery:   discovery.New(nil),
		Keys:        hccloud.NewFlow(dataDir, cloud.API, cloud.LoggedIn, log, hccloud.EU, hccloud.NA, hccloud.China),
		CheckLocal: func(ctx context.Context, haID, host string) error {
			return hclocal.Check(ctx, filepath.Join(dataDir, "appliances", haID), haID, host)
		},
		dataDir:      dataDir,
		tvChanges:    make(chan struct{}, 1),
		Service:      yandex.NewService(nil, tokens, n, log),
		Notifier:     n,
		Linking:      linking.NewServer(linking.Config{}, login, tokens, log),
		Tokens:       tokens,
		Cloud:        cloud,
		appliances:   filepath.Join(dataDir, "appliances"),
		log:          log,
		demoDuration: DemoDuration,
		links:        newLinks(time.Now()),
		devices:      map[string]*built{},
	}, nil
}

// Attach подключает базу состояния: шлюз берёт из неё связь приборов и отмечает в истории приборов,
// сколько не работал. Вызывается до первого Apply.
func (g *Gateway) Attach(state *statedb.DB) {
	g.state = state
	alive := state.Alive()
	g.links.restore(state.Links(), alive)
	if now := g.clock(); !alive.IsZero() {
		state.MarkGap(statedb.Change{Time: now, Value: "не работал " + hcdevice.Duration(now.Sub(alive).Seconds())})
	}
}

// Run держит связь с приборами и облаком и отправляет уведомления, пока ctx не отменён.
func (g *Gateway) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(9)
	for _, c := range g.startClouds() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refreshOnStart(ctx, g.log, c, startRefreshPause)
		}()
	}
	go func() {
		defer wg.Done()
		g.state.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		g.Cloud.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		g.Cloud.API.KeepFresh(ctx)
	}()
	go func() {
		defer wg.Done()
		g.Keys.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		g.watchLinks(ctx)
	}()
	go func() {
		defer wg.Done()
		g.DeLonghi.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		g.ThinQ.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		g.SmartThings.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		g.watchCloud(ctx)
	}()
	g.Service.Run(ctx)
	wg.Wait()
}

const (
	// startRefreshTries и startRefreshPause - сколько раз и с какой паузой шлюз пробует обновить данные
	// облака при запуске: после отключения света сервер запускается раньше роутера.
	startRefreshTries = 12
	startRefreshPause = 5 * time.Minute
	// startRefreshTimeout - предел одной попытки: она ходит в облако несколькими запросами.
	startRefreshTimeout = 2 * time.Minute
)

// startCloud - облако, которое шлюз обновляет при запуске.
type startCloud struct {
	name string
	// ready - интеграция настроена.
	ready func() bool
	load  func(ctx context.Context) error
}

// startClouds - облака, данные которых при запуске пусты или лежат на диске и могли устареть. Home
// Connect здесь нет: он загружает список при подключении потока событий.
func (g *Gateway) startClouds() []startCloud {
	return []startCloud{
		{"LG ThinQ", g.ThinQ.API.Ready, g.ThinQ.RefreshAccount},
		{"SmartThings", func() bool { return g.SmartThings.API.Configured() && g.SmartThings.API.LoggedIn() }, g.SmartThings.RefreshAccount},
		{"De'Longhi", func() bool { return g.DeLonghi.Status().LoggedIn }, g.DeLonghi.RefreshDevices},
	}
}

// refreshOnStart обновляет данные настроенного облака при запуске шлюза. Сбой не мешает работе: приборы
// работают по копии с диска, а данные можно обновить кнопкой.
func refreshOnStart(ctx context.Context, log *slog.Logger, c startCloud, pause time.Duration) {
	for try := 1; c.ready(); try++ {
		attempt, cancel := context.WithTimeout(ctx, startRefreshTimeout)
		err := c.load(attempt)
		cancel()
		if err == nil {
			log.Info("данные облака обновлены при запуске", "cloud", c.name)
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("данные облака не обновлены при запуске", "cloud", c.name, "try", try, "err", err)
		if try >= startRefreshTries {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// watchCloud пересобирает приборы, когда облако сообщило тип, программы или команды прибора
// или загружен профиль прибора LG: ждавший прибор появляется в Алисе, а Яндекс получает
// новый список устройств.
func (g *Gateway) watchCloud(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.Cloud.Changes():
			g.reapply("Home Connect")
		case <-g.ThinQ.Changes():
			g.reapply("LG ThinQ")
		case <-g.SmartThings.Changes():
			g.reapply("SmartThings")
		case <-g.Polaris.Changes():
			g.reapply("Polaris")
		case <-g.tvChanges:
			g.reapply("телевизор")
		}
	}
}

// reapply пересобирает приборы по применённым настройкам после новых данных облака.
func (g *Gateway) reapply(cloud string) {
	if cfg := g.cfg.Load(); cfg != nil {
		if err := g.Apply(cfg); err != nil {
			g.log.Error("приборы не пересобраны по данным облака", "cloud", cloud, "err", err)
		}
	}
}

// Handler обслуживает запросы Яндекса. Пока не заданы client_id и client_secret,
// шлюз отвечает 404: навык ещё не настроен.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	g.Service.Register(mux)
	g.Linking.Register(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg := g.cfg.Load(); cfg == nil || !cfg.Yandex.Ready() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Config - применённые настройки или nil до первого Apply.
func (g *Gateway) Config() *config.Config {
	return g.cfg.Load()
}

// Check проверяет, что настройки применятся: профили собираются, источники готовы.
// Работающий шлюз не меняется.
func (g *Gateway) Check(cfg *config.Config) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, _, _, err := g.prepare(cfg)
	return err
}

// Apply применяет настройки. Прибор с прежними настройками остаётся тем же объектом
// и не переподключается, а прибор с прежним источником сохраняет состояние.
// При ошибке не меняется ничего. После первого применения Яндекс получает callback/discovery.
func (g *Gateway) Apply(cfg *config.Config) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	next, list, changed, err := g.prepare(cfg)
	if err != nil {
		return err
	}
	h := cfg.HomeConnect
	g.Cloud.API.Configure(hcapi.Config{ClientID: h.ClientID, ClientSecret: h.ClientSecret, BaseURL: h.APIURL})
	g.ThinQ.Configure(cfg.ThinQ.PAT, cfg.ThinQ.Country, cfg.ThinQ.APIURL)
	g.DeLonghi.Configure(cfg.DeLonghi.GigyaURL, cfg.DeLonghi.APIURL, cfg.DeLonghi.IoTURL)
	g.SmartThings.Configure(cfg.SmartThings.ClientID, cfg.SmartThings.ClientSecret, cfg.SmartThings.APIURL)
	y := cfg.Yandex
	prev := g.cfg.Load()
	if y.SkillID == "" && (prev == nil || prev.Yandex.SkillID != "") {
		g.log.Warn("yandex.skill_id не задан, уведомления Яндексу не отправляются")
	}
	g.Notifier.Configure(y.SkillID, y.CallbackToken)
	g.Linking.SetConfig(linking.Config{ClientID: y.ClientID, ClientSecret: y.ClientSecret,
		RedirectURIs: slices.Clone(y.RedirectURIs)})
	if changed {
		g.Service.SetDevices(list)
	}
	g.devices = next
	g.waiting = map[string]bool{}
	for id := range cfg.Devices {
		if next[id] == nil {
			g.waiting[id] = true
		}
	}
	g.cfg.Store(cfg)
	if prev != nil {
		g.Service.Discovery()
	}
	return nil
}

// prepare собирает приборы по настройкам. changed - изменился ли список устройств Алисы.
// Прибор облака, чей тип ещё не известен, пропускается без ошибки.
func (g *Gateway) prepare(cfg *config.Config) (map[string]*built, []alice.Device, bool, error) {
	next := map[string]*built{}
	var list []alice.Device
	changed := false
	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		prev := g.devices[id]
		b, err := g.build(cfg, id, cfg.Devices[id], prev)
		if errors.Is(err, errWaiting) {
			continue
		}
		if err != nil {
			return nil, nil, false, fmt.Errorf("devices.%s: %w", id, err)
		}
		if b != prev {
			changed = true
		}
		next[id] = b
		list = append(list, b.dev)
	}
	return next, list, changed || len(next) != len(g.devices), nil
}

// build собирает прибор или возвращает prev, если его настройки не изменились.
// Если не изменился источник, новый прибор берёт прежний объект источника.
func (g *Gateway) build(cfg *config.Config, id string, dc config.Device, prev *built) (*built, error) {
	switch dc.Source {
	case config.SourceWLED:
		// Ленту дёшево пересоздать: любая правка её настроек пересоздаёт её.
		if prev != nil && sameDevice(prev.conf, dc) {
			return prev, nil
		}
		return &built{conf: dc, dev: &wled.Device{ID: id, Name: dc.Name, Room: dc.Room, Host: dc.Host,
			Presets: maps.Clone(dc.Presets), Log: g.log}}, nil
	}
	var a appliance.Appliance
	if prev != nil && sameSource(prev.conf, dc) {
		a = prev.appliance
	} else {
		var err error
		if a, err = g.newAppliance(id, dc); err != nil {
			return nil, err
		}
	}
	haType := a.Describe().Type
	if haType == "" {
		return nil, errWaiting
	}
	forType := cfg.Profiles[haType]
	// Тип, который шлюз сам не знает, ждёт своего профиля: иначе один новый прибор сломал бы все настройки.
	if dc.Source == config.SourcePolaris && profile.BuiltinProfile(haType) == nil && forType == nil && dc.Profile == nil {
		return nil, errWaiting
	}
	if prev != nil && a == prev.appliance && sameDevice(prev.conf, dc) && reflect.DeepEqual(prev.typeProfile, forType) {
		return prev, nil
	}
	forDevice := dc.Profile
	if dc.Source == config.SourceDeLonghi {
		// recipes и default_drink - слой профиля прибора под его profile.
		forDevice = profile.Merge(profile.DeLonghiLayer(dc.Recipes, dc.DefaultDrink), dc.Profile)
	}
	p, err := profile.Resolve(haType, forType, forDevice)
	if err != nil {
		return nil, err
	}
	dev := &hcdevice.Device{ID: id, Name: dc.Name, Room: dc.Room, Profile: p, Appliance: a,
		Manufacturer: manufacturer(dc.Source, a)}
	g.keepHistory(dev)
	return &built{conf: dc, haType: haType, typeProfile: forType, appliance: a, dev: dev}, nil
}

// keepHistory подключает историю прибора к базе состояния: новый объект прибора после правки
// настроек получает прежние строки.
func (g *Gateway) keepHistory(dev *hcdevice.Device) {
	if g.state == nil {
		return
	}
	saved := g.state.Changes(dev.ID)
	history := make([]hcdevice.Change, len(saved))
	for i, c := range saved {
		history[i] = hcdevice.Change{Time: c.Time, Kind: c.Kind, Key: c.Key, Value: c.Value}
	}
	dev.Restore(history)
	dev.Journal = func(changes []hcdevice.Change) {
		out := make([]statedb.Change, len(changes))
		for i, c := range changes {
			out[i] = statedb.Change{Time: c.Time, Kind: c.Kind, Key: c.Key, Value: c.Value}
		}
		g.state.AddChanges(dev.ID, out)
	}
}

// sameDevice - настройки прибора не изменились для шлюза: значок нужен только интерфейсу,
// и его смена прибор не пересобирает.
func sameDevice(a, b config.Device) bool {
	a.Icon, b.Icon = "", ""
	return reflect.DeepEqual(a, b)
}

// manufacturer - производитель в списке устройств Алисы. У устройства SmartThings - тот,
// которого сообщает облако.
func manufacturer(source string, a appliance.Appliance) string {
	switch source {
	case config.SourceThinQ, config.SourceWebOS:
		return "LG"
	case config.SourceDeLonghi:
		return "De'Longhi"
	case config.SourcePolaris:
		return "Polaris"
	case config.SourceSamsungTV:
		return "Samsung"
	case config.SourceSmartThings:
		if m, ok := a.(interface{ Manufacturer() string }); ok && m.Manufacturer() != "" {
			return m.Manufacturer()
		}
		return "SmartThings"
	}
	return "BSH"
}

func (g *Gateway) newAppliance(id string, dc config.Device) (appliance.Appliance, error) {
	switch dc.Source {
	case config.SourceDemo:
		return appliance.NewDemo(dc.Demo, g.demoDuration)
	case config.SourceHomeConnectCloud:
		return g.Cloud.Appliance(dc.HaID), nil
	case config.SourceHomeConnectLocal:
		return hclocal.Open(id, filepath.Join(g.appliances, dc.HaID), dc.HaID, dc.Host, g.log)
	case config.SourceThinQ:
		return g.ThinQ.Appliance(dc.DeviceID, dc.Location)
	case config.SourceDeLonghi:
		profileID := 1
		if dc.ProfileID != nil {
			profileID = *dc.ProfileID
		}
		return g.DeLonghi.Appliance(delonghi.Config{ID: id, Host: dc.Host, Serial: dc.Serial, Pin: dc.Pin, ProfileID: profileID})
	case config.SourceWebOS:
		return webos.New(webos.Config{ID: id, Host: dc.Host, MAC: dc.MAC, Broadcast: dc.Broadcast,
			Dir: filepath.Join(g.dataDir, "webos"), Changed: g.tvChanged}, g.log)
	case config.SourceSamsungTV:
		return samsungtv.New(samsungtv.Config{ID: id, Host: dc.Host, MAC: dc.MAC, Broadcast: dc.Broadcast,
			Dir: filepath.Join(g.dataDir, "samsung-tv")}, g.log)
	case config.SourceSmartThings:
		return g.SmartThings.Appliance(dc.DeviceID, dc.Location), nil
	case config.SourcePolaris:
		return g.Polaris.Appliance(dc.DeviceID), nil
	}
	return nil, fmt.Errorf("источник %s не поддерживается", dc.Source)
}

// tvChanged - телевизор сообщил новые входы: шлюз пересобирает приборы, а Яндекс получает
// новый список устройств.
func (g *Gateway) tvChanged() {
	select {
	case g.tvChanges <- struct{}{}:
	default:
	}
}

// sameSource - прибор подключён так же: отличаются только имя, комната или профиль.
// Рецепты и напиток кофемашины - тоже профиль.
func sameSource(a, b config.Device) bool {
	a.Name, a.Room, a.Icon, a.Profile, a.Recipes, a.DefaultDrink = "", "", "", nil, nil, ""
	b.Name, b.Room, b.Icon, b.Profile, b.Recipes, b.DefaultDrink = "", "", "", nil, nil, ""
	return reflect.DeepEqual(a, b)
}

// Device - устройство Алисы прибора с идентификатором id из настроек.
func (g *Gateway) Device(id string) (alice.Device, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.devices[id]
	if b == nil {
		return nil, false
	}
	return b.dev, true
}

// Waiting сообщает, что прибор облака ждёт своего типа от облака и в Алисе его пока нет.
func (g *Gateway) Waiting(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting[id]
}

// HomeConnect - прибор Home Connect с идентификатором id, если прибор подключён как Home Connect.
func (g *Gateway) HomeConnect(id string) (*hcdevice.Device, bool) {
	d, ok := g.Device(id)
	if !ok {
		return nil, false
	}
	hc, ok := d.(*hcdevice.Device)
	return hc, ok
}

// Status - связь и состояние программы каждого прибора для метрик.
func (g *Gateway) Status() []metrics.Device {
	g.mu.Lock()
	ids := slices.Sorted(maps.Keys(g.devices))
	list := make([]*built, len(ids))
	for i, id := range ids {
		list[i] = g.devices[id]
	}
	g.mu.Unlock()
	// Приборы опрашиваются без блокировки: Apply в это время может ждать остановки прибора.
	out := make([]metrics.Device, 0, len(list))
	for i, b := range list {
		d := metrics.Device{ID: ids[i], Source: b.conf.Source}
		if hc, ok := b.dev.(*hcdevice.Device); ok {
			st := hc.Appliance.State()
			d.Online = st.Online
			if op, ok := st.Values[appliance.StatusOperationState].(string); ok {
				d.Operation = op[strings.LastIndexByte(op, '.')+1:]
			} else if pw := hc.Profile.Power; pw != nil && pw.State != "" {
				// У прибора LG состояние работы - то, по которому профиль видит, что прибор включён.
				d.Operation, _ = st.Values[pw.State].(string)
			}
		} else {
			d.Online = !slices.ContainsFunc(b.dev.States(), func(s alice.DeviceState) bool { return s.ErrorCode == alice.CodeUnreachable })
		}
		out = append(out, d)
	}
	return out
}

// Strip - лента WLED с идентификатором id, если прибор - лента.
func (g *Gateway) Strip(id string) (*wled.Device, bool) {
	d, ok := g.Device(id)
	if !ok {
		return nil, false
	}
	s, ok := d.(*wled.Device)
	return s, ok
}

// LocalAppliances - приборы, чьи ключи для домашней сети загружены на странице «Подключения» или
// командой fetch-keys.
func (g *Gateway) LocalAppliances() []hclocal.Info {
	return hclocal.List(g.appliances)
}

// Types - тип Home Connect каждого прибора Home Connect по идентификатору.
func (g *Gateway) Types() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]string{}
	for id, b := range g.devices {
		if _, ok := b.dev.(*hcdevice.Device); ok {
			out[id] = b.haType
		}
	}
	return out
}

// TVLink - связь с телевизором в домашней сети для страницы прибора.
type TVLink struct {
	// Paired - телевизор разрешил подключение; Prompt - спрашивает разрешение сейчас.
	Paired, Prompt, Connected bool
	Model, MAC                string
	// UPnP - громкость телевизора Samsung доступна по UPnP; у LG - всегда false.
	UPnP bool
	Err  string
	// Cert - запомненный отпечаток сертификата; NewCert - другой, ждёт решения владельца; Plain -
	// соединение без шифрования.
	Cert, NewCert string
	Plain         bool
}

// TV - телевизор в домашней сети: связь, «подключить заново» и «забыть ключ».
type TV interface {
	Link() TVLink
	Retry()
	Forget() error
	// AcceptCert запоминает новый отпечаток сертификата.
	AcceptCert(fp string) error
}

type lgTV struct{ *webos.TV }

func (t lgTV) Link() TVLink {
	s := t.Status()
	return TVLink{Paired: s.Paired, Prompt: s.Prompt, Connected: s.Connected, Model: s.Model, MAC: s.MAC, Err: s.Err,
		Cert: s.Cert, NewCert: s.NewCert, Plain: s.Plain}
}

type samsungTV struct{ *samsungtv.TV }

func (t samsungTV) Link() TVLink {
	s := t.Status()
	return TVLink{Paired: s.Paired, Prompt: s.Prompt, Connected: s.Connected, Model: s.Model, MAC: s.MAC, UPnP: s.UPnP, Err: s.Err,
		Cert: s.Cert, NewCert: s.NewCert, Plain: s.Plain}
}

// Television - телевизор в домашней сети с идентификатором id, если прибор - телевизор.
func (g *Gateway) Television(id string) (TV, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.devices[id]
	if b == nil {
		return nil, false
	}
	switch a := b.appliance.(type) {
	case *webos.TV:
		return lgTV{a}, true
	case *samsungtv.TV:
		return samsungTV{a}, true
	}
	return nil, false
}
