package web

import (
	"bufio"
	"context"
	"hcgateway/internal/appliance"
	"hcgateway/internal/profile"
	"html"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/auth"
	"hcgateway/internal/config"
	"hcgateway/internal/delonghi/dlfake"
	"hcgateway/internal/gateway"
	"hcgateway/internal/hcapi/hcfake"
	"hcgateway/internal/linking"
	"hcgateway/internal/metrics"
	"hcgateway/internal/statedb"
)

const (
	testUser     = "owner"
	testPassword = "password-0001"
)

type fixture struct {
	t       *testing.T
	dir     string
	cfgPath string
	auth    *auth.Auth
	gw      *gateway.Gateway
	state   *statedb.DB
	hs      *httptest.Server
	client  *http.Client
	cookie  string
	csrf    string
	version string
}

// newFixture запускает интерфейс с настройками cfgText. before готовит каталог данных до запуска шлюза.
func newFixture(t *testing.T, cfgText string, before ...func(dir string)) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir()}
	for _, b := range before {
		b(f.dir)
	}
	f.cfgPath = filepath.Join(f.dir, "config.yaml")
	if err := os.WriteFile(f.cfgPath, []byte("data_dir: "+f.dir+"\n"+cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := config.OpenFile(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := file.Current()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := linking.OpenStore(filepath.Join(f.dir, "yandex-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.auth = auth.New(auth.NewUsers(filepath.Join(f.dir, auth.UserFile), log), nil, log)
	if f.gw, err = gateway.New(f.dir, store, f.auth, log); err != nil {
		t.Fatal(err)
	}
	f.state = statedb.Open(filepath.Join(f.dir, statedb.FileName), log)
	t.Cleanup(f.state.Close)
	f.auth.Bans.Attach(f.state)
	f.gw.Attach(f.state)
	if err := f.gw.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	f.hs = httptest.NewServer(New(f.auth, f.gw, file, cfg, log))
	t.Cleanup(f.hs.Close)
	f.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

func (f *fixture) writeUser(password string) {
	f.t.Helper()
	u, err := auth.NewUser(testUser, password)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := auth.SaveUser(filepath.Join(f.dir, auth.UserFile), u); err != nil {
		f.t.Fatal(err)
	}
}

var (
	csrfRe    = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	versionRe = regexp.MustCompile(`name="version" value="([^"]+)"`)
)

func (f *fixture) do(method, path string, form url.Values) (int, string, *http.Response) {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, f.hs.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if f.cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp
}

// get открывает страницу и запоминает токен форм и версию настроек.
func (f *fixture) get(path string) (int, string) {
	f.t.Helper()
	status, body, _ := f.do(http.MethodGet, path, nil)
	if m := csrfRe.FindStringSubmatch(body); m != nil {
		f.csrf = m[1]
	}
	if m := versionRe.FindStringSubmatch(body); m != nil {
		f.version = m[1]
	}
	return status, body
}

// post отправляет форму с токеном и версией страницы, открытой последней.
func (f *fixture) post(path string, form url.Values) (int, string, string) {
	f.t.Helper()
	if !form.Has("csrf") {
		form.Set("csrf", f.csrf)
	}
	if !form.Has("version") {
		form.Set("version", f.version)
	}
	status, body, resp := f.do(http.MethodPost, path, form)
	return status, body, resp.Header.Get("Location")
}

func (f *fixture) login() {
	f.t.Helper()
	status, _, resp := f.do(http.MethodPost, "/login", url.Values{"username": {testUser}, "password": {testPassword}})
	if status != http.StatusSeeOther {
		f.t.Fatalf("вход: %d", status)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			f.cookie = c.Value
		}
	}
}

func (f *fixture) config() *config.Config {
	f.t.Helper()
	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return cfg
}

const demoConfig = `devices:
  oven:
    name: Духовка
    room: Кухня
    source: demo
    demo: Oven
`

func TestLogin(t *testing.T) {
	f := newFixture(t, demoConfig)
	if status, body := f.get("/login"); status != http.StatusOK || !strings.Contains(body, "web-user") {
		t.Fatalf("без пользователя: %d", status)
	}
	f.writeUser(testPassword)
	if _, body := f.get("/login"); strings.Contains(body, "не создан") {
		t.Fatal("пользователь создан, а страница просит создать")
	}
	if status, _, resp := f.do(http.MethodGet, "/", nil); status != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("без входа: %d", status)
	}
	status, body, _ := f.do(http.MethodPost, "/login", url.Values{"username": {testUser}, "password": {"wrong-password"}})
	if status != http.StatusUnauthorized || !strings.Contains(body, "Неверный логин или пароль") {
		t.Fatalf("неверный пароль: %d", status)
	}

	status, _, resp := f.do(http.MethodPost, "/login", url.Values{"username": {testUser}, "password": {testPassword}})
	if status != http.StatusSeeOther {
		t.Fatalf("вход: %d", status)
	}
	c := resp.Cookies()[0]
	if c.Name != sessionCookie || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 12*3600 {
		t.Fatalf("cookie: %+v", c)
	}
	f.cookie = c.Value
	status, body = f.get("/")
	if status != http.StatusOK || !strings.Contains(body, "Духовка") {
		t.Fatalf("список приборов: %d", status)
	}
	h := resp.Header
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("заголовки: %v", h)
	}

	// Форма без токена сессии не принимается.
	if status, _, _ := f.post("/reload", url.Values{"csrf": {"чужой"}}); status != http.StatusForbidden {
		t.Fatalf("форма без токена: %d", status)
	}
	// Новый пароль закрывает сессии.
	f.writeUser("password-0002")
	if status, _, _ := f.do(http.MethodGet, "/", nil); status != http.StatusSeeOther {
		t.Fatalf("сессия после смены пароля: %d", status)
	}
}

func TestLoginBan(t *testing.T) {
	f := newFixture(t, "")
	f.writeUser(testPassword)
	for range auth.MaxFailures {
		f.do(http.MethodPost, "/login", url.Values{"username": {testUser}, "password": {"wrong-password"}})
	}
	status, body, _ := f.do(http.MethodPost, "/login", url.Values{"username": {testUser}, "password": {testPassword}})
	if status != http.StatusTooManyRequests || !strings.Contains(body, "Слишком много попыток, попробуйте позже") {
		t.Fatalf("после бана: %d", status)
	}
	bans := f.auth.Bans.List()
	if len(bans) != 1 {
		t.Fatalf("баны: %v", bans)
	}
	// Снять бан можно из интерфейса, войдя с другого адреса.
	f.auth.Bans.Lift(bans[0].Prefix)
	f.login()
	f.do(http.MethodPost, "/login", url.Values{"username": {testUser}, "password": {"wrong-password"}})
	for range auth.MaxFailures - 1 {
		f.do(http.MethodPost, "/login", url.Values{"username": {testUser}, "password": {"wrong-password"}})
	}
	if _, body := f.get("/logins"); !strings.Contains(body, bans[0].Prefix.String()) || !strings.Contains(body, "Снять") {
		t.Fatal("бана нет на странице входов")
	}
	if status, _, loc := f.post("/logins/lift", url.Values{"prefix": {bans[0].Prefix.String()}}); status != http.StatusSeeOther || loc != "/logins" {
		t.Fatalf("снять бан: %d", status)
	}
	if len(f.auth.Bans.List()) != 0 {
		t.Fatal("бан не снят")
	}
}

func TestDevicesEdit(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	f.get("/")

	saves := func(result string) float64 { return metrics.Value("hcgateway_config_saves_total", "result", result) }
	ok, invalid, conflict := saves(metrics.SaveOK), saves(metrics.SaveInvalid), saves(metrics.SaveConflict)

	// Ошибка в форме не сохраняется и показывается на странице.
	status, body, _ := f.post("/devices", url.Values{"id": {"../x"}, "name": {"Посудомойка"}, "source": {"demo"}, "demo": {"Dishwasher"}})
	if status != http.StatusBadRequest || !strings.Contains(body, "Идентификатор") || !strings.Contains(body, "Посудомойка") {
		t.Fatalf("неверный идентификатор: %d", status)
	}
	status, _, loc := f.post("/devices", url.Values{"id": {"dish"}, "name": {"Посудомойка"}, "room": {"Кухня"}, "source": {"demo"}, "demo": {"Dishwasher"}})
	if status != http.StatusSeeOther || loc != "/devices/dish" {
		t.Fatalf("добавление: %d %s", status, loc)
	}
	if d := f.config().Devices["dish"]; d.Name != "Посудомойка" || d.Demo != "Dishwasher" {
		t.Fatalf("в файле: %+v", d)
	}
	if _, ok := f.gw.Device("dish"); !ok {
		t.Fatal("прибор не применён")
	}
	if _, err := os.Stat(f.cfgPath + ".bak"); err != nil {
		t.Fatal("нет .bak")
	}
	// Правка по старой версии страницы не сохраняется.
	status, body, _ = f.post("/devices", url.Values{"id": {"dish2"}, "name": {"Ещё"}, "source": {"demo"}, "demo": {"Oven"}})
	if status != http.StatusBadRequest || !strings.Contains(body, "Откройте её заново") {
		t.Fatalf("старая версия: %d", status)
	}
	if saves(metrics.SaveOK) != ok+1 || saves(metrics.SaveInvalid) != invalid+1 || saves(metrics.SaveConflict) != conflict+1 {
		t.Fatal("сохранения настроек не посчитаны")
	}

	// Имя и комната.
	f.get("/devices/oven")
	if status, _, _ := f.post("/devices/oven", url.Values{"name": {"Духовка на кухне"}, "room": {""}, "demo": {"Oven"}}); status != http.StatusSeeOther {
		t.Fatalf("имя: %d", status)
	}
	if d := f.config().Devices["oven"]; d.Name != "Духовка на кухне" || d.Room != "" {
		t.Fatalf("имя в файле: %+v", d)
	}

	// Команда идёт тем же путём, что у Алисы.
	f.get("/devices/oven")
	if status, _, _ := f.post("/devices/oven/command", url.Values{"type": {"devices.capabilities.on_off"}, "instance": {"on"}, "value": {"true"}}); status != http.StatusSeeOther {
		t.Fatalf("команда: %d", status)
	}
	if hc, _ := f.gw.HomeConnect("oven"); !hc.Appliance.State().Running() {
		t.Fatal("программа не запущена")
	}
	_, body = f.get("/devices/oven")
	if !strings.Contains(body, "Команда выполнена") || !strings.Contains(body, `name="value" value="180"`) {
		t.Fatal("нет сообщения о команде или текущей температуры")
	}
	f.post("/devices/oven/command", url.Values{"type": {"devices.capabilities.range"}, "instance": {"temperature"}, "value": {"500"}})
	if _, body := f.get("/devices/oven"); !strings.Contains(body, "INVALID_VALUE") {
		t.Fatal("нет ошибки команды")
	}

	// Удаление.
	f.get("/devices/dish/delete")
	if status, _, loc := f.post("/devices/dish/delete", url.Values{}); status != http.StatusSeeOther || loc != "/" {
		t.Fatalf("удаление: %d", status)
	}
	if _, ok := f.config().Devices["dish"]; ok {
		t.Fatal("прибор не удалён из файла")
	}
	if _, ok := f.gw.Device("dish"); ok {
		t.Fatal("прибор не удалён из шлюза")
	}
}

func TestProfileEdit(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/devices/oven")
	if !strings.Contains(body, `<span title="Cooking.Oven.Program.HeatingMode.HotAir">HotAir</span>`) || strings.Contains(body, "встроенный профиль") {
		t.Fatal("нет программ прибора или у разделов пометка встроенного профиля")
	}

	// Режим одной программы: строки прибора и значения из списка Яндекса.
	form := url.Values{"op": {"save"}, "m.0.instance": {"program"}, "m.0.source": {"programs"},
		"m.0.k.0": {"Cooking.Oven.Program.HeatingMode.HotAir"}, "m.0.v.0": {"baking"},
		"m.0.k.1": {"Cooking.Oven.Program.HeatingMode.PizzaSetting"}, "m.0.v.1": {"pizza"}}
	if status, _, _ := f.post("/devices/oven/profile/modes", form); status != http.StatusSeeOther {
		t.Fatalf("режимы: %d", status)
	}
	p := f.config().Devices["oven"].Profile
	if p == nil || len(p.Modes) != 1 || len(p.Modes[0].Programs) != 2 || p.Modes[0].Programs["Cooking.Oven.Program.HeatingMode.PizzaSetting"] != "pizza" {
		t.Fatalf("профиль в файле: %+v", p)
	}
	hc, _ := f.gw.HomeConnect("oven")
	if len(hc.Profile.ProgramMode().Programs) != 2 {
		t.Fatal("профиль не применён")
	}

	// Ошибка профиля показывается у раздела, введённое не теряется.
	_, body = f.get("/devices/oven")
	if !strings.Contains(body, "задано для прибора") || !strings.Contains(body, "Вернуть встроенное") {
		t.Fatal("нет отметки переопределения")
	}
	form = url.Values{"op": {"save"}, "m.0.instance": {"program"}, "m.0.source": {"programs"},
		"m.0.k.0": {"Cooking.Oven.Program.HeatingMode.HotAir"}, "m.0.v.0": {"one"},
		"m.0.k.1": {"Cooking.Oven.Program.HeatingMode.PizzaSetting"}, "m.0.v.1": {"one"}}
	status, body, _ := f.post("/devices/oven/profile/modes", form)
	if status != http.StatusBadRequest || !strings.Contains(body, "назначен двум значениям") {
		t.Fatalf("ошибка профиля: %d", status)
	}

	// Значение, равное встроенному, не сохраняется; кнопка сброса убирает переопределение.
	f.get("/devices/oven")
	if status, _, _ := f.post("/devices/oven/profile/on_off", url.Values{"op": {"save"}, "on_off": {"program"}}); status != http.StatusSeeOther {
		t.Fatalf("on_off: %d", status)
	}
	if p := f.config().Devices["oven"].Profile; p.OnOff != "" {
		t.Fatalf("встроенное значение записано: %+v", p)
	}
	f.get("/devices/oven")
	if status, _, _ := f.post("/devices/oven/profile/modes", url.Values{"op": {"reset"}}); status != http.StatusSeeOther {
		t.Fatalf("сброс: %d", status)
	}
	if p := f.config().Devices["oven"].Profile; p != nil {
		t.Fatalf("после сброса: %+v", p)
	}

	// Пустой список - переопределение пустым: у духовки пропадает пауза.
	f.get("/devices/oven")
	if status, _, _ := f.post("/devices/oven/profile/toggles", url.Values{"op": {"save"}, "t.0.instance": {"pause"}, "t.0.delete": {"1"}, "t.1.instance": {""}}); status != http.StatusSeeOther {
		t.Fatalf("переключатели: %d", status)
	}
	if p := f.config().Devices["oven"].Profile; p == nil || p.Toggles == nil || len(p.Toggles) != 0 {
		t.Fatalf("пустые переключатели: %+v", p)
	}

	// Профиль типа действует на все приборы типа.
	f.get("/profiles/Oven")
	form = url.Values{"op": {"save"}, "o.0.k": {"Cooking.Oven.Option.SetpointTemperature"}, "o.0.v": {"200"}}
	if status, _, _ := f.post("/profiles/Oven/default_options", form); status != http.StatusSeeOther {
		t.Fatalf("профиль типа: %d", status)
	}
	if p := f.config().Profiles["Oven"]; p == nil || p.DefaultOptions["Cooking.Oven.Option.SetpointTemperature"] != 200.0 {
		t.Fatalf("профиль типа в файле: %+v", p)
	}
	if hc, _ := f.gw.HomeConnect("oven"); hc.Profile.DefaultOptions["Cooking.Oven.Option.SetpointTemperature"] != 200.0 {
		t.Fatal("профиль типа не применён")
	}
	if _, body := f.get("/profiles"); !strings.Contains(body, "default_options") {
		t.Fatal("нет отметки на странице профилей")
	}
	if status, _ := f.get("/profiles/Kettle"); status != http.StatusNotFound {
		t.Fatalf("неизвестный тип: %d", status)
	}
}

func TestConnections(t *testing.T) {
	f := newFixture(t, "yandex:\n  client_id: yandex\n  client_secret: 0123456789abcdef\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	if strings.Contains(body, "0123456789abcdef") || !strings.Contains(body, "задан") {
		t.Fatal("секрет на странице")
	}
	// Пустое поле секрета оставляет прежнее значение.
	form := url.Values{"skill_id": {"skill-1"}, "callback_token": {"owner-token"}, "client_id": {"yandex-2"}, "client_secret": {""},
		"redirect_uris": {"https://social.yandex.net/broker/redirect\r\nhttps://social.yandex.ru/broker/redirect\r\n"}}
	if status, _, _ := f.post("/connections/yandex", form); status != http.StatusSeeOther {
		t.Fatalf("сохранение: %d", status)
	}
	y := f.config().Yandex
	if y.SkillID != "skill-1" || y.CallbackToken != "owner-token" || y.ClientID != "yandex-2" || y.ClientSecret != "0123456789abcdef" {
		t.Fatalf("в файле: %+v", y)
	}
	if !f.gw.Notifier.Enabled() {
		t.Fatal("навык не применён")
	}
	data, _ := os.ReadFile(f.cfgPath)
	if strings.Contains(string(data), "redirect_uris") {
		t.Fatal("значение по умолчанию записано в файл")
	}

	// Правила раздела 3: client_secret не короче 16 символов.
	f.get("/connections")
	status, body, _ := f.post("/connections/yandex", url.Values{"client_id": {"yandex-2"}, "client_secret": {"short"},
		"redirect_uris": {"https://social.yandex.net/broker/redirect"}})
	if status != http.StatusBadRequest || !strings.Contains(body, "client_secret") || strings.Contains(body, "owner-token") {
		t.Fatalf("короткий секрет: %d", status)
	}
	// «Удалить» убирает секрет.
	f.get("/connections")
	form = url.Values{"client_id": {"yandex-2"}, "callback_token_clear": {"1"}, "skill_id": {""}, "redirect_uris": {"https://social.yandex.net/broker/redirect"}}
	if status, _, _ := f.post("/connections/yandex", form); status != http.StatusSeeOther {
		t.Fatalf("удаление токена: %d", status)
	}
	if y := f.config().Yandex; y.CallbackToken != "" || y.SkillID != "" {
		t.Fatalf("токен не удалён: %+v", y)
	}
}

func TestReload(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	f.get("/")
	// Ручная правка: сохранение из интерфейса её не затирает, а «Перечитать файл» загружает.
	data, _ := os.ReadFile(f.cfgPath)
	edited := strings.Replace(string(data), "name: Духовка", "name: Духовка из файла", 1)
	if err := os.WriteFile(f.cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body, _ := f.post("/devices", url.Values{"id": {"dish"}, "name": {"Посудомойка"}, "source": {"demo"}, "demo": {"Dishwasher"}})
	if status != http.StatusBadRequest || !strings.Contains(body, "Перечитать файл") {
		t.Fatalf("сохранение после ручной правки: %d", status)
	}
	if status, _, _ := f.post("/reload", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("перечитать: %d", status)
	}
	if hc, _ := f.gw.HomeConnect("oven"); hc.Name != "Духовка из файла" {
		t.Fatalf("правка не применена: %s", hc.Name)
	}
	_, body = f.get("/")
	if !strings.Contains(body, "Файл настроек перечитан") {
		t.Fatal("нет сообщения")
	}
	// Ошибка в файле: шлюз остаётся с прежними настройками.
	if err := os.WriteFile(f.cfgPath, []byte(edited+"unknown: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.post("/reload", url.Values{})
	if _, body := f.get("/"); !strings.Contains(body, "Файл не перечитан") {
		t.Fatal("нет ошибки перечитывания")
	}
	if hc, _ := f.gw.HomeConnect("oven"); hc.Name != "Духовка из файла" {
		t.Fatal("ошибка в файле изменила шлюз")
	}
}

func TestEmptyConfig(t *testing.T) {
	f := newFixture(t, "")
	f.writeUser(testPassword)
	f.login()
	// На страницах - число несделанных шагов настройки, сами шаги - на «Подключениях».
	if _, body := f.get("/"); !strings.Contains(body, "Не сделано шагов настройки: 3") || !strings.Contains(body, `<details class="field" id="add" open>`) {
		t.Fatal("нет строки о шагах настройки или форма добавления закрыта")
	}
	_, body := f.get("/connections")
	for _, want := range []string{`id="todo"`, "Не заданы идентификатор и секрет приложения: Яндекс не может привязать аккаунт.",
		"Нет ни одного прибора", `<a href="/#add">`} {
		if !strings.Contains(body, want) {
			t.Errorf("нет подсказки %q", want)
		}
	}
	if strings.Contains(body, "Не сделано шагов настройки") {
		t.Error("на «Подключениях» строка о шагах вместо списка")
	}
	if _, body := f.get("/logins"); !strings.Contains(body, "127.0.0.1") {
		t.Fatal("нет адреса клиента")
	}
}

func TestMetricsToken(t *testing.T) {
	f := newFixture(t, "")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	if !strings.Contains(body, "закрыт") {
		t.Fatal("нет отметки, что порт метрик закрыт")
	}
	if status, body, _ := f.post("/connections/metrics", url.Values{"metrics_token": {"short"}}); status != http.StatusBadRequest || !strings.Contains(body, "metrics.token") {
		t.Fatalf("короткий токен: %d", status)
	}
	f.get("/connections")
	token := strings.Repeat("a", 32)
	if status, _, _ := f.post("/connections/metrics", url.Values{"metrics_token": {token}}); status != http.StatusSeeOther {
		t.Fatalf("токен: %d", status)
	}
	if f.config().Metrics.Token != token || f.gw.Config().Metrics.Token != token {
		t.Fatal("токен не сохранён или не применён")
	}
	_, body = f.get("/connections")
	if strings.Contains(body, token) || !strings.Contains(body, "порт метрик откроется после перезапуска") {
		t.Fatal("токен на странице или нет подсказки о перезапуске")
	}
}

func TestHomeConnect(t *testing.T) {
	fake := hcfake.New(t, &hcfake.Appliance{HaID: "BOSCH-OVEN-1", Name: "Духовка", Type: "Oven", Brand: "Bosch",
		Connected: true, Status: map[string]any{}, Settings: map[string]any{}})
	cfg := "homeconnect:\n  client_id: " + hcfake.ClientID + "\n  client_secret: " + hcfake.ClientSecret + "\n  api_url: " + fake.URL + "\n"
	f := newFixture(t, cfg, func(dir string) { fake.TokenFile(t, filepath.Join(dir, "homeconnect-tokens.json")) })
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	if !strings.Contains(body, "Вход выполнен") || strings.Contains(body, hcfake.ClientSecret) {
		t.Fatal("нет отметки о входе или секрет на странице")
	}
	if status, _, _ := f.post("/connections/homeconnect/appliances", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("список приборов: %d", status)
	}
	_, body = f.get("/connections")
	if !strings.Contains(body, "BOSCH-OVEN-1") || !strings.Contains(body, `href="/?ha_id=BOSCH-OVEN-1#add"`) {
		t.Fatal("нет прибора аккаунта со ссылкой «Добавить»")
	}
	// «Добавить» заполняет форму прибором аккаунта.
	_, body = f.get("/?ha_id=BOSCH-OVEN-1")
	if !strings.Contains(body, `value="Духовка"`) || !strings.Contains(body, `<option value="homeconnect-cloud" selected>`) {
		t.Fatal("форма не заполнена прибором аккаунта")
	}
	form := url.Values{"id": {"oven"}, "name": {"Духовка"}, "source": {"homeconnect-cloud"}, "ha_id": {"BOSCH-OVEN-1"}, "demo": {"Oven"}}
	if status, _, _ := f.post("/devices", form); status != http.StatusSeeOther {
		t.Fatalf("добавление прибора облака: %d", status)
	}
	if d := f.config().Devices["oven"]; d.HaID != "BOSCH-OVEN-1" || d.Demo != "" || d.Source != "homeconnect-cloud" {
		t.Fatalf("в файле: %+v", d)
	}
	// Тип известен из списка аккаунта: прибор сразу собран, а не ждёт облака.
	if _, ok := f.gw.HomeConnect("oven"); !ok || f.gw.Waiting("oven") {
		t.Fatal("прибор облака не собран")
	}
	if _, body := f.get("/connections"); !strings.Contains(body, `href="/devices/oven"`) {
		t.Fatal("прибор аккаунта не отмечен как добавленный")
	}

	// Новый вход: страница показывает код и обновляется сама.
	if status, _, _ := f.post("/connections/homeconnect/login", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("вход: %d", status)
	}
	_, body = f.get("/connections")
	if !strings.Contains(body, hcfake.UserCode) || !regexp.MustCompile(`<meta http-equiv="refresh" content="5; url=/connections\?r=[0-9]+#homeconnect">`).MatchString(body) {
		t.Fatal("нет кода входа или обновления страницы")
	}
	if status, _, _ := f.post("/connections/homeconnect/logout", url.Values{}); status != http.StatusSeeOther || f.gw.Cloud.API.LoggedIn() {
		t.Fatalf("выход: %d", status)
	}
}

// writeLocalKeys кладёт в data_dir/appliances/<haId> то, что сохраняет fetch-keys.
func writeLocalKeys(t *testing.T, dataDir, haID string) {
	t.Helper()
	dir := filepath.Join(dataDir, "appliances", haID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"appliance.json":        `{"haId":"` + haID + `","brand":"Siemens","haType":"Dishwasher","mode":"tls","key":"AAAAAAAAAAAAAAAAAAAAAA"}`,
		"FeatureMapping.xml":    `<m><featureDescription><feature refUID="0201">BSH.Common.Status.DoorState</feature></featureDescription></m>`,
		"DeviceDescription.xml": `<device><statusList uid="0010"><status uid="0201" access="read"/></statusList></device>`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocal(t *testing.T) {
	f := newFixture(t, "", func(dir string) { writeLocalKeys(t, dir, "SIEMENS-DISH-1") })
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	if !strings.Contains(body, "TLS-PSK, порт 443") || !strings.Contains(body, `href="/?local_id=SIEMENS-DISH-1#add"`) {
		t.Fatal("нет прибора с ключами со ссылкой «Добавить»")
	}
	_, body = f.get("/?local_id=SIEMENS-DISH-1")
	if !strings.Contains(body, `<option value="homeconnect-local" selected>`) || !strings.Contains(body, `<option value="SIEMENS-DISH-1" selected>`) {
		t.Fatal("форма не заполнена прибором с ключами")
	}
	form := url.Values{"id": {"dish"}, "name": {"Посудомойка"}, "source": {"homeconnect-local"}, "ha_id": {"SIEMENS-DISH-1"},
		"demo": {"Oven"}, "device_id": {"lg"}}
	if status, body, _ := f.post("/devices", form); status != http.StatusBadRequest || !strings.Contains(body, "devices.dish.host") {
		t.Fatalf("прибор без адреса: %d", status)
	}
	f.get("/")
	form.Set("host", "192.168.1.5")
	if status, _, _ := f.post("/devices", form); status != http.StatusSeeOther {
		t.Fatalf("добавление: %d", status)
	}
	if d := f.config().Devices["dish"]; d.HaID != "SIEMENS-DISH-1" || d.Host != "192.168.1.5" || d.Demo != "" || d.DeviceID != "" {
		t.Fatalf("в файле: %+v", d)
	}
	_, body = f.get("/devices/dish")
	if !strings.Contains(body, `name="host" value="192.168.1.5"`) || !strings.Contains(body, `<span class="chip off">нет связи с запуска шлюза</span>`) {
		t.Fatal("страница прибора без адреса или со связью")
	}
	if status, _, _ := f.post("/devices/dish", url.Values{"name": {"Посудомойка"}, "ha_id": {"SIEMENS-DISH-1"},
		"host": {"dish.local:8443"}}); status != http.StatusSeeOther {
		t.Fatalf("смена адреса: %d", status)
	}
	if d := f.config().Devices["dish"]; d.Host != "dish.local:8443" {
		t.Fatalf("адрес в файле: %q", d.Host)
	}
	if _, body = f.get("/connections"); !strings.Contains(body, `href="/devices/dish"`) {
		t.Fatal("прибор с ключами не отмечен как добавленный")
	}
}

// Профили поддельных приборов LG повторяют раскладку из библиотеки LG, значения выдуманы.
const (
	lgWasherProfile = `{"property":[{"location":{"locationName":"MAIN"},` +
		`"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["INITIAL","RUNNING","END"]}}},` +
		`"operation":{"washerOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP"]}}}}],` +
		`"notification":{"push":["WASHING_IS_COMPLETE"]}}`
	lgFridgeProfile = `{"property":{"temperatureInUnits":[` +
		`{"locationName":"FRIDGE","targetTemperatureC":{"type":"range","mode":["r","w"],"value":{"w":{"min":1,"max":7,"step":1}}}},` +
		`{"locationName":"FREEZER","targetTemperatureC":{"type":"range","mode":["r","w"],"value":{"w":{"min":-24,"max":-14,"step":1}}}}]}}`
)

// newLG - поддельное облако ThinQ по PAT test-pat: стиральная машина и холодильник с двумя камерами.
func newLG(t *testing.T) *httptest.Server {
	t.Helper()
	profiles := map[string]string{"lg-washer-1": lgWasherProfile, "lg-fridge-1": lgFridgeProfile}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"messageId":"m","response":[`+
			`{"deviceId":"lg-washer-1","deviceInfo":{"deviceType":"DEVICE_WASHER","modelName":"F4V9","alias":"Стиралка"}},`+
			`{"deviceId":"lg-fridge-1","deviceInfo":{"deviceType":"DEVICE_REFRIGERATOR","modelName":"GA-B","alias":"Холодильник"}}]}`)
	})
	mux.HandleFunc("GET /devices/{id}/profile", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"messageId":"m","response":`+profiles[r.PathValue("id")]+`}`)
	})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-pat" || r.Header.Get("x-country") != "NL" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"code":"1102","message":"invalid token"}}`)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	return hs
}

func TestThinQ(t *testing.T) {
	lg := newLG(t)
	f := newFixture(t, "thinq:\n  api_url: "+lg.URL+"\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	if !strings.Contains(body, `id="thinq"`) || !strings.Contains(body, `PAT</span>: не задан`) {
		t.Fatal("нет раздела LG ThinQ")
	}
	if status, _, _ := f.post("/connections/thinq", url.Values{"pat": {"test-pat"}, "country": {"xx"}, "api_url": {lg.URL}}); status != http.StatusSeeOther {
		t.Fatalf("неверная страна: %d", status)
	}
	if _, body = f.get("/connections"); !strings.Contains(body, "thinq.country") || f.config().ThinQ.PAT != "" {
		t.Fatal("неверная страна сохранена или нет ошибки")
	}
	if status, _, _ := f.post("/connections/thinq", url.Values{"pat": {"test-pat"}, "country": {"nl"}, "api_url": {lg.URL}}); status != http.StatusSeeOther {
		t.Fatalf("сохранение: %d", status)
	}
	if c := f.config().ThinQ; c.PAT != "test-pat" || c.Country != "NL" || c.APIURL != lg.URL {
		t.Fatalf("в файле: %+v", c)
	}
	_, body = f.get("/connections")
	if strings.Contains(body, "test-pat") || !strings.Contains(body, `PAT</span>: задан`) {
		t.Fatal("PAT на странице или не отмечен")
	}
	// «Обновить список» загружает приборы аккаунта и их профили: у холодильника - строка на камеру.
	if status, _, _ := f.post("/connections/thinq/devices", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("список приборов: %d", status)
	}
	_, body = f.get("/connections")
	for _, want := range []string{`href="/?thinq_id=lg-washer-1#add"`, `href="/?thinq_id=lg-fridge-1&amp;location=FRIDGE#add"`,
		`href="/?thinq_id=lg-fridge-1&amp;location=FREEZER#add"`, "морозильная камера"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	// «Добавить» заполняет форму прибором аккаунта.
	_, body = f.get("/?thinq_id=lg-washer-1")
	if !strings.Contains(body, `value="Стиралка"`) || !strings.Contains(body, `<option value="thinq" selected>`) ||
		!strings.Contains(body, `<option value="lg-washer-1" selected>`) {
		t.Fatal("форма не заполнена прибором аккаунта")
	}
	form := url.Values{"id": {"washer"}, "name": {"Стиралка"}, "source": {"thinq"}, "device_id": {"lg-washer-1"},
		"demo": {"Oven"}, "ha_id": {"BOSCH-OVEN-1"}, "location": {""}}
	if status, _, _ := f.post("/devices", form); status != http.StatusSeeOther {
		t.Fatalf("добавление прибора LG: %d", status)
	}
	if d := f.config().Devices["washer"]; d.DeviceID != "lg-washer-1" || d.Demo != "" || d.HaID != "" || d.Source != "thinq" {
		t.Fatalf("в файле: %+v", d)
	}
	// Профиль LG уже загружен: прибор сразу собран по встроенному профилю типа.
	hc, ok := f.gw.HomeConnect("washer")
	if !ok || hc.Profile.YandexType != "washing_machine" || hc.Manufacturer != "LG" || f.gw.Waiting("washer") {
		t.Fatal("стиральная машина не собрана")
	}
	_, body = f.get("/devices/washer")
	if !strings.Contains(body, "deviceId lg-washer-1") || !strings.Contains(body, `<span class="chip off">нет связи с запуска шлюза</span>`) ||
		!strings.Contains(body, `name="power_setting" value="operation.washerOperationMode"`) || strings.Contains(body, `id="default_program"`) {
		t.Fatal("страница прибора LG: нет deviceId, связи или полей питания, или есть программа по умолчанию")
	}
	// Питание из редактора сохраняется в профиль прибора.
	if status, _, _ := f.post("/devices/washer/profile/on_off", url.Values{"op": {"save"}, "on_off": {"power"},
		"power_setting": {"operation.washerOperationMode"}, "power_on": {"START"}, "power_off": {"STOP"},
		"power_state": {"runState.currentState"}, "power_idle": {"INITIAL, END"}}); status != http.StatusSeeOther {
		t.Fatalf("сохранение питания: %d", status)
	}
	if pw := f.config().Devices["washer"].Profile.Power; pw == nil || !slices.Equal(pw.Idle, []string{"INITIAL", "END"}) || pw.Allowed != "" {
		t.Fatalf("питание в файле: %+v", pw)
	}
	// Камера холодильника - отдельный прибор настроек.
	f.get("/?thinq_id=lg-fridge-1&location=FREEZER")
	if status, _, _ := f.post("/devices", url.Values{"id": {"freezer"}, "name": {"Морозилка"}, "source": {"thinq"},
		"device_id": {"lg-fridge-1"}, "location": {"FREEZER"}}); status != http.StatusSeeOther {
		t.Fatalf("добавление камеры: %d", status)
	}
	if hc, ok := f.gw.HomeConnect("freezer"); !ok || f.config().Devices["freezer"].Location != "FREEZER" ||
		hc.Appliance.Describe().Options["temperatureInUnits.targetTemperatureC"].Min != -24 {
		t.Fatal("морозильная камера не собрана")
	}
	if _, body = f.get("/connections"); !strings.Contains(body, `href="/devices/washer"`) || !strings.Contains(body, `href="/devices/freezer"`) {
		t.Fatal("приборы аккаунта не отмечены как добавленные")
	}
	// Без PAT прибору LG не на чем работать: настройки не сохраняются.
	if status, _, _ := f.post("/connections/thinq", url.Values{"pat_clear": {"1"}, "country": {""}}); status != http.StatusSeeOther {
		t.Fatalf("удаление PAT: %d", status)
	}
	if _, body = f.get("/connections"); !strings.Contains(body, "thinq.pat и thinq.country") || f.config().ThinQ.PAT != "test-pat" {
		t.Fatal("PAT удалён при приборе LG")
	}
}

// stripServer - поддельная лента WLED: состояние при подключении, presets.json и запись команд.
type stripServer struct {
	*httptest.Server
	mu       sync.Mutex
	received []string
}

func newStrip(t *testing.T) *stripServer {
	t.Helper()
	s := &stripServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte(`{"state":{"on":true,"bri":255,"ps":-1,"mainseg":0,"seg":[{"id":0,"col":[[0,0,255]]}]},"info":{"name":"Полка","arch":"esp32"}}`))
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.received = append(s.received, string(data))
			s.mu.Unlock()
		}
	})
	mux.HandleFunc("/presets.json", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"0":{},"2":{"n":"Ночник"},"4":{"n":"Праздник"}}`)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestStrip(t *testing.T) {
	strip := newStrip(t)
	host := strings.TrimPrefix(strip.URL, "http://")
	f := newFixture(t, "devices:\n  shelf:\n    name: Полка\n    source: wled\n    host: \""+host+"\"\n")
	f.writeUser(testPassword)
	f.login()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		<-done
	}()
	go func() {
		defer close(done)
		f.gw.Run(ctx)
	}()

	_, body := f.get("/devices/shelf")
	if !strings.Contains(body, "Ночник") || !strings.Contains(body, "Праздник") || !strings.Contains(body, `name="host"`) {
		t.Fatal("нет пресетов ленты или адреса")
	}
	// Одной сцене - один пресет: ошибка у формы, введённое не теряется.
	status, body, _ := f.post("/devices/shelf/presets", url.Values{"p.0.id": {"2"}, "p.0.scene": {"night"},
		"p.1.id": {"4"}, "p.1.scene": {"night"}, "p.2.id": {""}, "p.2.scene": {""}})
	if status != http.StatusBadRequest || !strings.Contains(body, "сцена night у пресетов 2 и 4") {
		t.Fatalf("сцена дважды: %d", status)
	}
	f.get("/devices/shelf")
	if status, _, _ := f.post("/devices/shelf/presets", url.Values{"p.0.id": {"2"}, "p.0.scene": {"night"},
		"p.1.id": {"4"}, "p.1.scene": {"party"}, "p.2.id": {""}, "p.2.scene": {""}}); status != http.StatusSeeOther {
		t.Fatalf("пресеты: %d", status)
	}
	if p := f.config().Devices["shelf"].Presets; p[2] != "night" || p[4] != "party" || len(p) != 2 {
		t.Fatalf("пресеты в файле: %v", p)
	}

	// Новые пресеты пересоздают ленту, она переподключается; команда цвета уходит ленте.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, ok := f.gw.Strip("shelf"); ok && s.Presets[4] == "party" {
			if online, _, _ := s.Strip(); online {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("лента не переподключилась")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, body = f.get("/devices/shelf")
	if !strings.Contains(body, "Полка, esp32") || !strings.Contains(body, `type="color"`) || !strings.Contains(body, `<option value="party">`) {
		t.Fatal("нет сведений ленты или команд цвета и сцены")
	}
	if status, _, _ := f.post("/devices/shelf/command", url.Values{"type": {"devices.capabilities.color_setting"},
		"instance": {"rgb"}, "value": {"#ff0000"}}); status != http.StatusSeeOther {
		t.Fatalf("цвет: %d", status)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		strip.mu.Lock()
		got := slices.Contains(strip.received, `{"on":true,"seg":[{"col":[[255,0,0]],"fx":0}]}`)
		strip.mu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("команда цвета не дошла: %v", strip.received)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, body := f.get("/"); !strings.Contains(body, "включён, яркость 100 %") {
		t.Fatal("нет состояния ленты в списке")
	}
}

func TestHandler(t *testing.T) {
	f := newFixture(t, "")
	yandex := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Yandex", "1")
		w.WriteHeader(http.StatusTeapot)
	})
	file, err := config.OpenFile(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := file.Current()
	hs := httptest.NewServer(Handler(New(f.auth, f.gw, file, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))), yandex))
	defer hs.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) *http.Response {
		resp, err := client.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	// Яндексу - /v1.0 и /auth, остальное - интерфейсу; корень - список приборов.
	if resp := get("/"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("корень без входа: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := get("/login"); resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("вход: %d", resp.StatusCode)
	}
	for _, path := range []string{"/v1.0", "/v1.0/user/devices", "/auth/authorize"} {
		if resp := get(path); resp.StatusCode != http.StatusTeapot || resp.Header.Get("X-Yandex") != "1" {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	// Старые адреса /ui/... ведут на те же адреса без /ui, но не на чужой сайт.
	for path, want := range map[string]string{"/ui": "/", "/ui/": "/", "/ui/devices/oven?x=1": "/devices/oven?x=1",
		"/ui/%2Fevil.example": "/%2Fevil.example"} {
		if resp := get(path); resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != want {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	resp, err := client.PostForm(hs.URL+"/ui/login", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/login" {
		t.Fatalf("форма со старой страницы: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestCoffee(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	machine := dlfake.NewMachine(cloud)
	defer machine.Close()
	gigya, api, iot := cloud.URLs()
	f := newFixture(t, "delonghi:\n  gigya_url: "+gigya+"\n  api_url: "+api+"\n  iot_url: "+iot+"\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	if !strings.Contains(body, `id="delonghi"`) || !strings.Contains(body, "Longhi не выполнен") {
		t.Fatal("нет раздела De'Longhi")
	}
	if _, _, to := f.post("/connections/delonghi/login", url.Values{"email": {dlfake.Email}, "password": {"wrong"}}); to != "/connections#delonghi" {
		t.Fatalf("неверный пароль: переход %s", to)
	}
	if _, body = f.get("/connections"); !strings.Contains(body, "не принял почту или пароль") {
		t.Fatal("нет ошибки неверного пароля")
	}
	f.post("/connections/delonghi/login", url.Values{"email": {dlfake.Email}, "password": {dlfake.Password}})
	_, body = f.get("/connections")
	if !strings.Contains(body, "Вход выполнен, пул CH") || !strings.Contains(body, `href="/?delonghi_serial=`+dlfake.Serial+`#add"`) ||
		strings.Contains(body, dlfake.Password) {
		t.Fatal("после входа нет машины аккаунта или на странице пароль")
	}
	_, body = f.get("/?delonghi_serial=" + dlfake.Serial)
	if !strings.Contains(body, `<option value="delonghi" selected>`) || !strings.Contains(body, `value="`+dlfake.Serial+`"`) {
		t.Fatal("форма не заполнена машиной аккаунта")
	}
	if _, _, to := f.post("/devices", url.Values{"id": {"coffee"}, "name": {"Кофемашина"}, "source": {"delonghi"},
		"host": {"coffee.local"}, "serial": {dlfake.Serial}}); to != "" {
		t.Fatal("имя вместо IP принято")
	}
	f.get("/")
	if _, _, to := f.post("/devices", url.Values{"id": {"coffee"}, "name": {"Кофемашина"}, "source": {"delonghi"},
		"host": {machine.Host()}, "serial": {dlfake.Serial}}); to != "/devices/coffee" {
		t.Fatalf("добавление: переход %q", to)
	}
	_, body = f.get("/devices/coffee")
	if !strings.Contains(body, `id="pin"`) || !strings.Contains(body, `name="recipe.americano" value="6"`) {
		t.Fatal("нет раздела сертификата или рецептов")
	}
	f.post("/devices/coffee/recipes", url.Values{})
	if _, body = f.get("/devices/coffee"); !strings.Contains(body, "<td>986</td>") || !strings.Contains(body, `<option value="6" label="американо, 986 варок">`) {
		t.Fatal("нет рецептов из облака")
	}
	// Подтверждается только снятый шлюзом отпечаток.
	f.post("/devices/coffee/pin", url.Values{"pin": {machine.Pin()}})
	if f.config().Devices["coffee"].Pin != "" {
		t.Fatal("отпечаток, который шлюз не снимал, подтверждён")
	}
	f.get("/devices/coffee")
	f.post("/devices/coffee", url.Values{"name": {"Кофемашина"}, "host": {machine.Host()}, "serial": {dlfake.Serial},
		"profile_id": {"2"}, "recipe.espresso": {"1"}, "recipe.latte": {"9"}, "default_drink": {"latte"}})
	d := f.config().Devices["coffee"]
	if *d.ProfileID != 2 || d.Recipes["latte"] != 9 || len(d.Recipes) != 2 || d.DefaultDrink != "latte" {
		t.Fatalf("в файле: %+v", d)
	}
	if hc, ok := f.gw.HomeConnect("coffee"); !ok || hc.Profile.DefaultProgram != "DeLonghi.Recipe.9" {
		t.Fatal("рецепты не применены")
	}
}

// Скрипт интерфейса - свой файл без входа; CSP разрешает только его и соединения со шлюзом.
func TestScript(t *testing.T) {
	f := newFixture(t, demoConfig)
	status, body, resp := f.do(http.MethodGet, "/static/app.js", nil)
	if status != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(body, "bindSources") {
		t.Fatalf("скрипт: %d %s", status, resp.Header.Get("Content-Type"))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe-inline'; form") && strings.Contains(csp, "script-src 'unsafe") {
		t.Fatalf("CSP: %s", csp)
	}
	f.writeUser(testPassword)
	f.login()
	_, body = f.get("/")
	for _, want := range []string{`<script src="/static/app.js" defer></script>`, `data-sources="demo"`, `data-sources="delonghi"`,
		`data-sources="homeconnect-cloud homeconnect-local"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("на странице встроенный скрипт: CSP его не пропустит")
	}
}

func TestInfo(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	status, body := f.get("/devices/oven/info")
	for _, want := range []string{`data-stream="/devices/oven/stream"`, "Устройства Алисы", "Значения прибора",
		"<code>Oven</code>", "Действующий профиль", "встроенный"} {
		if status != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("%d, на странице нет %s", status, want)
		}
	}
	if status, _ := f.get("/devices/nope/info"); status != http.StatusNotFound {
		t.Fatalf("чужой прибор: %d", status)
	}
	// Поток SSE сразу присылает снимок состояния.
	req, _ := http.NewRequest(http.MethodGet, f.hs.URL+"/devices/oven/stream", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := f.client.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("тип потока: %s", resp.Header.Get("Content-Type"))
	}
	r := bufio.NewReader(resp.Body)
	line1, _ := r.ReadString('\n')
	line2, _ := r.ReadString('\n')
	if line1 != "event: state\n" || !strings.Contains(line2, `"values":[`) || !strings.Contains(line2, "BSH.Common.Status.OperationState") {
		t.Fatalf("поток: %q %q", line1, line2)
	}
	// Без входа - на страницу входа.
	f.cookie = ""
	if status, _, _ := f.do(http.MethodGet, "/devices/oven/stream", nil); status != http.StatusSeeOther {
		t.Fatalf("поток без входа: %d", status)
	}
}

func TestEditorHelpers(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/devices/oven")
	if !strings.Contains(body, `value="preview">Предпросмотр</button>`) {
		t.Fatal("нет кнопки предпросмотра")
	}
	// «Проверить» у режима отправляет прибору команду, как Алиса.
	m := regexp.MustCompile(`name="probe" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("нет кнопки «Проверить» у режима")
	}
	if _, _, to := f.post("/devices/oven/probe", url.Values{"probe": {html.UnescapeString(m[1])}}); to != "/devices/oven#modes" {
		t.Fatalf("проверка: переход %q", to)
	}
	if _, body = f.get("/devices/oven"); !strings.Contains(body, "выполнена") {
		t.Fatal("команда режима не выполнена")
	}
	f.post("/devices/oven/probe", url.Values{"probe": {"program|nonsense"}})
	if _, body = f.get("/devices/oven"); !strings.Contains(body, "неверный режим") {
		t.Fatal("неверный режим принят")
	}
	// Предпросмотр показывает устройство Алисы, но не сохраняет поле.
	before := f.config().Devices["oven"].Profile
	status, body, _ := f.post("/devices/oven/profile/yandex_type", url.Values{"yandex_type": {"cooking.kettle"}, "op": {"preview"}})
	if status != http.StatusOK || !strings.Contains(body, "Предпросмотр устройства Алисы") || !strings.Contains(body, "devices.types.cooking.kettle") {
		t.Fatalf("предпросмотр: %d", status)
	}
	if f.config().Devices["oven"].Profile != before {
		t.Fatal("предпросмотр сохранил профиль")
	}
}

func TestUnmapped(t *testing.T) {
	p := &profile.Profile{YandexType: "cooking", OnOff: "none", Properties: []profile.Property{{Type: "float", Instance: "temperature", Status: "t"}}}
	st := appliance.State{Values: map[string]any{"t": 20.0, "flag": true, "mode": "A", "alarm": appliance.EventPresent, "num": 3.0}}
	desc := appliance.Description{Options: map[string]appliance.Option{"mode": {Values: []string{"A", "B"}}, "num": {Min: 1, Max: 5}}}
	got := map[string]string{}
	for _, r := range unmapped(&st, p, &desc) {
		got[r.Key] = r.Section
	}
	want := map[string]string{"flag": "toggles", "mode": "modes", "alarm": "events", "num": "ranges"}
	if !maps.Equal(got, want) {
		t.Fatalf("несопоставленные %v", got)
	}
}
