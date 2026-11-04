package delonghi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/delonghi/dlfake"
)

// syncBuffer - журнал, в который пишут из разных горутин.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newAccount(t *testing.T, cloud *dlfake.Cloud) (*Account, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	a, err := New(filepath.Join(t.TempDir(), "delonghi", "session.json"), slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.Configure(cloud.URLs())
	return a, logs
}

func loggedIn(t *testing.T, cloud *dlfake.Cloud) (*Account, *syncBuffer) {
	t.Helper()
	a, logs := newAccount(t, cloud)
	if err := a.Login(context.Background(), dlfake.Email, dlfake.Password); err != nil {
		t.Fatal(err)
	}
	return a, logs
}

// Эталон - алгоритм _sign из comfort-hub/delonghi-comfort (Python 3.9).
func TestGigyaSign(t *testing.T) {
	params := url.Values{"apiKey": {"4_mXSplGaqrFT0H88TAjqJuA"}, "oauth_token": {"st2.s.AcbH~x y+z/="},
		"expiration": {"3600"}, "timestamp": {"1761900000"}, "nonce": {"abc*def!'()"}}
	got, err := gigyaSign("POST", "https://accounts.eu1.gigya.com/accounts.getJWT", params, "c2VjcmV0LWtleS0xMjM=")
	if err != nil || got != "DcgEba6KTY3xnpu3OFdtgXmU+co=" {
		t.Fatalf("подпись %s, %v", got, err)
	}
}

func TestLogin(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	a, logs := newAccount(t, cloud)
	ctx := context.Background()
	if err := a.Login(ctx, dlfake.Email, "wrong-password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("неверный пароль: %v", err)
	}
	if a.Status().LoggedIn {
		t.Fatal("после неверного пароля вход выполнен")
	}
	if err := a.Login(ctx, dlfake.Email, dlfake.Password); err != nil {
		t.Fatal(err)
	}
	st := a.Status()
	if !st.LoggedIn || st.Pool != dlfake.Pool || time.Until(st.Expires) < 6*24*time.Hour {
		t.Fatalf("состояние входа %+v", st)
	}
	info, err := os.Stat(a.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("файл сессии: %v, права %v", err, info.Mode())
	}
	data, _ := os.ReadFile(a.path)
	if bytes.Contains(data, []byte(dlfake.Password)) {
		t.Fatal("пароль сохранён в файл сессии")
	}
	if strings.Contains(logs.String(), dlfake.Password) {
		t.Fatal("пароль в журнале")
	}

	// Сессия переживает перезапуск: JWT выпускается из неё без пароля.
	b, err := New(a.path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	b.Configure(cloud.URLs())
	token, err := b.Token(ctx)
	if err != nil || !cloud.Valid(token) {
		t.Fatalf("токен после перезапуска: %v", err)
	}
	if err := b.Logout(); err != nil || b.Status().LoggedIn {
		t.Fatalf("выход: %v", err)
	}
	if _, err := os.Stat(a.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("файл сессии не удалён")
	}
}

func TestDevices(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	a, _ := loggedIn(t, cloud)
	if err := a.RefreshDevices(context.Background()); err != nil {
		t.Fatal(err)
	}
	list := a.Devices()
	if len(list) != 1 || list[0].Serial != dlfake.Serial || list[0].MachineName != dlfake.Thing || list[0].Model != "ECAM63070" {
		t.Fatalf("машины %+v", list)
	}
}

// Продление: за refreshBefore до конца срока; после failuresAlarm неудач подряд машины
// один раз сообщают, что нужен вход.
func TestRefresh(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	a, _ := loggedIn(t, cloud)
	now := time.Now()
	a.now = func() time.Time { return now }
	if d := a.nextRefresh(); d < 4*24*time.Hour || d > 5*24*time.Hour {
		t.Fatalf("продление через %v, ожидалось около 5 дней", d)
	}
	now = now.Add(5*24*time.Hour + time.Minute)
	if a.nextRefresh() != 0 {
		t.Fatal("за 2 дня до конца срока продление не нужно")
	}

	ap, err := a.Appliance(Config{ID: "coffee", Host: "127.0.0.1:1", Serial: dlfake.Serial})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []string
	ap.notify = func(e []string) {
		mu.Lock()
		events = append(events, e...)
		mu.Unlock()
	}
	a.attach(ap)
	cloud.FailJWT = true
	for range failuresAlarm + 2 {
		if a.refresh(context.Background()) == nil {
			t.Fatal("продление с отказом Gigya удалось")
		}
	}
	if a.nextRefresh() != refreshRetry {
		t.Fatalf("после неудачи повтор не через %v", refreshRetry)
	}
	mu.Lock()
	n := slices.Index(events, EventLoginRequired)
	count := len(slices.DeleteFunc(slices.Clone(events), func(e string) bool { return e != EventLoginRequired }))
	mu.Unlock()
	if n < 0 || count != 1 || !a.Status().LoginRequired {
		t.Fatalf("события %v, состояние %+v", events, a.Status())
	}
	cloud.FailJWT = false
	if err := a.refresh(context.Background()); err != nil || a.Status().LoginRequired || a.Status().Failures != 0 {
		t.Fatalf("продление после восстановления: %v %+v", err, a.Status())
	}
}

// harness - машина в Алисе с подделками и журналом изменений.
type harness struct {
	t       *testing.T
	cloud   *dlfake.Cloud
	machine *dlfake.Machine
	acct    *Account
	ap      *Appliance
	logs    *syncBuffer
	mu      sync.Mutex
	events  []string
	cancel  context.CancelFunc
	done    chan struct{}
}

func start(t *testing.T, pin string) *harness {
	t.Helper()
	h := &harness{t: t, cloud: dlfake.NewCloud(), done: make(chan struct{})}
	h.machine = dlfake.NewMachine(h.cloud)
	h.acct, h.logs = loggedIn(t, h.cloud)
	if pin == "machine" {
		pin = h.machine.Pin()
	}
	ap, err := h.acct.Appliance(Config{ID: "coffee", Host: h.machine.Host(), Serial: dlfake.Serial, Pin: pin, ProfileID: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.ap = ap
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		ap.Run(ctx, func(e []string) {
			h.mu.Lock()
			h.events = append(h.events, e...)
			h.mu.Unlock()
		})
		close(h.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
		h.machine.Close()
		h.cloud.Close()
	})
	return h
}

func (h *harness) wait(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) has(event string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.events, event)
}

func TestAppliance(t *testing.T) {
	h := start(t, "machine")
	ctx := context.Background()
	h.wait("вход в машину", func() bool { return h.machine.Connected() == 1 })
	h.machine.Status(map[string]any{"CurrMode": 7.0, "Status": "Idle", "BeverageDispensing": 0.0,
		"alarms": map[string]any{"TankLevel": false, "NoCoffee": true}, "peripherals": map[string]any{"DripPresence": true}})
	h.wait("состояние", func() bool { return h.ap.State().Online })
	st := h.ap.State()
	if st.Values[StatusMode] != 7.0 || st.Values[AlarmPrefix+"NoCoffee"] != appliance.EventPresent ||
		st.Values[AlarmPrefix+"TankLevel"] != appliance.EventOff || st.Values[PeripheralPrefix+"DripPresence"] != true {
		t.Fatalf("значения %v", st.Values)
	}

	if err := h.ap.Start(ctx, ProgramPrefix+"6", nil); err != nil {
		t.Fatal(err)
	}
	if h.ap.State().ActiveProgram != ProgramPrefix+"6" {
		t.Fatal("напиток не виден как работающая программа")
	}
	auths, cmds := h.machine.Snapshot()
	last := cmds[len(cmds)-1]
	if len(auths) != 1 || last["Message"] != "BeverageRequest" || last["RecipeId"] != 6.0 || last["ProfileId"] != 1.0 ||
		last["AppId"] != "MCL" || last["ConnectionId"] != 1.0 || len(last["RequestId"].(string)) != 5 {
		t.Fatalf("команда %v", last)
	}
	h.machine.Status(map[string]any{"CurrMode": 7.0, "Status": "Busy", "BeverageDispensing": 40.0})
	h.wait("занята", func() bool { return h.ap.State().Values[StatusState] == "Busy" })
	if err := h.ap.Start(ctx, ProgramPrefix+"1", nil); !errors.Is(err, appliance.ErrBusy) {
		t.Fatalf("варка занятой машиной: %v", err)
	}
	h.machine.Status(map[string]any{"CurrMode": 11.0, "Status": "Busy", "BeverageDispensing": 100.0})
	h.wait("напиток готов", func() bool { return h.has(EventBeverageFinished) })
	if h.ap.State().ActiveProgram != "" {
		t.Fatal("напиток всё ещё идёт")
	}

	if err := h.ap.Set(ctx, SettingPower, PowerOff); err != nil {
		t.Fatal(err)
	}
	_, cmds = h.machine.Snapshot()
	if c := cmds[len(cmds)-1]; c["Message"] != "MachineUI" || c["TurnOnOff"] != 1.0 {
		t.Fatalf("выключение %v", c)
	}
	// Промывка при выключении «наливает» в служебном режиме: это не напиток.
	h.mu.Lock()
	h.events = nil
	h.mu.Unlock()
	h.machine.Status(map[string]any{"CurrMode": 2.0, "Status": "Busy", "BeverageDispensing": 50.0})
	h.machine.Status(map[string]any{"CurrMode": 0.0, "Status": "Standby", "BeverageDispensing": 100.0})
	h.wait("выключена", func() bool { return h.ap.State().Values[StatusMode] == 0.0 })
	if h.has(EventBeverageFinished) {
		t.Fatal("промывка сочтена напитком")
	}
	if err := h.ap.Start(ctx, ProgramPrefix+"6", nil); !errors.Is(err, appliance.ErrWrongMode) {
		t.Fatalf("варка выключенной машиной: %v", err)
	}

	// Машина рвёт соединение при включении: шлюз входит заново.
	h.machine.Drop()
	h.wait("переподключение", func() bool {
		a, _ := h.machine.Snapshot()
		return len(a) == 2 && h.machine.Connected() == 1
	})
	if strings.Contains(h.logs.String(), dlfake.Password) {
		t.Fatal("пароль в журнале")
	}
	for _, a := range auths {
		if strings.Contains(h.logs.String(), a["AuthToken"].(string)) {
			t.Fatal("токен в журнале")
		}
	}
}

// Без подтверждённого отпечатка шлюз только снимает отпечаток, токен не уходит.
// С чужим отпечатком - тоже.
func TestPin(t *testing.T) {
	h := start(t, "")
	h.wait("отпечаток", func() bool { return h.ap.Observed() != "" })
	if h.ap.Observed() != h.machine.Pin() {
		t.Fatalf("отпечаток %s, у машины %s", h.ap.Observed(), h.machine.Pin())
	}
	if a, _ := h.machine.Snapshot(); len(a) != 0 {
		t.Fatal("без подтверждения токен ушёл машине")
	}

	g := start(t, strings.Repeat("AB", 32))
	time.Sleep(300 * time.Millisecond)
	if a, _ := g.machine.Snapshot(); len(a) != 0 || g.ap.State().Online {
		t.Fatal("с чужим отпечатком токен ушёл машине")
	}
	if _, err := g.acct.Appliance(Config{Pin: "12"}); err == nil {
		t.Fatal("короткий отпечаток принят")
	}
}

// Пока по домашней сети состояния нет, шлюз читает тень; состояние из домашней сети главнее.
func TestCloudState(t *testing.T) {
	h := start(t, "machine")
	h.wait("вход в машину", func() bool { return h.machine.Connected() == 1 })
	h.ap.readCloud(context.Background())
	st := h.ap.State()
	if h.cloud.Reads() != 1 || st.Values[StatusMode] != 7.0 || st.Values[AlarmPrefix+"TankLevel"] != appliance.EventPresent {
		t.Fatalf("тень не прочитана: чтений %d, значения %v", h.cloud.Reads(), st.Values)
	}
	h.machine.Status(map[string]any{"CurrMode": 7.0, "Status": "Busy"})
	h.wait("состояние из сети", func() bool { return h.ap.State().Values[StatusState] == "Busy" })
	h.ap.readCloud(context.Background())
	if h.cloud.Reads() != 1 || h.ap.State().Values[StatusState] != "Busy" {
		t.Fatal("тень прочитана, хотя состояние по сети известно")
	}
}

// После смены питания тень читается часто по одному соединению; тень старше сообщения по
// домашней сети состояние не меняет.
func TestFastCloud(t *testing.T) {
	fastPoll, fastWindow = 50*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() { fastPoll, fastWindow = 1500*time.Millisecond, 30*time.Second })
	h := start(t, "machine")
	h.wait("вход в машину", func() bool { return h.machine.Connected() == 1 })
	h.machine.Status(map[string]any{"CurrMode": 7.0, "Status": "Idle"})
	h.wait("состояние", func() bool { return h.ap.State().Online })
	// Тень говорит «выключена», но с временем на минуту раньше сообщения машины.
	h.cloud.SetShadow(map[string]any{"CurrMode": 0.0, "Status": "Standby"}, time.Now().Add(-time.Minute))
	if err := h.ap.Set(context.Background(), SettingPower, PowerOff); err != nil {
		t.Fatal(err)
	}
	h.wait("частый опрос", func() bool { return h.cloud.Reads() >= 5 })
	if h.ap.State().Values[StatusMode] != 7.0 {
		t.Fatal("старая тень перезаписала состояние из домашней сети")
	}
	if c := h.cloud.Connects(); c != 1 {
		t.Fatalf("соединений с брокером %d, нужно одно на окно", c)
	}
	// Новая тень применяется, окно заканчивается само.
	h.cloud.SetShadow(map[string]any{"CurrMode": 0.0, "Status": "Standby"}, time.Now().Add(2*time.Second))
	h.wait("тень применена", func() bool { return h.ap.State().Values[StatusMode] == 0.0 })
	time.Sleep(fastWindow + 300*time.Millisecond)
	n := h.cloud.Reads()
	time.Sleep(300 * time.Millisecond)
	if h.cloud.Reads() != n {
		t.Fatal("опрос не закончился после окна")
	}
}

// Перед командой токен машины не уходит в журнал, а неверный токен машина не принимает.
func TestRefusedAuth(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	m := dlfake.NewMachine(cloud)
	defer m.Close()
	pin, _ := parsePin(m.Pin())
	if _, err := dialLAN(context.Background(), m.Host(), pin, dlfake.Serial, "bad-token"); !errors.Is(err, errAuthRefused) {
		t.Fatalf("неверный токен: %v", err)
	}
}

func TestRecipeStats(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	a, _ := loggedIn(t, cloud)
	stats, err := a.RecipeStats(context.Background(), dlfake.Serial)
	if err != nil || len(stats) != 3 || stats[0] != (RecipeStat{6, 986}) || stats[1] != (RecipeStat{1, 85}) {
		t.Fatalf("рецепты %v, %v", stats, err)
	}
	if _, err := a.RecipeStats(context.Background(), "OTHER"); err == nil {
		t.Fatal("чужая машина")
	}
}

// Счётчик напитков - сумма варок всех рецептов; без входа в облако его нет.
func TestReadBrews(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	a, _ := loggedIn(t, cloud)
	ap, err := a.Appliance(Config{ID: "coffee", Host: "127.0.0.1:1", Serial: dlfake.Serial})
	if err != nil {
		t.Fatal(err)
	}
	ap.readBrews(context.Background())
	stats, err := a.RecipeStats(context.Background(), dlfake.Serial)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, s := range stats {
		total += s.Brew
	}
	if got := ap.State().Values[StatusBrews]; got != float64(total) {
		t.Fatalf("счётчик %v, нужно %d", got, total)
	}
}

// IP машины в домашней сети - из тени MachineStatus; не IPv4 - пусто.
func TestLanAddress(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	a, _ := loggedIn(t, cloud)
	cloud.SetShadow(map[string]any{"CurrMode": 7.0, "LanIpAddress": "192.168.1.70"}, time.Now())
	if ip, err := a.LanAddress(context.Background(), dlfake.Thing); err != nil || ip != "192.168.1.70" {
		t.Fatalf("адрес: %q %v", ip, err)
	}
	cloud.SetShadow(map[string]any{"CurrMode": 7.0, "LanIpAddress": "машина"}, time.Now())
	if ip, err := a.LanAddress(context.Background(), dlfake.Thing); err != nil || ip != "" {
		t.Fatalf("не адрес: %q %v", ip, err)
	}
}

// Причина отказа машины - по её тревогам; без тревог - занята.
func TestRefusal(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	a, _ := loggedIn(t, cloud)
	ap, err := a.Appliance(Config{ID: "coffee", Host: "127.0.0.1:1", Serial: dlfake.Serial})
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.refusal(); !errors.Is(err, appliance.ErrBusy) {
		t.Fatalf("без тревог: %v", err)
	}
	for alarm, want := range map[string]error{"TankLevel": appliance.ErrNotEnoughWater, "TankPresence": appliance.ErrNotEnoughWater,
		"NoCoffee": appliance.ErrContainerEmpty, "Waste": appliance.ErrContainerFull, "Drain": appliance.ErrDripTrayFull, "Descale": appliance.ErrHumanNeeded} {
		ap.values = map[string]any{AlarmPrefix + alarm: appliance.EventPresent}
		if err := ap.refusal(); !errors.Is(err, want) {
			t.Fatalf("%s: %v", alarm, err)
		}
	}
}
