package web

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hcgateway/internal/config"
	"hcgateway/internal/hcapi"
	"hcgateway/internal/hccloud"
	"hcgateway/internal/smartthings/stfake"
)

func TestFlow(t *testing.T) {
	f := flow(true, false, false)
	if !f[0].Done || f[0].Open || !f[1].Open || f[2].Open {
		t.Fatalf("шаги: %+v", f)
	}
	if f := flow(true, true); f[0].Open || f[1].Open {
		t.Fatalf("все сделаны, а шаг раскрыт: %+v", f)
	}
	if done, note := added(2, 5); done || note != "в шлюзе 2 из 5" {
		t.Fatalf("пометка: %v %q", done, note)
	}
	if done, note := added(0, 0); done || note != "" {
		t.Fatal("пустой список сделан")
	}
}

// Карточки подключений с одной главной кнопкой и шаги подключения в разделах: раскрыт
// первый несделанный шаг, сделанный свёрнут и помечен «готово».
func TestConnectionCards(t *testing.T) {
	cloud := stfake.New()
	defer cloud.Close()
	f := newFixture(t, "smartthings:\n  api_url: "+cloud.URL()+"\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	for _, want := range []string{`<div class="conn-grid">`, `<article class="card warn">`, `<a class="button" href="#yandex">Настроить</a>`,
		`<details class="step" id="yandex-app" open>`, `<details class="step" id="yandex-link">`,
		`<details class="step" id="smartthings-app" open>`, `<span class="step-num">2</span>Вход в SmartThings`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	f.post("/connections/smartthings", url.Values{"client_id": {stfake.ClientID}, "client_secret": {stfake.ClientSecret}})
	_, body = f.get("/connections")
	for _, want := range []string{`<details class="step done" id="smartthings-app">`, `<span class="chip run">готово</span><span class="muted">Изменить</span>`,
		`<details class="step" id="smartthings-login" open>`, `<form method="post" action="/connections/smartthings/login"><input type="hidden" name="csrf"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("после настройки SmartThings нет %s", want)
		}
	}
}

// Форма Home Connect подсказывает адрес облака Китая и принимает его без https://.
func TestHomeConnectAddress(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	if _, body := f.get("/connections"); !strings.Contains(body, "для приборов из Китая - https://api.home-connect.cn") {
		t.Fatal("нет подсказки об адресе")
	}
	f.post("/connections/homeconnect", url.Values{"client_id": {"id"}, "client_secret": {"secret"}, "api_url": {"api.home-connect.cn"}})
	if _, body := f.get("/connections"); !strings.Contains(body, "Настройки Home Connect сохранены.") ||
		!strings.Contains(body, `name="api_url" value="https://api.home-connect.cn"`) {
		t.Fatal("адрес без https:// не сохранён")
	}
	if got := f.config().HomeConnect.APIURL; got != "https://api.home-connect.cn" {
		t.Fatalf("адрес в настройках: %s", got)
	}
}

// keysFixture - страница с облаком на подделке: прибор good и токен с идентификатором user-1.
func keysFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","region":"CN"}`)) + ".sig"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/security/oauth/token":
			w.Write([]byte(`{"access_token":"` + jwt + `","refresh_token":"r2","expires_in":86400}`))
		case r.Header.Get("Authorization") != "Bearer "+jwt:
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, "/paired-appliances"):
			w.Write([]byte(`{"appliances":[{"haId":"good","brand":"Bosch","haType":"Oven"}]}`))
		case strings.HasSuffix(r.URL.Path, "/encryption-information"):
			w.Write([]byte(`{"tls":{"key":"AAAA"}}`))
		case !strings.HasPrefix(r.URL.Path, "/api/iddf/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			var buf bytes.Buffer
			z := zip.NewWriter(&buf)
			for name, text := range map[string]string{
				"good_FeatureMapping.xml":    `<m><featureDescription><feature refUID="0201">BSH.Common.Status.DoorState</feature></featureDescription></m>`,
				"good_DeviceDescription.xml": `<device><statusList uid="0010"><status uid="0201" access="read"/></statusList></device>`} {
				f, _ := z.Create(name)
				f.Write([]byte(text))
			}
			z.Close()
			w.Write(buf.Bytes())
		}
	}))
	t.Cleanup(srv.Close)
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	f.gw.Keys = hccloud.NewFlow(f.config().DataDir, f.gw.Cloud.API, nil, slog.New(slog.DiscardHandler), hccloud.Region{Name: "cn", Base: srv.URL, Hosts: []string{srv.URL}})
	return f, jwt
}

// keysLoaded ждёт конец загрузки и проверяет, что прибор можно добавить, а ключа и токена на странице нет.
func keysLoaded(t *testing.T, f *fixture, jwt string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.gw.Keys.Status().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_, body := f.get("/connections")
	for _, want := range []string{`<span class="ok">загружен</span>`, `<a href="/?local_id=good#add">Добавить</a>`, "ключи приборов: 1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	if strings.Contains(body, jwt) || strings.Contains(body, "AAAA") {
		t.Fatal("токен или ключ попал на страницу")
	}
}

// Вход по ссылке: ссылка, отказ на чужой адрес, загрузка, прибор в списке.
func TestKeysLogin(t *testing.T) {
	f, jwt := keysFixture(t)
	if _, body := f.get("/connections"); !strings.Contains(body, `<option value="cn">Китай</option>`) {
		t.Fatal("нет выбора региона")
	}
	if _, _, to := f.post("/connections/local/login", url.Values{"region": {"mars"}}); to != "/connections#local" {
		t.Fatalf("чужой регион: %s", to)
	}
	if _, body := f.get("/connections"); !strings.Contains(body, "Неизвестный регион") {
		t.Fatal("чужой регион принят")
	}
	f.post("/connections/local/login", url.Values{"region": {"cn"}})
	_, body := f.get("/connections")
	m := regexp.MustCompile(`href="([^"]*/oauth/authorize[^"]*)"`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `name="redirect"`) || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("нет ссылки, поля или страница обновляется, пока вставляют адрес")
	}
	link, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	f.post("/connections/local/keys", url.Values{"redirect": {"hcauth://auth/prod?code=CODE&state=wrong"}})
	if _, body = f.get("/connections"); !strings.Contains(body, "Адрес не подошёл") {
		t.Fatal("чужой адрес принят")
	}
	f.post("/connections/local/keys", url.Values{"redirect": {"hcauth://auth/prod?code=CODE&state=" + link.Query().Get("state")}})
	keysLoaded(t, f, jwt)
}

// Загрузка по данным токена и региону из списка.
func TestKeysToken(t *testing.T) {
	f, jwt := keysFixture(t)
	f.get("/connections")
	f.post("/connections/local/token", url.Values{"region": {"cn"}, "token": {"{ access_token: '<token>' }"}})
	if _, body := f.get("/connections"); !strings.Contains(body, "В данных нет токена") || !strings.Contains(body, `<option value="">Определить по токену</option>`) {
		t.Fatal("заглушка принята или нет выбора «по токену»")
	}
	f.post("/connections/local/token", url.Values{"region": {"mars"}, "token": {jwt}})
	if _, body := f.get("/connections"); !strings.Contains(body, "Неизвестный регион") {
		t.Fatal("чужой регион принят")
	}
	data := "{\n  access_token: '" + jwt + "',\n  refresh_token: 'secret-refresh',\n  expires_in: 86400\n}"
	if _, _, to := f.post("/connections/local/token", url.Values{"region": {"cn"}, "token": {data}}); to != "/connections#local" {
		t.Fatalf("переход: %s", to)
	}
	keysLoaded(t, f, jwt)
	_, body := f.get("/connections")
	for _, want := range []string{"Токен сохранён: регион Китай", "Обновить ключи", "Продлить сейчас", "Вход по токену приложения Home Connect"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	if strings.Contains(body, "secret-refresh") {
		t.Fatal("refresh token попал на страницу")
	}
	f.post("/connections/local/refresh", url.Values{})
	if _, body = f.get("/connections"); !strings.Contains(body, "Токен продлён.") {
		t.Fatal("токен не продлён")
	}
	if _, _, to := f.post("/connections/local/update", url.Values{}); to != "/connections#local" {
		t.Fatalf("обновление ключей: %s", to)
	}
	keysLoaded(t, f, jwt)
	if _, body = f.get("/connections/local/logout"); !strings.Contains(body, "В облаке токен не отзывается") {
		t.Fatal("нет подтверждения выхода")
	}
	if _, _, to := f.post("/connections/local/logout", url.Values{}); to != "/connections#local" {
		t.Fatalf("выход: %s", to)
	}
	if f.gw.Cloud.API.LoggedIn() {
		t.Fatal("токены не удалены")
	}
	f.post("/connections/local/update", url.Values{})
	if _, body = f.get("/connections"); !strings.Contains(body, "Сохранённого токена нет") {
		t.Fatal("обновление без токена")
	}
}

// Проверка приложения и региона токена: понятный отказ до запросов к облаку.
func TestKeysTokenChecks(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	// У регионов нет адресов: запрос к облаку уронил бы проверку итога.
	f.gw.Keys = hccloud.NewFlow(f.dir, f.gw.Cloud.API, nil, slog.New(slog.DiscardHandler), hccloud.Region{Name: "eu"}, hccloud.Region{Name: "cn"})
	f.get("/connections")
	token := func(claims string) string {
		return "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","exp":4102444800`+claims+`}`)) + ".sig"
	}
	for _, c := range []struct{ region, claims, want string }{
		{"cn", `,"azp":"portal"`, "Токен выдан другому приложению"},
		{"eu", `,"region":"CN"`, "Токен выдан для региона «Китай», а выбран «Европа»"},
		{"", "", "Шлюз не смог определить регион по токену"},
		{"", `,"region":"XX"`, "Шлюз не смог определить регион по токену"},
	} {
		f.post("/connections/local/token", url.Values{"region": {c.region}, "token": {token(c.claims)}})
		if _, body := f.get("/connections"); !strings.Contains(html.UnescapeString(body), c.want) {
			t.Errorf("%s %q: на странице нет %q", c.region, c.claims, c.want)
		}
		if f.gw.Cloud.API.LoggedIn() || f.gw.Keys.Status().Running {
			t.Fatal("отвергнутый токен принят")
		}
	}
}

// Регион из токена: шлюз сообщает, какой взял, и загружает ключи.
func TestKeysTokenRegionFromToken(t *testing.T) {
	f, jwt := keysFixture(t)
	f.get("/connections")
	f.post("/connections/local/token", url.Values{"region": {""}, "token": {jwt}})
	if _, body := f.get("/connections"); !strings.Contains(body, "Токен принят: регион Китай") {
		t.Fatal("нет сообщения о регионе")
	}
	keysLoaded(t, f, jwt)
}

// Без входа прибор облака называет причину, а страница подсказывает, что сделать.
func TestCloudDeviceWithoutLogin(t *testing.T) {
	f := newFixture(t, "devices:\n  oven:\n    name: Духовка\n    source: homeconnect-cloud\n    ha_id: BOSCH-OVEN-1\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	if !strings.Contains(body, "Нет входа в Home Connect: приборы облака не на связи.") {
		t.Fatal("нет шага о входе")
	}
	if _, body = f.get("/"); !strings.Contains(body, "нет входа в Home Connect") {
		t.Fatal("метка прибора не называет причину")
	}
}

// Облако отвергло токен: раздел ключей и «Что ещё настроить» говорят об этом, сбой продления виден.
func TestRefreshProblemsOnPage(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	api := f.gw.Cloud.API
	if err := api.UseTokens(hcapi.Tokens{Access: "a", Refresh: "r", ClientID: "app", BaseURL: srv.URL, Region: "cn",
		Expires: time.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, body := f.get("/connections")
	if !strings.Contains(body, "действует ещё 2 ч") && !strings.Contains(body, "действует ещё 1 ч") {
		t.Fatal("нет срока токена")
	}
	f.get("/connections")
	f.post("/connections/local/refresh", url.Values{})
	if _, body = f.get("/connections"); !strings.Contains(body, "Токен не продлён") || !strings.Contains(body, "Последнее продление не удалось только что") {
		t.Fatal("сбой продления не виден")
	}
	status.Store(http.StatusBadRequest)
	f.post("/connections/local/refresh", url.Values{})
	_, body = f.get("/connections")
	for _, want := range []string{"Облако не продлило токен приложения Home Connect", "Облако не продлило токен Home Connect: нужен новый вход или новый токен."} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
}

// Прибор с тем же haId до суффикса и другим источником - только предупреждение.
func TestTwinDevice(t *testing.T) {
	cfg := &config.Config{Devices: map[string]config.Device{
		"oven":  {HaID: "385000-001", Source: config.SourceHomeConnectCloud},
		"other": {HaID: "999", Source: config.SourceHomeConnectLocal},
	}}
	if got := twinDevice(cfg, "385000", config.SourceHomeConnectLocal); got != "oven" {
		t.Errorf("двойник: %q", got)
	}
	if got := twinDevice(cfg, "385000-001", config.SourceHomeConnectCloud); got != "" {
		t.Errorf("тот же источник: %q", got)
	}
	if got := twinDevice(cfg, "111", config.SourceHomeConnectCloud); got != "" {
		t.Errorf("другой прибор: %q", got)
	}
}
