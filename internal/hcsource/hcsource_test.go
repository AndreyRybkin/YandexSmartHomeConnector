package hcsource

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/hcapi"
	"hcgateway/internal/hcapi/hcfake"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/profile"
)

const (
	haID   = "BOSCH-OVEN-1"
	hotAir = "Cooking.Oven.Program.HeatingMode.HotAir"
	pizza  = "Cooking.Oven.Program.HeatingMode.PizzaSetting"
)

func init() { programPause = time.Millisecond }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ovenFake() *hcfake.Appliance {
	temp := []map[string]any{{"key": appliance.OptionSetpointTemperature, "type": "Int", "unit": "°C",
		"constraints": map[string]any{"min": 30, "max": 250, "stepsize": 5}}}
	return &hcfake.Appliance{HaID: haID, Name: "Духовка", Type: "Oven", Brand: "Bosch", Connected: true,
		Status: map[string]any{
			appliance.StatusOperationState:     appliance.OperationReady,
			appliance.StatusDoorState:          appliance.DoorClosed,
			appliance.StatusRemoteStartAllowed: true,
			statusLocalControl:                 false,
		},
		Settings: map[string]any{appliance.SettingPowerState: appliance.PowerOn},
		Programs: []hcfake.Program{{Key: hotAir, Options: temp}, {Key: pizza, Options: temp}},
		Commands: []string{appliance.CommandPause, appliance.CommandResume}}
}

// newCloud - облако с выполненным входом в поддельный аккаунт.
func newCloud(t *testing.T, fake *hcfake.Cloud, dir string) *Cloud {
	t.Helper()
	path := filepath.Join(dir, "homeconnect-tokens.json")
	if _, err := os.Stat(path); err != nil {
		fake.TokenFile(t, path)
	}
	api, err := hcapi.New(path, discard())
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(hcapi.Config{ClientID: hcfake.ClientID, ClientSecret: hcfake.ClientSecret, BaseURL: fake.URL})
	return New(api, filepath.Join(dir, "homeconnect"), discard())
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloudAppliance(t *testing.T) {
	fake := hcfake.New(t, ovenFake())
	dir := t.TempDir()
	c := newCloud(t, fake, dir)
	a := c.Appliance(haID)
	if a.Describe().Type != "" || a.State().Online {
		t.Fatal("описание или связь до потока событий")
	}
	events := make(chan []string, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, func(ev []string) {
		if len(ev) > 0 {
			events <- ev
		}
	})
	go c.Run(ctx)

	// Поток подключён: облако сообщает тип, состояние, команды и программы с параметрами.
	eventually(t, "описание и состояние", func() bool {
		d := a.Describe()
		return d.Type == "Oven" && len(d.Programs) == 2 && len(d.Commands) == 2 && a.State().Online
	})
	select {
	case <-c.Changes():
	default:
		t.Fatal("нет сигнала об изменении описания")
	}
	if o := a.Describe().Options[appliance.OptionSetpointTemperature]; o.Min != 30 || o.Max != 250 || o.Step != 5 {
		t.Fatalf("параметр: %+v", o)
	}
	if len(c.Account()) != 1 || !c.Connected() {
		t.Fatal("список аккаунта или связь потока")
	}

	cmd, done := context.WithTimeout(ctx, 3*time.Second)
	defer done()
	if err := a.Start(cmd, hotAir, map[string]any{appliance.OptionSetpointTemperature: 180.0}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Run", func() bool {
		st := a.State()
		return st.Values[appliance.StatusOperationState] == appliance.OperationRun && st.ActiveProgram == hotAir
	})
	// Прибор не в Ready и программу не останавливали: запуск не отправляется.
	if err := a.Start(cmd, pizza, nil); !errors.Is(err, appliance.ErrWrongMode) {
		t.Fatalf("запуск во время работы: %v", err)
	}
	// Смена программы: остановка, а новая программа запускается в фоне, когда прибор вернётся в Ready.
	if err := a.Stop(cmd); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Aborting", func() bool {
		return a.State().Values[appliance.StatusOperationState] != appliance.OperationRun
	})
	if err := a.Start(cmd, pizza, nil); err != nil {
		t.Fatalf("запуск после остановки: %v", err)
	}
	eventually(t, "новая программа", func() bool {
		st := a.State()
		return st.ActiveProgram == pizza && st.Values[appliance.StatusOperationState] == appliance.OperationRun
	})

	// Событие прибора уходит в уведомление.
	fake.Send(hcapi.EventEvent, haID, hcfake.Item(appliance.EventProgramFinished, appliance.EventPresent))
	select {
	case ev := <-events:
		if len(ev) != 1 || ev[0] != appliance.EventProgramFinished {
			t.Fatalf("события: %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("нет события")
	}
	// Местное управление: команда не отправляется.
	fake.Send(hcapi.EventStatus, haID, hcfake.Item(statusLocalControl, true))
	eventually(t, "местное управление", func() bool { return a.State().Values[statusLocalControl] == true })
	if err := a.Start(cmd, hotAir, nil); !errors.Is(err, appliance.ErrBusy) {
		t.Fatalf("местное управление: %v", err)
	}
	fake.Send(hcapi.EventDisconnected, haID)
	eventually(t, "нет связи", func() bool { return !a.State().Online })
	cancel()

	// После перезапуска описание берётся из файла, без запросов к облаку.
	again := New(c.API, filepath.Join(dir, "homeconnect"), discard()).Appliance(haID)
	if d := again.Describe(); d.Type != "Oven" || len(d.Programs) != 2 || len(d.Commands) != 2 {
		t.Fatalf("описание из файла: %+v", d)
	}
}

func TestMapErr(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{hcapi.ErrBusy, appliance.ErrBusy},
		{hcapi.ErrPaused, appliance.ErrBusy},
		{hcapi.ErrNotLoggedIn, appliance.ErrUnreachable},
		{&hcapi.APIError{Status: 400}, appliance.ErrInvalidValue},
		{&hcapi.APIError{Status: 403}, appliance.ErrNotSupported},
		{&hcapi.APIError{Status: 409}, appliance.ErrWrongMode},
		{&hcapi.APIError{Status: 429}, appliance.ErrBusy},
		{&hcapi.APIError{Status: 503}, appliance.ErrUnreachable},
		{errors.New("dial tcp: connection refused"), appliance.ErrUnreachable},
	}
	for _, c := range cases {
		if got := mapErr(c.err); !errors.Is(got, c.want) {
			t.Errorf("%v: %v", c.err, got)
		}
	}
	if mapErr(nil) != nil {
		t.Error("nil")
	}
}

// Смена токена переключает поток событий на облако нового токена сразу, без паузы.
func TestRunSwitchesLogin(t *testing.T) {
	var second atomic.Bool
	sse := func(onOpen func()) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/homeappliances/events" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if onOpen != nil {
				onOpen()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
	}
	first := sse(nil)
	defer first.Close()
	next := sse(func() { second.Store(true) })
	defer next.Close()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	api, err := hcapi.New(filepath.Join(dir, "tokens.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	tokens := func(base string) hcapi.Tokens {
		return hcapi.Tokens{Access: "a", Refresh: "r", ClientID: "app", BaseURL: base, Expires: time.Now().Add(time.Hour)}
	}
	if err := api.UseTokens(tokens(first.URL)); err != nil {
		t.Fatal(err)
	}
	c := New(api, filepath.Join(dir, "homeconnect"), log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !c.Connected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !c.Connected() {
		t.Fatal("поток не открылся")
	}
	if err := api.UseTokens(tokens(next.URL)); err != nil {
		t.Fatal(err)
	}
	for !second.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !second.Load() {
		t.Fatal("поток не перешёл на новый токен")
	}
}

// Forget забывает объект прибора: следующий вызов создаёт новый.
func TestForget(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	api, err := hcapi.New(filepath.Join(dir, "tokens.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	c := New(api, filepath.Join(dir, "homeconnect"), log)
	a := c.Appliance(haID)
	if c.Appliance(haID) != a {
		t.Fatal("новый объект без Forget")
	}
	c.Forget(haID)
	if c.Appliance(haID) == a {
		t.Fatal("объект не забыт")
	}
}

// Снимок облака не содержит событий: уровень соли после переподключения потока не сбрасывается.
func TestSnapshotKeepsEvents(t *testing.T) {
	const salt = "Dishcare.Dishwasher.Event.SaltLack"
	c := newCloud(t, hcfake.New(t, ovenFake()), t.TempDir())
	a := c.Appliance(haID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.loadState(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.state.Values[salt] = appliance.EventPresent
	a.state.Values[appliance.StatusOperationState] = "старое"
	a.mu.Unlock()
	if err := a.loadState(ctx); err != nil {
		t.Fatal(err)
	}
	st := a.State()
	if st.Values[salt] != appliance.EventPresent {
		t.Fatalf("уровень соли: %v", st.Values[salt])
	}
	if st.Values[appliance.StatusOperationState] != appliance.OperationReady {
		t.Fatalf("состояние из снимка: %v", st.Values[appliance.StatusOperationState])
	}
}

const (
	fridgeID       = "BOSCH-FRIDGE-1"
	fridgeSetpoint = "Refrigeration.FridgeFreezer.Setting.SetpointTemperatureRefrigerator"
	coffeeID       = "SIEMENS-COFFEE-1"
	ovenLight      = "Cooking.Oven.Setting.Light.Cavity.001.Power"
)

func fridgeFake() *hcfake.Appliance {
	return &hcfake.Appliance{HaID: fridgeID, Name: "Холодильник", Type: "Refrigerator", Brand: "Bosch", Connected: true,
		Status:   map[string]any{},
		Settings: map[string]any{fridgeSetpoint: 4, appliance.SettingPowerState: appliance.PowerOn},
		SettingInfo: map[string]map[string]any{fridgeSetpoint: {"unit": "°C", "type": "Double",
			"constraints": map[string]any{"min": 2, "max": 8, "stepsize": 1, "access": "readWrite"}}}}
}

// settingPath - путь GET настройки прибора.
func settingPath(id, key string) string {
	return "/api/homeappliances/" + id + "/settings/" + key
}

// described - прибор с загруженным состоянием и описанием. describe вызывает сам тест: так нет
// фоновых запросов, а тест знает, когда загрузка закончилась.
func described(t *testing.T, c *Cloud, id string) *Appliance {
	t.Helper()
	if err := c.RefreshAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := c.Appliance(id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.load(ctx)
	return a
}

// Пределы настройки приходят отдельным запросом и дают холодильнику умение «температура»; второй
// раз шлюз их не запрашивает, в том числе после перезапуска.
func TestSettingLimits(t *testing.T) {
	fake := hcfake.New(t, fridgeFake())
	dir := t.TempDir()
	c := newCloud(t, fake, dir)
	a := described(t, c, fridgeID)
	o := a.Describe().Options[fridgeSetpoint]
	if o.Min != 2 || o.Max != 8 || o.Step != 1 || o.ReadOnly {
		t.Fatalf("настройка: %+v", o)
	}
	select {
	case <-c.Changes():
	default:
		t.Fatal("нет сигнала об изменении описания")
	}
	p, err := profile.Resolve("Refrigerator", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := (&hcdevice.Device{ID: "f", Profile: p, Appliance: a}).Info()[0]
	if !slices.ContainsFunc(info.Capabilities, func(cap alice.Capability) bool { return cap.Type == alice.CapRange }) {
		t.Fatalf("нет умения range: %+v", info.Capabilities)
	}

	path := settingPath(fridgeID, fridgeSetpoint)
	if n := fake.Count(http.MethodGet, path); n != 1 {
		t.Fatalf("запросов настройки: %d", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.load(ctx)
	again := New(c.API, filepath.Join(dir, "homeconnect"), discard()).Appliance(fridgeID)
	if o := again.Describe().Options[fridgeSetpoint]; o.Min != 2 || o.Max != 8 {
		t.Fatalf("настройка из файла: %+v", o)
	}
	again.load(ctx)
	if n := fake.Count(http.MethodGet, path); n != 1 {
		t.Fatalf("повторный запрос настройки: %d", n)
	}
}

// Файл описания без признака загрузки: настройка лежит пустой, шлюз дозапрашивает её один раз и
// не теряет программы и команды из файла.
func TestSettingLimitsOldFile(t *testing.T) {
	fake := hcfake.New(t, fridgeFake())
	dir := t.TempDir()
	path := filepath.Join(dir, "homeconnect", fridgeID, "description.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"type":"Refrigerator","programs":["старая"],"options":{"` + fridgeSetpoint + `":{}},"commands":["к"],"commands_loaded":true}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	a := described(t, newCloud(t, fake, dir), fridgeID)
	d := a.Describe()
	if o := d.Options[fridgeSetpoint]; o.Min != 2 || o.Max != 8 {
		t.Fatalf("настройка: %+v", o)
	}
	if !slices.Contains(d.Programs, "старая") || !slices.Contains(d.Commands, "к") {
		t.Fatalf("описание из файла потеряно: %+v", d)
	}
}

// «Выключи» уходит значением из списка настройки: прибор без Standby получает Off.
func TestPowerOffFromAllowedValues(t *testing.T) {
	cases := []struct {
		name    string
		allowed []any
		want    string
	}{
		{"есть Off", []any{appliance.PowerOn, appliance.PowerOff}, appliance.PowerOff},
		{"только Standby", []any{appliance.PowerOn, appliance.PowerStandby}, appliance.PowerStandby},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &hcfake.Appliance{HaID: coffeeID, Name: "Кофемашина", Type: "CoffeeMaker", Brand: "Siemens", Connected: true,
				Status:   map[string]any{},
				Settings: map[string]any{appliance.SettingPowerState: appliance.PowerOn},
				SettingInfo: map[string]map[string]any{appliance.SettingPowerState: {
					"constraints": map[string]any{"allowedvalues": tc.allowed, "access": "readWrite"}}}}
			c := newCloud(t, hcfake.New(t, f), t.TempDir())
			a := described(t, c, coffeeID)
			p, err := profile.Resolve("CoffeeMaker", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			d := &hcdevice.Device{ID: "c", Profile: p, Appliance: a}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			act := alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: false}}
			if code := d.Act(ctx, "c", &act); code != "" {
				t.Fatalf("код: %s", code)
			}
			settings, err := c.API.Settings(ctx, coffeeID)
			if err != nil {
				t.Fatal(err)
			}
			if len(settings) != 1 || settings[0].Value != tc.want {
				t.Fatalf("настройка питания: %+v", settings)
			}
		})
	}
}

// hasBacklight - есть ли у прибора переключатель подсветки.
func hasBacklight(t *testing.T, typ string, a *Appliance) bool {
	t.Helper()
	p, err := profile.Resolve(typ, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := (&hcdevice.Device{ID: "d", Profile: p, Appliance: a}).Info()[0]
	return slices.ContainsFunc(info.Capabilities, func(c alice.Capability) bool {
		tp, ok := c.Parameters.(alice.InstanceParams)
		return c.Type == alice.CapToggle && ok && tp.Instance == "backlight"
	})
}

// Переключатель необязательной настройки есть, только если прибор сообщает её и разрешает менять.
func TestSettingAccess(t *testing.T) {
	cases := []struct {
		name   string
		access string
		want   bool
	}{{"можно менять", "readWrite", true}, {"только чтение", "read", false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ovenFake()
			f.Settings[ovenLight] = false
			f.SettingInfo = map[string]map[string]any{ovenLight: {"constraints": map[string]any{"access": tc.access}}}
			a := described(t, newCloud(t, hcfake.New(t, f), t.TempDir()), haID)
			if got := hasBacklight(t, "Oven", a); got != tc.want {
				t.Fatalf("подсветка: %v", got)
			}
		})
	}
}

// Подсветка вытяжки: без настройки в облаке переключателя нет, с настройкой есть.
func TestHoodLight(t *testing.T) {
	for _, has := range []bool{false, true} {
		f := ovenFake()
		f.Type = "Hood"
		f.Settings = map[string]any{}
		if has {
			f.Settings["Cooking.Common.Setting.Lighting"] = true
		}
		a := described(t, newCloud(t, hcfake.New(t, f), t.TempDir()), haID)
		if got := hasBacklight(t, "Hood", a); got != has {
			t.Fatalf("настройка есть: %v, подсветка: %v", has, got)
		}
	}
}

// Новое имя значения двери холодильника и Locked стиральной машины доходят до Алисы.
func TestDoorStates(t *testing.T) {
	cases := []struct {
		typ, status, value, want string
	}{
		{"Refrigerator", "Refrigeration.Common.Status.Door.Refrigerator", "Refrigeration.Common.EnumType.Door.States.Open", "opened"},
		{"Refrigerator", "Refrigeration.Common.Status.Door.Refrigerator", "BSH.Common.EnumType.DoorState.Closed", "closed"},
		{"Washer", appliance.StatusDoorState, "BSH.Common.EnumType.DoorState.Locked", "closed"},
		{"Dishwasher", appliance.StatusDoorState, "BSH.Common.EnumType.DoorState.Ajar", "opened"},
	}
	for _, tc := range cases {
		f := ovenFake()
		f.Type = tc.typ
		f.Status = map[string]any{tc.status: tc.value}
		a := described(t, newCloud(t, hcfake.New(t, f), t.TempDir()), haID)
		p, err := profile.Resolve(tc.typ, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal((&hcdevice.Device{ID: "d", Profile: p, Appliance: a}).States())
		want := `"state":{"instance":"open","value":"` + tc.want + `"}`
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s %s: %s", tc.typ, tc.value, raw)
		}
	}
}

// Прибор не на связи (409), 5xx и 429 прекращают загрузку после первого запроса: ни описание, ни
// признак не меняются, а следующая загрузка запрашивает настройку снова.
func TestSettingLimitsTemporaryFailure(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := fridgeFake()
			fake := hcfake.New(t, f)
			fake.FailSetting(appliance.SettingPowerState, status)
			dir := t.TempDir()
			a := described(t, newCloud(t, fake, dir), fridgeID)
			d := a.Describe()
			if _, ok := d.Options[fridgeSetpoint]; !ok || d.Options[fridgeSetpoint].Max != 0 {
				t.Fatalf("настройка: %+v", d.Options[fridgeSetpoint])
			}
			if d.Type != "Refrigerator" {
				t.Fatalf("описание стёрто: %+v", d)
			}
			// Имена сортируются: PowerState идёт первым, настройка холодильника не запрашивается.
			if n := fake.Count(http.MethodGet, settingPath(fridgeID, fridgeSetpoint)); n != 0 {
				t.Fatalf("запросы после сбоя: %d", n)
			}
			first := fake.Count(http.MethodGet, settingPath(fridgeID, appliance.SettingPowerState))
			want := 1
			if status >= 500 {
				want = 3 // Чтение после 5xx клиент сам повторяет 2 раза.
			}
			if first != want {
				t.Fatalf("запросов настройки: %d", first)
			}
			// Новый клиент: у прежнего после 429 очередь закрыта на минуту.
			fake.FailSetting(appliance.SettingPowerState, 0)
			a = described(t, newCloud(t, fake, dir), fridgeID)
			if o := a.Describe().Options[fridgeSetpoint]; o.Min != 2 || o.Max != 8 {
				t.Fatalf("настройка после восстановления: %+v", o)
			}
		})
	}
}

// Отказ на настройку отмечается как окончательный, но после 2 отказов подряд загрузка
// останавливается: 5 ошибок подряд поставили бы все запросы на паузу на 10 минут.
func TestSettingLimitsRefused(t *testing.T) {
	keys := []string{"A.Setting.One", "A.Setting.Two", "A.Setting.Three"}
	f := ovenFake()
	f.Settings = map[string]any{}
	for _, k := range keys {
		f.Settings[k] = true
	}
	fake := hcfake.New(t, f)
	for _, k := range keys {
		fake.FailSetting(k, http.StatusForbidden)
	}
	a := described(t, newCloud(t, fake, t.TempDir()), haID)
	total := func() int {
		n := 0
		for _, k := range keys {
			n += fake.Count(http.MethodGet, settingPath(haID, k))
		}
		return n
	}
	if n := total(); n != maxRefused {
		t.Fatalf("запросов после отказов: %d", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.load(ctx)
	if n := total(); n != len(keys) {
		t.Fatalf("запросов после второй загрузки: %d", n)
	}
	a.load(ctx)
	if n := total(); n != len(keys) {
		t.Fatalf("окончательный отказ запрошен снова: %d", n)
	}
	if err := a.Set(ctx, keys[0], false); err != nil {
		t.Fatalf("команды после отказов: %v", err)
	}
}

// Тело ответа не разобрано: настройка остаётся без пределов, и загрузка идёт дальше.
func TestSettingLimitsBadBody(t *testing.T) {
	f := fridgeFake()
	f.SettingInfo[appliance.SettingPowerState] = map[string]any{"constraints": map[string]any{"min": "не число"}}
	fake := hcfake.New(t, f)
	a := described(t, newCloud(t, fake, t.TempDir()), fridgeID)
	if o := a.Describe().Options[fridgeSetpoint]; o.Max != 8 {
		t.Fatalf("следующая настройка: %+v", o)
	}
	if o := a.Describe().Options[appliance.SettingPowerState]; o.Min != 0 || len(o.Values) != 0 {
		t.Fatalf("настройка с плохим ответом: %+v", o)
	}
}

// Пока идёт загрузка пределов одного прибора, другой уже на связи.
func TestSettingLimitsDoNotBlockStart(t *testing.T) {
	coffee := &hcfake.Appliance{HaID: coffeeID, Name: "Кофемашина", Type: "CoffeeMaker", Brand: "Siemens", Connected: true,
		Status: map[string]any{}, Settings: map[string]any{appliance.SettingPowerState: appliance.PowerOn}}
	fake := hcfake.New(t, fridgeFake(), coffee)
	release := fake.HoldSettings()
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	c := newCloud(t, fake, t.TempDir())
	fridge, machine := c.Appliance(fridgeID), c.Appliance(coffeeID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	eventually(t, "оба прибора на связи", func() bool { return fridge.State().Online && machine.State().Online })
	if o := fridge.Describe().Options[fridgeSetpoint]; o.Max != 0 {
		t.Fatalf("пределы до ответа облака: %+v", o)
	}
	release()
	released = true
	eventually(t, "пределы", func() bool { return fridge.Describe().Options[fridgeSetpoint].Max == 8 })
}
