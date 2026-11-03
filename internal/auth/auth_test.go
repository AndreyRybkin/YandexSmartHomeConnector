package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/metrics"
	"hcgateway/internal/statedb"
)

func TestMain(m *testing.M) {
	// Настоящие 19 МиБ на проверку замедлили бы тесты с -race в десятки раз.
	params = argonParams{memory: 64, time: 1, threads: 1}
	os.Exit(m.Run())
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ok2(_ User, ok bool) bool { return ok }

func writeUser(t *testing.T, path, name, password string) User {
	t.Helper()
	u, err := NewUser(name, password)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveUser(path, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPasswordRules(t *testing.T) {
	p, err := GeneratePassword()
	if err != nil || len(p) != generatedLen || strings.Trim(p, passwordAlphabet) != "" {
		t.Fatalf("созданный пароль %q: %v", p, err)
	}
	cases := map[string]bool{
		strings.Repeat("a", 11):  false,
		strings.Repeat("a", 12):  true,
		strings.Repeat("я", 12):  true,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
	}
	for pw, ok := range cases {
		if err := CheckPassword(pw); (err == nil) != ok {
			t.Errorf("пароль из %d байт: %v", len(pw), err)
		}
	}
	for name, ok := range map[string]bool{"admin": true, "a_b-1": true, "": false, "админ": false, strings.Repeat("a", 33): false} {
		if ValidName(name) != ok {
			t.Errorf("имя %q", name)
		}
	}
	if _, err := NewUser("admin", "short"); err == nil {
		t.Fatal("короткий пароль принят")
	}
}

func TestUsersFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), UserFile)
	users := NewUsers(path, discard())
	if _, ok := users.Current(); ok || ok2(users.check("admin", "password-0001")) {
		t.Fatal("пользователя ещё нет")
	}
	first := writeUser(t, path, "admin", "password-0001")
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("права файла %v", info.Mode().Perm())
	}
	if u, ok := users.Current(); !ok || u != first {
		t.Fatalf("пользователь %+v", u)
	}
	if !ok2(users.check("admin", "password-0001")) || ok2(users.check("admin", "password-0002")) || ok2(users.check("root", "password-0001")) {
		t.Fatal("проверка логина и пароля")
	}
	// Новый файл действует без перезапуска, старый пароль - нет.
	second := writeUser(t, path, "admin", "password-0002")
	if u, _ := users.Current(); u.Hash != second.Hash || ok2(users.check("admin", "password-0001")) || !ok2(users.check("admin", "password-0002")) {
		t.Fatal("новый файл не перечитан")
	}
	// Хэш с чужими параметрами не принимается: проверка заняла бы неизвестно сколько памяти.
	bad := second
	bad.Hash = strings.Replace(bad.Hash, params.String(), "m=4194304,t=1,p=1", 1)
	if err := SaveUser(path, bad); err != nil {
		t.Fatal(err)
	}
	if _, ok := users.Current(); ok {
		t.Fatal("принят хэш с чужими параметрами")
	}
	os.Remove(path)
	if _, ok := users.Current(); ok {
		t.Fatal("удалённый файл")
	}
}

func newBans(now *time.Time) *Bans {
	b := NewBans()
	b.now = func() time.Time { return *now }
	return b
}

func TestBans(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	b := newBans(&now)
	a := netip.MustParseAddr("203.0.113.7")
	for i := 1; i < MaxFailures; i++ {
		if _, banned := b.Fail(a, WhereWeb); banned {
			t.Fatalf("бан после %d попыток", i)
		}
		now = now.Add(time.Minute)
	}
	if p, banned := b.Fail(a, WhereWeb); !banned || p != netip.MustParsePrefix("203.0.113.7/32") || !b.Banned(a) {
		t.Fatalf("нет бана после %d попыток", MaxFailures)
	}
	if list := b.List(); len(list) != 1 || !list[0].Until.Equal(now.Add(BanTime)) {
		t.Fatalf("список банов %v", list)
	}
	// Бан длится час.
	now = now.Add(BanTime)
	if b.Banned(a) {
		t.Fatal("бан после срока")
	}

	// Попытки старше часа не считаются, успешный вход обнуляет счётчик.
	c := netip.MustParseAddr("198.51.100.1")
	for range MaxFailures - 1 {
		b.Fail(c, WhereWeb)
	}
	now = now.Add(FailWindow)
	if _, banned := b.Fail(c, WhereWeb); banned {
		t.Fatal("учтены попытки старше часа")
	}
	b.Fail(c, WhereWeb)
	b.Reset(c)
	b.Fail(c, WhereWeb)
	if _, banned := b.Fail(c, WhereWeb); banned {
		t.Fatal("успешный вход не обнулил счётчик")
	}

	// IPv6 банится сетью /64, IPv4 в IPv6 - как IPv4.
	for range MaxFailures {
		b.Fail(netip.MustParseAddr("2001:db8:1:2::10"), WhereWeb)
	}
	if !b.Banned(netip.MustParseAddr("2001:db8:1:2:ffff::1")) || b.Banned(netip.MustParseAddr("2001:db8:1:3::10")) {
		t.Fatal("бан IPv6 не по /64")
	}
	if !b.Lift(netip.MustParsePrefix("2001:db8:1:2::/64")) || b.Banned(netip.MustParseAddr("2001:db8:1:2::10")) {
		t.Fatal("бан не снят")
	}
	if b.Lift(netip.MustParsePrefix("2001:db8:1:2::/64")) {
		t.Fatal("снят несуществующий бан")
	}
	if key(netip.MustParseAddr("::ffff:203.0.113.7")) != key(a) {
		t.Fatal("IPv4 в IPv6")
	}
}

func TestBansOverflow(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	b := newBans(&now)
	first := netip.AddrFrom4([4]byte{10, 0, 0, 0})
	for range MaxFailures {
		b.Fail(first, WhereWeb)
	}
	for i := 1; i < maxEntries+10; i++ {
		now = now.Add(time.Millisecond)
		b.Fail(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), WhereWeb)
	}
	if len(b.entries) > maxEntries {
		t.Fatalf("записей %d", len(b.entries))
	}
	if b.Banned(first) {
		t.Fatal("самый старый бан не удалён при переполнении")
	}
}

func TestClientAddr(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	req := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	cases := []struct {
		r    *http.Request
		want string
	}{
		{req("203.0.113.7:5000", "1.2.3.4"), "203.0.113.7"},
		{req("172.18.0.2:5000"), "172.18.0.2"},
		{req("172.18.0.2:5000", "1.2.3.4, 198.51.100.9"), "198.51.100.9"},
		{req("172.18.0.2:5000", "1.2.3.4", "198.51.100.9"), "198.51.100.9"},
		{req("172.18.0.2:5000", "мусор"), "172.18.0.2"},
		{req("[::ffff:172.18.0.2]:5000", "2001:db8::1"), "2001:db8::1"},
	}
	for _, c := range cases {
		if got := ClientAddr(c.r, trusted); got != netip.MustParseAddr(c.want) {
			t.Errorf("%s %v: %s, ожидался %s", c.r.RemoteAddr, c.r.Header.Values("X-Forwarded-For"), got, c.want)
		}
	}
}

func TestLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), UserFile)
	writeUser(t, path, "owner", "password-0001")
	a := New(NewUsers(path, discard()), nil, discard())
	req := func(remote string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = remote
		return r
	}
	if res, _ := a.Login(req("203.0.113.7:1"), WhereWeb, "owner", "password-0001"); res != OK {
		t.Fatalf("верный пароль: %v", res)
	}
	// Счётчик общий для интерфейса и привязки.
	webWrong := metrics.Value("hcgateway_web_logins_total", "result", metrics.LoginWrong)
	linkingBanned := metrics.Value("hcgateway_linking_logins_total", "result", metrics.LoginBanned)
	bans := metrics.Value("hcgateway_auth_bans_total")
	for i := range MaxFailures {
		where := WhereWeb
		if i%2 == 1 {
			where = WhereLinking
		}
		if res, _ := a.Login(req("203.0.113.7:1"), where, "owner", "wrong-password"); res != Wrong {
			t.Fatalf("попытка %d: %v", i+1, res)
		}
	}
	if res, _ := a.Login(req("203.0.113.7:2"), WhereLinking, "owner", "password-0001"); res != Banned {
		t.Fatalf("после бана: %v", res)
	}
	if metrics.Value("hcgateway_web_logins_total", "result", metrics.LoginWrong) != webWrong+MaxFailures/2 ||
		metrics.Value("hcgateway_linking_logins_total", "result", metrics.LoginBanned) != linkingBanned+1 ||
		metrics.Value("hcgateway_auth_bans_total") != bans+1 || a.Bans.Active() != 1 {
		t.Fatal("входы и баны не посчитаны")
	}
	if res, _ := a.Login(req("198.51.100.1:1"), WhereWeb, "owner", "password-0001"); res != OK {
		t.Fatalf("другой адрес: %v", res)
	}

	// Обе проверки заняты - попытка ждёт не дольше wait.
	a.wait = 10 * time.Millisecond
	a.checks <- struct{}{}
	a.checks <- struct{}{}
	r := req("198.51.100.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if res, _ := a.Login(r.WithContext(ctx), WhereWeb, "owner", "password-0001"); res != Busy {
		t.Fatalf("проверки заняты: %v", res)
	}
}

// Действующий бан переживает перезапуск, снятый и истёкший - нет, а счётчик попыток начинается заново.
func TestBansRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), statedb.FileName)
	banned := netip.MustParseAddr("203.0.113.7")
	lifted := netip.MustParseAddr("203.0.113.8")
	counting := netip.MustParseAddr("203.0.113.9")
	db := statedb.Open(path, discard())
	b := NewBans()
	b.Attach(db)
	for range MaxFailures {
		b.Fail(banned, WhereLinking)
		b.Fail(lifted, WhereWeb)
	}
	b.Fail(counting, WhereWeb)
	if !b.Lift(key(lifted)) {
		t.Fatal("бан не снят")
	}
	db.Close()

	db = statedb.Open(path, discard())
	defer db.Close()
	r := NewBans()
	r.Attach(db)
	if !r.Banned(banned) || r.Active() != 1 {
		t.Fatalf("после перезапуска: в бане %v, всего %d", r.Banned(banned), r.Active())
	}
	if r.Banned(lifted) {
		t.Fatal("снятый бан вернулся")
	}
	for range MaxFailures - 1 {
		if _, ban := r.Fail(counting, WhereWeb); ban {
			t.Fatal("счётчик попыток пережил перезапуск")
		}
	}
	rows, err := r.History(0, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("журнал: %v %v", rows, err)
	}
	if rows[0].Prefix != key(lifted) || rows[0].EndedBy != statedb.EndedWeb || rows[0].Where != WhereWeb ||
		rows[1].Prefix != key(banned) || rows[1].EndedBy != "" || rows[1].Where != WhereLinking {
		t.Fatalf("строки: %+v", rows)
	}
}

// Команда unban действует на следующую попытку входа без перезапуска, а новый бан того же адреса
// после команды остаётся.
func TestBansUnbanCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), statedb.FileName)
	a := netip.MustParseAddr("203.0.113.7")
	db := statedb.Open(path, discard())
	defer db.Close()
	b := NewBans()
	b.Attach(db)
	now := time.Now()
	b.now = func() time.Time { return now }
	for range MaxFailures {
		b.Fail(a, WhereWeb)
	}
	db.Flush()
	if n, err := statedb.Unban(path, key(a), false, now); err != nil || n != 1 {
		t.Fatalf("команда: %d %v", n, err)
	}
	if b.Banned(a) {
		t.Fatal("бан остался после команды")
	}
	now = now.Add(time.Minute)
	for range MaxFailures {
		b.Fail(a, WhereWeb)
	}
	db.Flush()
	now = now.Add(time.Minute)
	if !b.Banned(a) {
		t.Fatal("старая команда сняла новый бан")
	}
}

func TestBanTarget(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":       "203.0.113.7/32",
		"2001:db8:1:2::10":  "2001:db8:1:2::/64",
		"2001:db8:1:2::/64": "2001:db8:1:2::/64",
		"203.0.113.7/32":    "203.0.113.7/32",
	} {
		if p, err := BanTarget(in); err != nil || p.String() != want {
			t.Fatalf("%s: %v %v", in, p, err)
		}
	}
	if _, err := BanTarget("не адрес"); err == nil {
		t.Fatal("мусор принят")
	}
}
