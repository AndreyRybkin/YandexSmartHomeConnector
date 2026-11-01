package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"hcgateway/internal/metrics"
)

// Result - итог попытки входа.
type Result int

const (
	// OK - вход выполнен.
	OK Result = iota
	// Wrong - неверный логин или пароль.
	Wrong
	// Banned - адрес в бане, пароль не проверялся.
	Banned
	// Busy - проверки паролей заняты дольше checkWait.
	Busy
)

// Откуда вход, для журнала.
const (
	WhereWeb     = "веб-интерфейс"
	WhereLinking = "привязка"
)

const (
	// maxChecks - сколько паролей шлюз проверяет одновременно: каждая проверка занимает 19 МБ памяти.
	maxChecks = 2
	// checkWait - сколько попытка входа ждёт свободной проверки.
	checkWait = 10 * time.Second
	// maxNameBytes и maxPasswordBytes - длиннее имени и пароля не бывает, такие шлюз не проверяет.
	maxNameBytes     = 32
	maxPasswordBytes = 4 * MaxPasswordLen
)

// Auth проверяет логин и пароль владельца и банит адреса после неудачных попыток.
// Счётчик попыток общий для веб-интерфейса и страницы привязки.
type Auth struct {
	Users   *Users
	Bans    *Bans
	trusted []netip.Prefix
	log     *slog.Logger
	checks  chan struct{}
	wait    time.Duration
}

// New создаёт проверку входа. trusted - прокси, чьему X-Forwarded-For шлюз верит.
func New(users *Users, trusted []netip.Prefix, log *slog.Logger) *Auth {
	return &Auth{Users: users, Bans: NewBans(), trusted: trusted, log: log,
		checks: make(chan struct{}, maxChecks), wait: checkWait}
}

// Addr - адрес клиента запроса.
func (a *Auth) Addr(r *http.Request) netip.Addr {
	return ClientAddr(r, a.trusted)
}

// resultLabels - итоги входа в метриках.
var resultLabels = map[Result]string{OK: metrics.LoginOK, Wrong: metrics.LoginWrong, Banned: metrics.LoginBanned, Busy: metrics.LoginBusy}

// Login проверяет логин и пароль из запроса r. where - откуда вход, для журнала и метрик.
// При OK возвращает пользователя, чей пароль проверен: к нему привязывается сессия.
func (a *Auth) Login(r *http.Request, where, name, password string) (Result, User) {
	res, u := a.login(r, where, name, password)
	metrics.Login(where == WhereWeb, resultLabels[res])
	return res, u
}

func (a *Auth) login(r *http.Request, where, name, password string) (Result, User) {
	addr := a.Addr(r)
	if a.Bans.Banned(addr) {
		a.log.Warn("вход с забаненного адреса", "where", where, "addr", addr)
		return Banned, User{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.wait)
	defer cancel()
	select {
	case a.checks <- struct{}{}:
	case <-ctx.Done():
		a.log.Warn("проверки паролей заняты", "where", where, "addr", addr)
		return Busy, User{}
	}
	defer func() { <-a.checks }()
	// Пока попытка ждала проверки, адрес могли забанить другие попытки с него.
	if a.Bans.Banned(addr) {
		a.log.Warn("вход с забаненного адреса", "where", where, "addr", addr)
		return Banned, User{}
	}
	if u, ok := a.Users.check(name, password); ok {
		a.Bans.Reset(addr)
		a.log.Info("вход", "where", where, "addr", addr)
		return OK, u
	}
	a.log.Warn("неудачный вход", "where", where, "addr", addr)
	if p, banned := a.Bans.Fail(addr); banned {
		metrics.Ban()
		a.log.Warn("адрес забанен", "addr", p, "for", BanTime)
	}
	return Wrong, User{}
}

// ClientAddr - адрес клиента. Запрос от доверенного прокси несёт адрес клиента последним
// в X-Forwarded-For: Traefik дописывает его в конец заголовка. Остальным заголовок
// не верится: иначе клиент подставил бы чужой адрес и обошёл бан.
func ClientAddr(r *http.Request, trusted []netip.Prefix) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	remote := ap.Addr().Unmap()
	if !inPrefixes(trusted, remote) {
		return remote
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return remote
	}
	last := values[len(values)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}
	a, err := netip.ParseAddr(strings.TrimSpace(last))
	if err != nil {
		return remote
	}
	return a.Unmap().WithZone("")
}

func inPrefixes(list []netip.Prefix, a netip.Addr) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
