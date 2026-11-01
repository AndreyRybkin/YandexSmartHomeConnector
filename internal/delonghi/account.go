// Package delonghi - кофемашины De'Longhi на платформе приложения My Coffee Lounge:
// вход в аккаунт через Gigya, список машин из облака, управление по домашней сети через
// WebSocket машины и состояние из облака AWS IoT, пока машина его не присылает сама.
package delonghi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"hcgateway/internal/atomicfile"
	"hcgateway/internal/metrics"
)

// Source - имя источника в метриках и настройках.
const Source = "delonghi"

const (
	// DefaultAPIURL - REST облака приложения (AWS API Gateway, prod EU).
	DefaultAPIURL = "https://8q8c9xktb0.execute-api.eu-central-1.amazonaws.com/dlg-prod"
	// appSource - заголовок source запросов облака, как у приложения.
	appSource = "mycoffeelounge"
	// tokenTTL - срок JWT, который выпускает шлюз.
	tokenTTL = 7 * 24 * time.Hour
	// refreshBefore - за сколько до конца срока шлюз продлевает JWT.
	refreshBefore = 2 * 24 * time.Hour
	// refreshRetry - пауза между попытками продления.
	refreshRetry = 6 * time.Hour
	// failuresAlarm - после стольких неудач подряд нужен новый вход.
	failuresAlarm = 3
	// maxSessionFile - предел файла сессии.
	maxSessionFile = 64 << 10
	// httpTimeout - предел запроса к облаку.
	httpTimeout = 15 * time.Second
	// maxDevices - предел списка машин аккаунта.
	maxDevices = 50
)

// ErrNotLoggedIn - вход в аккаунт De'Longhi не выполнен.
var ErrNotLoggedIn = errors.New("вход в аккаунт De'Longhi не выполнен")

// DeviceInfo - машина аккаунта из облака.
type DeviceInfo struct {
	// MachineName - имя машины в AWS IoT, например EUPDL01COF000000000000.
	MachineName string `json:"machineName"`
	Serial      string `json:"serialNumber"`
	Model       string `json:"machineModel"`
	SKU         string `json:"sku"`
	Status      string `json:"status"`
	// Shared - машина чужого аккаунта, к которой дали доступ.
	Shared bool `json:"-"`
}

// Status - вход в аккаунт для страницы «Подключения».
type Status struct {
	LoggedIn bool
	Pool     string
	// Expires - срок JWT; пусто, если JWT ещё нет.
	Expires time.Time
	// LoginRequired - продление не удалось failuresAlarm раз подряд: нужен новый вход.
	LoginRequired bool
	Failures      int
}

// Account - аккаунт De'Longhi: сессия Gigya, JWT, список машин и машины в Алисе.
type Account struct {
	path string
	log  *slog.Logger
	http *http.Client
	now  func() time.Time
	// wake будит цикл продления после входа и выхода.
	wake chan struct{}

	mu sync.Mutex
	// gigyaURL, apiURL и iotURL - адреса Gigya, REST и брокера; тесты подставляют поддельные.
	gigyaURL, apiURL, iotURL string
	sess                     *session
	token                    string
	expires                  time.Time
	failures                 int
	alarmed                  bool
	devices                  []DeviceInfo
	appliances               map[*Appliance]bool
}

// New открывает файл сессии path; если его нет, вход не выполнен.
func New(path string, log *slog.Logger) (*Account, error) {
	a := &Account{path: path, log: log, http: &http.Client{Timeout: httpTimeout}, now: time.Now,
		wake: make(chan struct{}, 1), gigyaURL: DefaultGigyaURL, apiURL: DefaultAPIURL, iotURL: DefaultIoTURL,
		appliances: map[*Appliance]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSessionFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSessionFile {
		return nil, fmt.Errorf("%s больше %d байт", path, maxSessionFile)
	}
	var s session
	if err := json.Unmarshal(data, &s); err != nil || s.Token == "" || s.Secret == "" || s.APIKey == "" {
		return nil, fmt.Errorf("%s: неверный файл сессии De'Longhi", path)
	}
	a.sess = &s
	return a, nil
}

// Configure задаёт адреса Gigya, REST и брокера; пустой адрес - адрес приложения.
func (a *Account) Configure(gigyaURL, apiURL, iotURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gigyaURL, a.apiURL, a.iotURL = or(gigyaURL, DefaultGigyaURL), or(apiURL, DefaultAPIURL), or(iotURL, DefaultIoTURL)
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (a *Account) urls() (gigya, api, iot string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gigyaURL, a.apiURL, a.iotURL
}

// Login входит в аккаунт по почте и паролю и сохраняет сессию. Пароль не сохраняется.
func (a *Account) Login(ctx context.Context, email, password string) error {
	s, err := a.gigyaLogin(ctx, email, password)
	if err != nil {
		return err
	}
	token, exp, err := a.gigyaJWT(ctx, s, tokenTTL)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// atomicfile пишет с правами 0600: в файле секрет сессии.
	if err := atomicfile.Write(a.path, data); err != nil {
		return err
	}
	a.mu.Lock()
	a.sess, a.token, a.expires, a.failures, a.alarmed = &s, token, exp, 0, false
	a.mu.Unlock()
	a.log.Info("De'Longhi: вход выполнен", "pool", s.Pool, "expires", exp)
	signal(a.wake)
	a.wakeAppliances()
	return nil
}

// Logout забывает сессию и удаляет её файл.
func (a *Account) Logout() error {
	a.mu.Lock()
	a.sess, a.token, a.expires, a.failures, a.alarmed, a.devices = nil, "", time.Time{}, 0, false, nil
	a.mu.Unlock()
	signal(a.wake)
	if err := os.Remove(a.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Status - состояние входа.
func (a *Account) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{LoggedIn: a.sess != nil, Expires: a.expires, LoginRequired: a.alarmed, Failures: a.failures}
	if a.sess != nil {
		st.Pool = a.sess.Pool
	}
	return st
}

// Token - действующий JWT. Если его нет или срок вышел, шлюз выпускает новый сразу.
func (a *Account) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	token, exp, s := a.token, a.expires, a.sess
	a.mu.Unlock()
	if s == nil {
		return "", ErrNotLoggedIn
	}
	if token != "" && a.now().Before(exp) {
		return token, nil
	}
	if err := a.refresh(ctx); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token, nil
}

// refresh выпускает новый JWT из сессии. Неудачи считаются: после failuresAlarm подряд
// машины в Алисе сообщают, что нужен новый вход, - один раз до успеха или нового входа.
func (a *Account) refresh(ctx context.Context) error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return ErrNotLoggedIn
	}
	token, exp, err := a.gigyaJWT(ctx, *s, tokenTTL)
	a.mu.Lock()
	if a.sess != s {
		// Пока шёл запрос, владелец вышел или вошёл заново.
		a.mu.Unlock()
		return ErrNotLoggedIn
	}
	if err == nil {
		a.token, a.expires, a.failures, a.alarmed = token, exp, 0, false
		a.mu.Unlock()
		a.log.Info("De'Longhi: токен продлён", "expires", exp)
		return nil
	}
	a.failures++
	alarm := a.failures >= failuresAlarm && !a.alarmed
	if alarm {
		a.alarmed = true
	}
	failures := a.failures
	a.mu.Unlock()
	a.log.Warn("De'Longhi: токен не продлён", "failures", failures, "err", err)
	if alarm {
		a.notifyAppliances(EventLoginRequired)
	}
	return err
}

// Run продлевает JWT за refreshBefore до конца срока, после неудачи - через refreshRetry,
// пока ctx не отменён.
func (a *Account) Run(ctx context.Context) {
	for {
		wait := a.nextRefresh()
		if wait == 0 {
			if a.refresh(ctx) != nil {
				wait = refreshRetry
			} else {
				continue
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-a.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// nextRefresh - через сколько продлевать: 0 - сейчас; без входа - через сутки, пока не
// разбудит вход.
func (a *Account) nextRefresh() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess == nil {
		return 24 * time.Hour
	}
	if a.failures > 0 {
		return refreshRetry
	}
	d := a.expires.Add(-refreshBefore).Sub(a.now())
	if a.token == "" || d <= 0 {
		return 0
	}
	return d
}

// Devices - машины аккаунта из последнего списка.
func (a *Account) Devices() []DeviceInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]DeviceInfo(nil), a.devices...)
}

// RefreshDevices загружает список машин аккаунта.
func (a *Account) RefreshDevices(ctx context.Context) error {
	token, err := a.Token(ctx)
	if err != nil {
		return err
	}
	_, api, _ := a.urls()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/devices", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("source", appSource)
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		metrics.SourceRequest(Source, "error")
		return fmt.Errorf("список машин: %w", err)
	}
	defer resp.Body.Close()
	metrics.SourceRequest(Source, strconv.Itoa(resp.StatusCode))
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGigyaBody+1))
	if err != nil {
		return fmt.Errorf("список машин: %w", err)
	}
	if resp.StatusCode != http.StatusOK || len(body) > maxGigyaBody {
		return fmt.Errorf("список машин: HTTP %d", resp.StatusCode)
	}
	var data struct {
		OwnedByMe  []DeviceInfo `json:"ownedByMe"`
		SharedToMe []DeviceInfo `json:"sharedToMe"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return errors.New("список машин: ответ не в ожидаемом виде")
	}
	for i := range data.SharedToMe {
		data.SharedToMe[i].Shared = true
	}
	list := append(data.OwnedByMe, data.SharedToMe...)
	if len(list) > maxDevices {
		list = list[:maxDevices]
	}
	a.mu.Lock()
	a.devices = list
	a.mu.Unlock()
	return nil
}

// machineName - имя машины в AWS IoT по серийному номеру из списка машин.
func (a *Account) machineName(serial string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, d := range a.devices {
		if d.Serial == serial {
			return d.MachineName
		}
	}
	return ""
}

func (a *Account) attach(ap *Appliance) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appliances[ap] = true
}

func (a *Account) detach(ap *Appliance) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.appliances, ap)
}

func (a *Account) list() []*Appliance {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Appliance, 0, len(a.appliances))
	for ap := range a.appliances {
		out = append(out, ap)
	}
	return out
}

func (a *Account) notifyAppliances(event string) {
	for _, ap := range a.list() {
		ap.event(event)
	}
}

// wakeAppliances будит машины после входа: им больше не нужно ждать паузы переподключения.
func (a *Account) wakeAppliances() {
	for _, ap := range a.list() {
		signal(ap.wake)
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// RecipeStat - рецепт кофемашины и сколько раз его сварили, из тени BrewingStatisticsTotals.
type RecipeStat struct {
	ID   int
	Brew int
}

// RecipeStats читает из облака номера рецептов машины с серийным номером serial и
// сколько раз каждый сварили, по убыванию числа варок.
func (a *Account) RecipeStats(ctx context.Context, serial string) ([]RecipeStat, error) {
	thing := a.machineName(serial)
	if thing == "" {
		if err := a.RefreshDevices(ctx); err != nil {
			return nil, err
		}
		if thing = a.machineName(serial); thing == "" {
			return nil, errors.New("машины с этим серийным номером нет в аккаунте")
		}
	}
	r, err := a.readShadow(ctx, thing, "BrewingStatisticsTotals")
	if err != nil {
		return nil, err
	}
	list, _ := r["Recipes"].([]any)
	var out []RecipeStat
	for _, item := range list {
		m, _ := item.(map[string]any)
		id, ok1 := m["ID"].(float64)
		brew, ok2 := m["Brew"].(float64)
		if ok1 && ok2 && id >= 1 && id <= maxRecipe && len(out) < maxRecipe {
			out = append(out, RecipeStat{ID: int(id), Brew: int(brew)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Brew > out[j].Brew })
	return out, nil
}
