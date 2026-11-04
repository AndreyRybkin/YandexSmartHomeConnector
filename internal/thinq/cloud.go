package thinq

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"hcgateway/internal/atomicfile"
	"hcgateway/internal/metrics"
)

const (
	// Пауза перед переподключением MQTT: от 10 с до 10 минут, удваивается после каждой неудачи.
	reconnectMin = 10 * time.Second
	reconnectMax = 10 * time.Minute
	// stableSession - соединение, которое проработало столько, сбрасывает паузу.
	stableSession = time.Minute
	// mqttTimeout - предел подключения и подписки MQTT.
	mqttTimeout = 20 * time.Second
	// keepAlive - проверка соединения MQTT.
	keepAlive = 30 * time.Second
	// resubscribe - как часто продлевать подписки на приборы: подписка на события действует 4464 часа.
	resubscribe = 24 * time.Hour
	// maxClientFile и maxProfileFile - пределы файла клиента и файла профиля прибора.
	maxClientFile  = 64 << 10
	maxProfileFile = 1 << 20
	// profileRetry - через сколько снова загружать профиль, который не загрузился.
	profileRetry = 5 * time.Minute
)

// errReconfigured - PAT или страна изменились: MQTT подключается заново.
var errReconfigured = errors.New("настройки ThinQ изменились")

// credentials - клиент MQTT шлюза: идентификатор, ключ, сертификат LG и топик.
// Хранится в thinq-client.json: LG предупреждает, что за слишком много клиентов может
// заблокировать вызовы API, поэтому идентификатор создаётся один раз.
type credentials struct {
	ClientID    string `json:"client_id"`
	Key         string `json:"key,omitempty"`
	Certificate string `json:"certificate,omitempty"`
	Topic       string `json:"topic,omitempty"`
	// PAT - SHA-256 PAT, для которого выдан сертификат: с другим PAT клиент регистрируется заново.
	PAT string `json:"pat,omitempty"`
}

// stored - профиль прибора на диске: тип прибора и профиль LG как есть.
type stored struct {
	DeviceType string          `json:"deviceType"`
	Profile    json.RawMessage `json:"profile"`
}

// Cloud - аккаунт LG: клиент API, профили приборов, одно соединение MQTT и приборы.
type Cloud struct {
	API  *Client
	path string
	// dir - каталог профилей приборов, data_dir/thinq.
	dir string
	log *slog.Logger
	// roots - корневые сертификаты сервера MQTT; nil - системные. Тесты подставляют свои.
	roots *x509.CertPool
	wake  chan struct{}
	// changes - профиль прибора загружен: шлюз пересобирает приборы. fetch - нужен профиль.
	changes chan struct{}
	fetch   chan struct{}

	mu    sync.Mutex
	creds credentials
	// appliances - прибор или камера по deviceId и камере: один объект на пару.
	appliances map[string]*Appliance
	// attached - приборы, которые сейчас работают в Алисе.
	attached  map[*Appliance]bool
	profiles  map[string]*stored
	account   []DeviceInfo
	connected bool
}

// New открывает файл клиента path, а если его нет - создаёт идентификатор клиента.
// dir - каталог профилей приборов.
func New(path, dir string, log *slog.Logger) (*Cloud, error) {
	c := &Cloud{path: path, dir: dir, log: log, wake: make(chan struct{}, 1), changes: make(chan struct{}, 1),
		fetch: make(chan struct{}, 1), appliances: map[string]*Appliance{}, attached: map[*Appliance]bool{},
		profiles: map[string]*stored{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		id, err := NewClientID()
		if err != nil {
			return nil, err
		}
		c.creds = credentials{ClientID: id}
		if err := c.save(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case len(data) > maxClientFile:
		return nil, fmt.Errorf("%s больше %d байт", path, maxClientFile)
	default:
		if err := json.Unmarshal(data, &c.creds); err != nil || c.creds.ClientID == "" {
			return nil, fmt.Errorf("%s: неверный файл клиента ThinQ", path)
		}
	}
	c.API = NewClient(c.creds.ClientID)
	return c, nil
}

func (c *Cloud) save() error {
	data, err := json.MarshalIndent(c.creds, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(c.path, data)
}

// Configure задаёт PAT, страну и адрес облака; пустой адрес - по стране. Новые значения
// подключают MQTT заново.
func (c *Cloud) Configure(pat, country, base string) {
	oldBase, oldPAT, oldCountry, _ := c.API.settings()
	c.API.Configure(pat, country, base)
	if newBase, _, _, _ := c.API.settings(); oldPAT == pat && oldCountry == country && oldBase == newBase {
		return
	}
	signal(c.wake)
	signal(c.fetch)
}

// Connected сообщает, подключён ли MQTT.
func (c *Cloud) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Account - приборы аккаунта, как их последний раз сообщило облако.
func (c *Cloud) Account() []DeviceInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.account)
}

// RefreshAccount загружает список приборов аккаунта и заново - профили всех его приборов. Прибор,
// профиль которого не загрузился, не портит ответ остальным: шлюз пишет его в журнал, а ошибку
// возвращает, только если не загрузился ни один профиль.
func (c *Cloud) RefreshAccount(ctx context.Context) error {
	list, err := c.API.Devices(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.account = list
	c.mu.Unlock()
	var first error
	loaded := 0
	for _, d := range list {
		err := c.loadProfile(ctx, d.ID, d.Type)
		switch {
		case err == nil:
			loaded++
		case first == nil:
			first = err
		}
		if err != nil {
			c.log.Warn("ThinQ: профиль прибора не загружен", "device_id", d.ID, "type", d.Type, "err", err)
		}
	}
	if loaded > 0 {
		return nil
	}
	return first
}

// Changes сообщает, что загружен профиль прибора: прибор можно собрать или пересобрать.
func (c *Cloud) Changes() <-chan struct{} {
	return c.changes
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

// Appliance - прибор deviceID или его камера location. Для пары - всегда один объект: он
// переживает смену имени и профиля прибора в настройках. Ошибка - в загруженном профиле
// нет такой камеры.
func (c *Cloud) Appliance(deviceID, location string) (*Appliance, error) {
	c.mu.Lock()
	key := deviceID + "/" + location
	a := c.appliances[key]
	if a == nil {
		a = &Appliance{cloud: c, deviceID: deviceID, location: location}
		c.appliances[key] = a
	}
	st := c.profileLocked(deviceID)
	c.mu.Unlock()
	if st == nil {
		signal(c.fetch)
		return a, nil
	}
	m, err := parseProfile(st.DeviceType, st.Profile, location)
	if err != nil {
		return nil, fmt.Errorf("прибор LG %s: %w", deviceID, err)
	}
	a.mu.Lock()
	if a.model == nil {
		a.model = m
	}
	a.mu.Unlock()
	return a, nil
}

// Locations - камеры или части прибора по загруженному профилю; пусто - камер нет.
func (c *Cloud) Locations(deviceID string) []string {
	c.mu.Lock()
	st := c.profileLocked(deviceID)
	c.mu.Unlock()
	if st == nil {
		return nil
	}
	m, err := parseProfile(st.DeviceType, st.Profile, "")
	if err != nil || len(m.Locations) < 2 {
		return nil
	}
	return m.Locations
}

func (c *Cloud) profilePath(deviceID string) string {
	return filepath.Join(c.dir, deviceID+".json")
}

// profileLocked - профиль прибора из памяти, а при первом обращении - с диска.
func (c *Cloud) profileLocked(deviceID string) *stored {
	if st, ok := c.profiles[deviceID]; ok {
		return st
	}
	data, err := os.ReadFile(c.profilePath(deviceID))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err == nil && len(data) > maxProfileFile:
		err = fmt.Errorf("файл больше %d байт", maxProfileFile)
	}
	var st stored
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err != nil || st.DeviceType == "" {
		c.log.Warn("ThinQ: профиль прибора на диске не прочитан, загружу заново", "device_id", deviceID, "err", err)
		return nil
	}
	c.profiles[deviceID] = &st
	return &st
}

// loadProfile загружает профиль прибора, сохраняет его и задаёт приборам с этим deviceId.
func (c *Cloud) loadProfile(ctx context.Context, deviceID, deviceType string) error {
	raw, err := c.API.Profile(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("профиль прибора LG %s: %w", deviceID, err)
	}
	// Ответ, который не разбирается, не должен заменить рабочий профиль на диске.
	if _, err := parseProfile(deviceType, raw, ""); err != nil {
		return fmt.Errorf("профиль прибора LG %s: %w", deviceID, err)
	}
	st := &stored{DeviceType: deviceType, Profile: raw}
	data, err := json.MarshalIndent(st, "", "  ")
	if err == nil && len(data) > maxProfileFile {
		err = fmt.Errorf("профиль прибора LG %s больше %d байт", deviceID, maxProfileFile)
	}
	if err == nil {
		if err = os.MkdirAll(c.dir, 0o700); err == nil {
			err = atomicfile.Write(c.profilePath(deviceID), data)
		}
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.profiles[deviceID] = st
	var list []*Appliance
	for _, a := range c.appliances {
		if a.deviceID == deviceID {
			list = append(list, a)
		}
	}
	c.mu.Unlock()
	for _, a := range list {
		m, err := parseProfile(deviceType, raw, a.location)
		if err != nil {
			c.log.Warn("ThinQ: профиль не подходит прибору из настроек", "device_id", deviceID, "location", a.location, "err", err)
			continue
		}
		a.setModel(m)
	}
	c.log.Info("ThinQ: профиль прибора загружен", "device_id", deviceID, "type", deviceType)
	signal(c.changes)
	return nil
}

// loadMissing загружает профили приборов из настроек, которых ещё нет: тип прибора берётся
// из списка аккаунта.
func (c *Cloud) loadMissing(ctx context.Context) {
	c.mu.Lock()
	var missing []string
	for _, a := range c.appliances {
		if c.profileLocked(a.deviceID) == nil && !slices.Contains(missing, a.deviceID) {
			missing = append(missing, a.deviceID)
		}
	}
	account := slices.Clone(c.account)
	c.mu.Unlock()
	if len(missing) == 0 || !c.API.Ready() {
		return
	}
	if !slices.ContainsFunc(account, func(d DeviceInfo) bool { return slices.Contains(missing, d.ID) }) {
		list, err := c.API.Devices(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("ThinQ: список приборов не загружен", "err", err)
			}
			return
		}
		c.mu.Lock()
		c.account = list
		c.mu.Unlock()
		account = list
	}
	for _, id := range missing {
		i := slices.IndexFunc(account, func(d DeviceInfo) bool { return d.ID == id })
		if i < 0 {
			c.log.Warn("ThinQ: прибора нет в аккаунте LG", "device_id", id)
			continue
		}
		if err := c.loadProfile(ctx, id, account[i].Type); err != nil && ctx.Err() == nil {
			c.log.Warn("ThinQ: профиль прибора не загружен", "device_id", id, "err", err)
		}
	}
}

// watchProfiles загружает недостающие профили, когда они нужны, и раз в profileRetry, пока
// ctx не отменён.
func (c *Cloud) watchProfiles(ctx context.Context) {
	for {
		// Сигнал, который пришёл до загрузки, загрузка и покрывает.
		drain(c.fetch)
		c.loadMissing(ctx)
		t := time.NewTimer(profileRetry)
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

func (c *Cloud) attach(a *Appliance) {
	c.mu.Lock()
	c.attached[a] = true
	connected := c.connected
	c.mu.Unlock()
	if connected {
		go c.prepare(context.Background(), a.deviceID)
	}
}

func (c *Cloud) detach(a *Appliance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.attached, a)
}

// running - работающие приборы: все или с deviceId id.
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

// runningIDs - deviceId работающих приборов без повторов.
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

// Run загружает профили приборов, держит MQTT и переподключается после обрыва, пока ctx
// не отменён.
func (c *Cloud) Run(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchProfiles(ctx)
	}()
	defer func() { <-done }()
	// Настройки, применённые до запуска, первое подключение и так учтёт.
	drain(c.wake)
	pause := reconnectMin
	for {
		if !c.API.Ready() {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}
		started := time.Now()
		err := c.session(ctx)
		c.setConnected(false)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errReconfigured) || time.Since(started) >= stableSession {
			pause = reconnectMin
		}
		if !errors.Is(err, errReconfigured) {
			c.log.Warn("ThinQ: нет соединения MQTT, переподключение", "err", err, "after", pause)
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
}

func (c *Cloud) setConnected(up bool) {
	c.mu.Lock()
	c.connected = up
	c.mu.Unlock()
	metrics.SourceConnected(SourceName, "", up)
	if !up {
		for _, a := range c.running("") {
			a.offline()
		}
	}
}

// session - одно соединение MQTT: регистрация клиента, адрес сервера, подключение, подписка.
// После подключения шлюз один раз загружает состояние приборов: события за время обрыва потеряны.
func (c *Cloud) session(ctx context.Context) error {
	creds, err := c.register(ctx)
	if err != nil {
		return fmt.Errorf("регистрация клиента MQTT: %w", err)
	}
	server, err := c.API.Route(ctx)
	if err != nil {
		return fmt.Errorf("адрес MQTT: %w", err)
	}
	host := strings.TrimPrefix(server, "mqtts://")
	if !strings.Contains(host, ":") {
		host += ":8883"
	}
	cert, err := tls.X509KeyPair([]byte(creds.Certificate), []byte(creds.Key))
	if err != nil {
		return fmt.Errorf("сертификат клиента MQTT: %w", err)
	}
	lost := make(chan error, 1)
	opts := mqtt.NewClientOptions().AddBroker("tls://" + host).SetClientID(creds.ClientID).
		SetTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: c.roots, MinVersion: tls.VersionTLS12}).
		SetCleanSession(false).SetKeepAlive(keepAlive).SetAutoReconnect(false).SetConnectTimeout(mqttTimeout).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			select {
			case lost <- err:
			default:
			}
		})
	client := mqtt.NewClient(opts)
	if t := client.Connect(); !t.WaitTimeout(mqttTimeout) || t.Error() != nil {
		return fmt.Errorf("подключение MQTT: %w", tokenErr(t))
	}
	defer client.Disconnect(250)
	if t := client.Subscribe(creds.Topic, 1, func(_ mqtt.Client, m mqtt.Message) { c.handle(m.Payload()) }); !t.WaitTimeout(mqttTimeout) || t.Error() != nil {
		return fmt.Errorf("подписка MQTT: %w", tokenErr(t))
	}
	c.setConnected(true)
	c.log.Info("ThinQ: MQTT подключён")
	for _, id := range c.runningIDs() {
		c.prepare(ctx, id)
	}
	refresh := time.NewTicker(resubscribe)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-lost:
			return err
		case <-c.wake:
			return errReconfigured
		case <-refresh.C:
			for _, id := range c.runningIDs() {
				c.subscribe(ctx, id)
			}
		}
	}
}

func tokenErr(t mqtt.Token) error {
	if err := t.Error(); err != nil {
		return err
	}
	return errors.New("таймаут")
}

// prepare подписывает клиента на события прибора и загружает его состояние.
func (c *Cloud) prepare(ctx context.Context, id string) {
	c.subscribe(ctx, id)
	state, err := c.API.State(ctx, id)
	if err != nil {
		c.log.Warn("ThinQ: состояние прибора не загружено", "device_id", id, "err", err)
		return
	}
	for _, a := range c.running(id) {
		a.apply(state, true)
	}
}

func (c *Cloud) subscribe(ctx context.Context, id string) {
	if err := c.API.SubscribePush(ctx, id); err != nil {
		c.log.Warn("ThinQ: подписка на уведомления не оформлена", "device_id", id, "err", err)
	}
	if err := c.API.SubscribeEvents(ctx, id); err != nil {
		c.log.Warn("ThinQ: подписка на события не оформлена", "device_id", id, "err", err)
	}
}

// handle разбирает сообщение MQTT: состояние прибора или уведомление.
func (c *Cloud) handle(payload []byte) {
	var m struct {
		PushType string          `json:"pushType"`
		DeviceID string          `json:"deviceId"`
		Report   json.RawMessage `json:"report"`
		PushCode string          `json:"pushCode"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		c.log.Warn("ThinQ: сообщение MQTT не разобрано", "err", err)
		return
	}
	list := c.running(m.DeviceID)
	if m.DeviceID == "" || len(list) == 0 {
		return
	}
	switch m.PushType {
	case "DEVICE_STATUS":
		for _, a := range list {
			a.apply(m.Report, false)
		}
	case "DEVICE_PUSH":
		// Уведомление получают все камеры прибора: в нём нет камеры.
		c.log.Info("ThinQ: уведомление прибора", "device_id", m.DeviceID, "code", m.PushCode)
		for _, a := range list {
			a.push(strings.ToUpper(m.PushCode))
		}
	}
}

// register регистрирует клиента MQTT и получает сертификат, если его ещё нет или выдан для
// другого PAT. Ключ - RSA 2048, запрос на подпись - с CN lg_thinq и SHA-512, как в библиотеке LG.
func (c *Cloud) register(ctx context.Context) (credentials, error) {
	_, pat, _, _ := c.API.settings()
	sum := sha256.Sum256([]byte(pat))
	patHash := hex.EncodeToString(sum[:])
	c.mu.Lock()
	creds := c.creds
	c.mu.Unlock()
	if creds.Certificate != "" && creds.Key != "" && creds.PAT == patHash {
		return creds, nil
	}
	if err := c.API.RegisterClient(ctx); err != nil {
		return creds, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return creds, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "lg_thinq"}, SignatureAlgorithm: x509.SHA512WithRSA}, key)
	if err != nil {
		return creds, err
	}
	certPEM, topic, err := c.API.IssueCertificate(ctx, base64Lines(der))
	if err != nil {
		return creds, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return creds, err
	}
	creds.Key = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	creds.Certificate, creds.Topic, creds.PAT = certPEM, topic, patHash
	c.mu.Lock()
	c.creds = creds
	err = c.save()
	c.mu.Unlock()
	if err != nil {
		return creds, err
	}
	c.log.Info("ThinQ: клиент MQTT зарегистрирован")
	return creds, nil
}

// base64Lines - запрос на подпись в PEM без первой и последней строки, одной строкой.
func base64Lines(der []byte) string {
	block := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	var out strings.Builder
	for _, line := range strings.Split(block, "\n") {
		if line != "" && !strings.HasPrefix(line, "-----") {
			out.WriteString(line)
		}
	}
	return out.String()
}
