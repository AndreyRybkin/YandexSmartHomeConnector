package hccloud

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/hcapi"
)

const (
	// flowLoginTTL - сколько шлюз ждёт адрес hcauth:// после выдачи ссылки на вход.
	flowLoginTTL = 10 * time.Minute
	// flowTimeout - предел всей загрузки ключей после входа.
	flowTimeout = 5 * time.Minute
)

var (
	// ErrBusy - загрузка ключей уже идёт.
	ErrBusy = errors.New("загрузка ключей уже идёт")
	// ErrRegion - такого региона нет.
	ErrRegion = errors.New("неизвестный регион")
	// ErrNoLogin - ссылка на вход не выдана или её срок истёк.
	ErrNoLogin = errors.New("нет начатого входа")
	// ErrNoTokens - сохранённого токена нет.
	ErrNoTokens = errors.New("нет сохранённого токена")
	// ErrForeignToken - токен выдан другому приложению: шлюз не сможет его продлить.
	ErrForeignToken = errors.New("токен выдан другому приложению")
	// ErrTokenRegion - регион не выбран, а токен его не называет или называет незнакомый.
	ErrTokenRegion = errors.New("регион токена не определён")
	// ErrBadRedirect - адрес не от этого входа или вход отклонён.
	ErrBadRedirect = errors.New("адрес hcauth:// не подошёл")
)

// RegionMismatchError - токен выдан для одного региона, а выбран другой.
type RegionMismatchError struct {
	// Token и Selected - имена регионов токена и выбранного.
	Token, Selected string
}

func (e *RegionMismatchError) Error() string {
	return "токен выдан для региона " + e.Token + ", а выбран " + e.Selected
}

// regionClaimRe - значение поля region, которое можно показать на странице.
var regionClaimRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)

// FlowStatus - состояние загрузки ключей для страницы интерфейса. Секретов в нём нет,
// кроме одноразовой ссылки на вход.
type FlowStatus struct {
	// Waiting - ссылка выдана, шлюз ждёт адрес hcauth://; Running - идёт загрузка.
	Waiting, Running bool
	// Done - загрузка закончена, удачно или нет.
	Done bool
	// Failed - загрузка не дошла до приборов; Empty - в аккаунте нет приборов.
	Failed, Empty bool
	// Expired - шлюз не дождался адреса hcauth://.
	Expired bool
	// NoRefresh - в данных токена нет refresh token: токен использован один раз и не сохранён.
	NoRefresh bool
	// UnknownRegion - незнакомый регион из токена: шлюз использовал выбранный.
	UnknownRegion string
	Region        string
	Link          string
	Expires       time.Time
	Results       []Saved
}

// Flow - вход в аккаунт Home Connect и загрузка ключей со страницы интерфейса: один вход за раз.
// Токен с refresh token после ответа облака попадает в клиент облака: тот хранит и продлевает его,
// и по нему же работает источник homeconnect-cloud.
type Flow struct {
	dataDir string
	regions map[string]Region
	api     *hcapi.Client
	// loggedIn - токен сохранён: облаку пора подключиться.
	loggedIn func()
	log      *slog.Logger
	now      func() time.Time

	// wg - идущая загрузка.
	wg sync.WaitGroup

	mu     sync.Mutex
	login  *Login
	region Region
	st     FlowStatus
	// cancel обрывает загрузку; closed - шлюз останавливается, новых загрузок нет.
	cancel context.CancelFunc
	closed bool
}

// NewFlow создаёт загрузку ключей в data_dir для регионов regions.
func NewFlow(dataDir string, api *hcapi.Client, loggedIn func(), log *slog.Logger, regions ...Region) *Flow {
	m := make(map[string]Region, len(regions))
	for _, r := range regions {
		m[r.Name] = r
	}
	return &Flow{dataDir: dataDir, regions: m, api: api, loggedIn: loggedIn, log: log, now: time.Now}
}

// Start выдаёт новую ссылку на вход: прежняя перестаёт действовать.
func (f *Flow) Start(region string) error {
	r, ok := f.regions[region]
	if !ok {
		return ErrRegion
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Running || f.closed {
		return ErrBusy
	}
	l, err := NewLogin(r, ScopeFull)
	if err != nil {
		return err
	}
	link, err := l.URL()
	if err != nil {
		return err
	}
	f.login, f.region = l, r
	f.st = FlowStatus{Waiting: true, Region: r.Name, Link: link, Expires: f.now().Add(flowLoginTTL)}
	return nil
}

// Finish принимает адрес hcauth:// и начинает загрузку в фоне. Адрес не от этого входа
// не обрывает вход: его можно вставить ещё раз.
func (f *Flow) Finish(redirect string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Running || f.closed {
		return ErrBusy
	}
	f.expire()
	if f.login == nil {
		return ErrNoLogin
	}
	if _, err := f.login.parseRedirect(redirect); err != nil {
		f.log.Warn("загрузка ключей: адрес hcauth:// не подошёл", "err", err)
		return ErrBadRedirect
	}
	l, r := f.login, f.region
	f.login = nil
	f.st = FlowStatus{Running: true, Region: r.Name}
	return f.launch(r, func(ctx context.Context) (*Grant, error) { return l.Exchange(ctx, redirect) })
}

// FinishToken начинает загрузку в фоне по готовому токену из данных data, без входа по ссылке.
// Пустой region - регион берётся из токена. Токен другого приложения или другого региона отвергается
// до запросов к облаку: он туда не уходит.
func (f *Flow) FinishToken(region, data string) error {
	selected, ok := f.regions[region]
	if region != "" && !ok {
		return ErrRegion
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Running || f.closed {
		return ErrBusy
	}
	grant, err := ParseToken(data, f.now())
	if err != nil {
		return err
	}
	if grant.ClientID != "" && grant.ClientID != appClientID {
		return ErrForeignToken
	}
	claimed, known := f.regionByClaim(grant.Region)
	switch {
	case region == "" && !known:
		return ErrTokenRegion
	case region == "":
		selected = claimed
	case known && claimed.Name != region:
		return &RegionMismatchError{Token: claimed.Name, Selected: region}
	}
	st := FlowStatus{Running: true, Region: selected.Name}
	if grant.Region != "" && !known {
		// Значения поля известны не все: незнакомое не повод отказать, но выбор надо подтвердить.
		f.log.Warn("загрузка ключей: незнакомый регион в токене, используется выбранный", "region", selected.Name)
		st.UnknownRegion = "?"
		if regionClaimRe.MatchString(grant.Region) {
			st.UnknownRegion = grant.Region
		}
	}
	f.login = nil
	f.st = st
	return f.launch(selected, func(context.Context) (*Grant, error) { return grant, nil })
}

// regionByClaim находит регион по полю region токена, без учёта регистра.
func (f *Flow) regionByClaim(claim string) (Region, bool) {
	for name, r := range f.regions {
		if strings.EqualFold(name, claim) {
			return r, true
		}
	}
	return Region{}, false
}

// Update загружает ключи заново по сохранённому токену, без нового входа.
func (f *Flow) Update() error {
	t, ok := f.api.AppTokens()
	if !ok {
		return ErrNoTokens
	}
	r, ok := f.regions[t.Region]
	if !ok {
		return ErrRegion
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Running || f.closed {
		return ErrBusy
	}
	f.login = nil
	f.st = FlowStatus{Running: true, Region: r.Name}
	return f.launch(r, f.saved)
}

// saved - сохранённый токен: уже в клиенте облака, сохранять его заново не нужно.
func (f *Flow) saved(ctx context.Context) (*Grant, error) {
	token, err := f.api.AppAccess(ctx)
	if err != nil {
		return nil, err
	}
	subject, err := tokenSubject(token)
	if err != nil {
		return nil, err
	}
	return &Grant{Access: token, Subject: subject, kept: true}, nil
}

// launch начинает загрузку в фоне. Вызывается под f.mu.
func (f *Flow) launch(r Region, auth func(ctx context.Context) (*Grant, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), flowTimeout)
	f.cancel = cancel
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer cancel()
		f.run(ctx, r, auth)
	}()
	return nil
}

// Run ждёт отмены ctx, затем обрывает идущую загрузку и ждёт её конца. Новые загрузки после этого
// не начинаются.
func (f *Flow) Run(ctx context.Context) {
	<-ctx.Done()
	f.mu.Lock()
	f.closed = true
	if f.cancel != nil {
		f.cancel()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

func (f *Flow) run(ctx context.Context, r Region, auth func(ctx context.Context) (*Grant, error)) {
	err := f.download(ctx, r, auth)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Running, f.st.Done = false, true
	if err != nil {
		f.st.Failed = true
		f.st.Empty = errors.Is(err, ErrNoAppliances)
		f.log.Warn("загрузка ключей не удалась", "region", r.Name, "err", err)
	}
}

func (f *Flow) download(ctx context.Context, r Region, auth func(ctx context.Context) (*Grant, error)) error {
	g, err := auth(ctx)
	if err != nil {
		return err
	}
	account, paired, err := Discover(ctx, r, g.Access, g.Subject)
	if errors.Is(err, ErrUnauthorized) && g.kept {
		// Сохранённый токен отозван раньше срока: продлить и повторить один раз.
		f.log.Info("загрузка ключей: облако не приняло токен, шлюз его продлевает")
		if err = f.api.RefreshNow(ctx); err == nil {
			if g, err = f.saved(ctx); err == nil {
				account, paired, err = Discover(ctx, r, g.Access, g.Subject)
			}
		}
	}
	if err != nil {
		return err
	}
	// Токен сохраняется только после ответа облака: неверный токен или регион не заменит рабочий вход.
	if err := f.keep(r, g); err != nil {
		return err
	}
	_, err = Download(ctx, f.dataDir, account, paired, func(s Saved) {
		if s.Err != nil {
			f.log.Warn("загрузка ключей: прибор не загружен", "haId", s.Appliance.HaID, "err", s.Err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.st.Results = append(f.st.Results, s)
	})
	return err
}

// keep передаёт новый токен клиенту облака. Токен без refresh token нельзя продлить: он остаётся
// только на эту загрузку.
func (f *Flow) keep(r Region, g *Grant) error {
	if g.kept {
		return nil
	}
	if g.Refresh == "" {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.st.NoRefresh = true
		return nil
	}
	err := f.api.UseTokens(hcapi.Tokens{Access: g.Access, Refresh: g.Refresh, Scope: g.Scope, Expires: g.Expires,
		ClientID: appClientID, BaseURL: r.Base, Region: r.Name, Refreshed: f.now()})
	if err != nil {
		return err
	}
	f.log.Info("токен приложения Home Connect сохранён", "region", r.Name)
	if f.loggedIn != nil {
		f.loggedIn()
	}
	return nil
}

// expire забывает вход, срок которого истёк. Вызывается под f.mu.
func (f *Flow) expire() {
	if f.login != nil && f.now().After(f.st.Expires) {
		f.login = nil
		f.st = FlowStatus{Expired: true, Region: f.region.Name}
	}
}

// Status - состояние для страницы.
func (f *Flow) Status() FlowStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire()
	st := f.st
	st.Results = append([]Saved(nil), f.st.Results...)
	return st
}
