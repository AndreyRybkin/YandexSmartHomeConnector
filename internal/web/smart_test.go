package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"hcgateway/internal/config"
	"hcgateway/internal/smartthings/stfake"
)

var stLoginRe = regexp.MustCompile(`href="([^"]+/oauth/authorize[^"]+)"`)

// Вход в SmartThings: ссылка со state, возврат без сессии, список устройств и добавление
// устройства и камеры по ссылке «Добавить».
func TestSmartThings(t *testing.T) {
	cloud := stfake.New()
	defer cloud.Close()
	f := newFixture(t, "smartthings:\n  api_url: "+cloud.URL()+"\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	for _, want := range []string{`id="smartthings"`, "Задайте идентификатор и секрет приложения API Access",
		"/connections/smartthings/callback</code>", "r:devices:* x:devices:* r:locations:* sse"} {
		if !strings.Contains(body, want) {
			t.Fatalf("нет %q на странице", want)
		}
	}
	if _, _, to := f.post("/connections/smartthings", url.Values{"client_id": {stfake.ClientID}, "client_secret": {stfake.ClientSecret}}); to != "/connections#smartthings" {
		t.Fatalf("сохранение: переход %s", to)
	}
	if c := f.config(); c.SmartThings.ClientID != stfake.ClientID || c.SmartThings.ClientSecret != stfake.ClientSecret {
		t.Fatalf("настройки: %+v", c.SmartThings)
	}
	_, body = f.get("/connections")
	if strings.Contains(body, stfake.ClientSecret) || !strings.Contains(body, "Вход в SmartThings не выполнен") {
		t.Fatal("секрет на странице или нет приглашения войти")
	}
	if _, _, to := f.post("/connections/smartthings/login", url.Values{}); to != "/connections#smartthings" {
		t.Fatalf("вход: переход %s", to)
	}
	_, body = f.get("/connections")
	m := stLoginRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("нет ссылки на страницу входа")
	}
	login, _ := url.Parse(html.UnescapeString(m[1]))
	state := login.Query().Get("state")
	if login.Query().Get("redirect_uri") != "https://"+strings.TrimPrefix(f.hs.URL, "http://")+"/connections/smartthings/callback" || state == "" {
		t.Fatalf("страница входа: %s", login)
	}
	// Возврат приходит без cookie сессии: его подтверждает только state.
	callback := func(state string) (int, string) {
		resp, err := http.Get(f.hs.URL + "/connections/smartthings/callback?" + url.Values{"state": {state}, "code": {stfake.Code}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	if status, _ := callback("чужой"); status != http.StatusNotFound {
		t.Fatalf("чужой state: %d", status)
	}
	if status, text := callback(state); status != http.StatusOK || !strings.Contains(text, "Вход в SmartThings выполнен") {
		t.Fatalf("возврат: %d %s", status, text)
	}
	if status, _ := callback(state); status != http.StatusNotFound {
		t.Fatalf("state второй раз: %d", status)
	}
	f.get("/connections")
	if _, _, to := f.post("/connections/smartthings/devices", url.Values{}); to != "/connections#smartthings" {
		t.Fatalf("список: переход %s", to)
	}
	_, body = f.get("/connections")
	for _, want := range []string{`href="/?st_id=tv-1&amp;component=main#add"`, "Холодильник - freezer", "Телевизор (SmartThings)",
		"Запросов к SmartThings:"} {
		if !strings.Contains(body, want) {
			t.Fatalf("нет %q в списке устройств", want)
		}
	}
	_, body = f.get("/?st_id=tv-1&component=main")
	if !strings.Contains(body, `<option value="smartthings" selected>`) || !strings.Contains(body, `<option value="tv-1" selected>`) {
		t.Fatal("форма не заполнена устройством SmartThings")
	}
	if _, _, to := f.post("/devices", url.Values{"name": {"Телевизор"}, "source": {"smartthings"}, "device_id": {"tv-1"},
		"location": {"main"}}); to != "/devices/televizor" {
		t.Fatalf("добавление: переход %s", to)
	}
	f.get("/")
	if _, _, to := f.post("/devices", url.Values{"name": {"Морозилка"}, "source": {"smartthings"}, "device_id": {"fridge-1"},
		"location": {"freezer"}}); to != "/devices/morozilka" {
		t.Fatalf("добавление камеры: переход %s", to)
	}
	c := f.config()
	if d := c.Devices["televizor"]; d.Source != config.SourceSmartThings || d.DeviceID != "tv-1" || d.Location != "" {
		t.Fatalf("телевизор: %+v", d)
	}
	if d := c.Devices["morozilka"]; d.Location != "freezer" {
		t.Fatalf("камера: %+v", d)
	}
	if status, body := f.get("/devices/televizor"); status != http.StatusOK || !strings.Contains(body, "Источник: SmartThings") {
		t.Fatalf("страница прибора: %d", status)
	}
	if status, _ := f.get("/profiles/SmartThings.Television"); status != http.StatusOK {
		t.Fatalf("профиль типа SmartThings: %d", status)
	}
	if _, body = f.get("/connections/smartthings/logout"); !strings.Contains(body, "Выйти из SmartThings?") {
		t.Fatal("нет подтверждения выхода")
	}
	if _, _, to := f.post("/connections/smartthings/logout", url.Values{}); to != "/connections#smartthings" || f.gw.SmartThings.API.LoggedIn() {
		t.Fatalf("выход: переход %s", to)
	}
}

// Телевизор в домашней сети: только IP, MAC приводится к одному виду, страница прибора
// показывает связь и кнопки.
func TestTVDevice(t *testing.T) {
	f := newFixture(t, "")
	f.writeUser(testPassword)
	f.login()
	f.get("/")
	if status, body, _ := f.post("/devices", url.Values{"name": {"Телевизор"}, "source": {"webos"}, "host": {"tv.local"}}); status != http.StatusBadRequest ||
		!strings.Contains(body, "нужен IP телевизора") {
		t.Fatalf("имя вместо IP: %d", status)
	}
	if _, _, to := f.post("/devices", url.Values{"name": {"Телевизор"}, "source": {"webos"}, "host": {"127.0.0.1"},
		"mac": {"aa-bb-cc-dd-ee-ff"}, "broadcast": {"192.168.1.255"}, "device_id": {"чужое"}}); to != "/devices/televizor" {
		t.Fatalf("добавление: переход %s", to)
	}
	if d := f.config().Devices["televizor"]; d.MAC != "AA:BB:CC:DD:EE:FF" || d.Broadcast != "192.168.1.255" || d.DeviceID != "" {
		t.Fatalf("телевизор: %+v", d)
	}
	_, body := f.get("/devices/televizor")
	for _, want := range []string{`id="tv"`, "Телевизор ещё не разрешил подключение", "AA:BB:CC:DD:EE:FF", "/tv/retry"} {
		if !strings.Contains(body, want) {
			t.Fatalf("нет %q на странице телевизора", want)
		}
	}
	if _, _, to := f.post("/devices/televizor/tv/retry", url.Values{}); to != "/devices/televizor#tv" {
		t.Fatalf("подключить заново: переход %s", to)
	}
	f.get("/")
	if status, body, _ := f.post("/devices", url.Values{"name": {"Samsung"}, "source": {"samsung-tv"}, "host": {"127.0.0.2"},
		"mac": {"zz"}}); status != http.StatusBadRequest || !strings.Contains(body, "mac") {
		t.Fatalf("неверный MAC: %d", status)
	}
}

// Приложения телевизора LG: поля есть только у webOS, значения проверяет сервер, пустое поле убирает режим.
func TestTVAppsForm(t *testing.T) {
	f := newFixture(t, "devices:\n  tv:\n    name: Телевизор\n    source: webos\n    host: 127.0.0.1\n"+
		"  sam:\n    name: Самсунг\n    source: samsung-tv\n    host: 127.0.0.2\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/devices/tv")
	if !strings.Contains(body, `name="app.ten"`) {
		t.Fatal("нет полей приложений")
	}
	if _, body := f.get("/devices/sam"); strings.Contains(body, `name="app.one"`) {
		t.Fatal("поля приложений у Samsung")
	}
	f.get("/devices/tv")
	form := url.Values{"name": {"Телевизор"}, "host": {"127.0.0.1"}, "section": {"basic"}, "app.one": {"example.app.one"}, "app.three": {" example.app.three "}}
	if _, _, to := f.post("/devices/tv", form); !strings.HasPrefix(to, "/devices/tv") {
		t.Fatalf("сохранение: переход %s", to)
	}
	if a := f.config().Devices["tv"].Apps; len(a) != 2 || a["one"] != "example.app.one" || a["three"] != "example.app.three" {
		t.Fatalf("приложения: %v", a)
	}
	f.get("/devices/tv")
	form.Del("version")
	form.Set("app.one", "example/../app")
	if status, body, _ := f.post("/devices/tv", form); status != http.StatusBadRequest || !strings.Contains(body, "apps.one") || !strings.Contains(body, `name="app.one" value="example`) {
		t.Fatalf("неверный идентификатор: %d", status)
	}
	if a := f.config().Devices["tv"].Apps; a["one"] != "example.app.one" {
		t.Fatalf("сохранён неверный идентификатор: %v", a)
	}
	f.get("/devices/tv")
	form.Del("version")
	form.Del("app.one")
	form.Del("app.three")
	if _, _, to := f.post("/devices/tv", form); !strings.HasPrefix(to, "/devices/tv") {
		t.Fatalf("очистка: переход %s", to)
	}
	if a := f.config().Devices["tv"].Apps; a != nil {
		t.Fatalf("приложения после очистки: %v", a)
	}
}
