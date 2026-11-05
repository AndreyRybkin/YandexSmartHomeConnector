package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/config"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/i18n"
	"hcgateway/internal/profile"
)

// Значения, которые шлюз читает сам, не попадают в «Не сопоставлено»; питание Home Connect
// предлагается в раздел «включи».
func TestUnmappedImplicit(t *testing.T) {
	p := &profile.Profile{YandexType: "cooking", OnOff: "program", Toggles: []profile.Toggle{{Instance: "pause"}}}
	st := appliance.State{Values: map[string]any{
		appliance.StatusOperationState:     appliance.OperationReady,
		appliance.StatusRemoteStartAllowed: true,
		appliance.StatusDoorState:          appliance.DoorClosed,
		appliance.SettingPowerState:        appliance.PowerOn,
	}}
	desc := appliance.Description{Options: map[string]appliance.Option{appliance.SettingPowerState: {Values: []string{appliance.PowerOn}}}}
	rows := unmapped(&st, p, &desc)
	if len(rows) != 1 || rows[0].Key != appliance.SettingPowerState || rows[0].Section != "on_off" {
		t.Fatalf("несопоставленные %+v", rows)
	}
	m := mappings(p)
	if !strings.HasPrefix(m[appliance.StatusOperationState], "on_off") || !strings.HasPrefix(m[appliance.StatusDoorState], "шлюз") {
		t.Fatalf("сопоставления %v", m)
	}
}

func TestSlug(t *testing.T) {
	for name, want := range map[string]string{"Кофемашина": "kofemashina", "Духовка на кухне": "dukhovka-na-kukhne",
		"LG Стиралка #2": "lg-stiralka-2", "Объём": "obem", "!!!": "device", "  Ёлка--Щука  ": "elka-shchuka", strings.Repeat("я", 40): strings.Repeat("ya", 32)} {
		if got := slug(name); got != want {
			t.Errorf("slug(%q) = %q, нужно %q", name, got, want)
		}
	}
	if got := freeID(map[string]config.Device{"kofemashina": {}, "kofemashina-2": {}}, "Кофемашина"); got != "kofemashina-3" {
		t.Errorf("freeID = %q", got)
	}
}

const roomsConfig = `devices:
  oven:
    name: Духовка
    room: Кухня
    source: demo
    demo: Oven
  dish:
    name: Посудомойка
    source: demo
    demo: Dishwasher
`

// Список приборов: комнаты, состояние словами, кнопка питания и идентификатор из имени.
func TestDevicesList(t *testing.T) {
	f := newFixture(t, roomsConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/")
	for _, want := range []string{"<h2>Кухня</h2>", "<h2>Без комнаты</h2>", `<article class="tile idle" id="d-oven">`,
		`<span class="chip idle">готов</span>`, "демо-прибор", `<use href="#i-oven"/>`,
		`<button class="switch" name="value" value="true" role="switch" aria-checked="false">`, `data-stream="/stream"`,
		`<details class="field" id="add">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("в списке нет %s", want)
		}
	}
	if strings.Index(body, "Кухня") > strings.Index(body, "Без комнаты") {
		t.Fatal("приборы без комнаты не последние")
	}
	form := url.Values{"type": {"devices.capabilities.on_off"}, "instance": {"on"}, "value": {"true"}, "from": {"list"}}
	if _, _, to := f.post("/devices/oven/command", form); to != "/#d-oven" {
		t.Fatalf("кнопка списка: переход %q", to)
	}
	if _, body = f.get("/"); !strings.Contains(body, `<span class="chip run">работает, Выпечка`) ||
		!strings.Contains(body, `<button class="switch on" name="value" value="false" role="switch" aria-checked="true">`) {
		t.Fatal("после включения нет состояния словами или кнопки «Выключить»")
	}
	// Пустой идентификатор - из имени, занятый - с номером.
	for _, want := range []string{"kofemashina", "kofemashina-2"} {
		f.get("/")
		if _, _, to := f.post("/devices", url.Values{"name": {"Кофемашина"}, "source": {"demo"}, "demo": {"Oven"}}); to != "/devices/"+want {
			t.Fatalf("добавление без идентификатора: переход %q", to)
		}
	}
}

// Поток списка приборов сразу присылает состояние всех приборов.
func TestListStream(t *testing.T) {
	f := newFixture(t, roomsConfig)
	f.writeUser(testPassword)
	f.login()
	req, _ := http.NewRequest(http.MethodGet, f.hs.URL+"/stream", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := f.client.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	line1, _ := r.ReadString('\n')
	line2, _ := r.ReadString('\n')
	if line1 != "event: list\n" || !strings.Contains(line2, `"id":"oven"`) || !strings.Contains(line2, `"switch":true`) ||
		strings.Contains(line2, "Духовка") {
		t.Fatalf("поток: %q %q", line1, line2)
	}
}

// Отвязка, выходы и удаление прибора - через страницу подтверждения.
func TestConfirm(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	for path, want := range map[string]string{
		"/connections/yandex/unlink":      "Отвязать аккаунт Яндекса?",
		"/connections/homeconnect/logout": "Выйти из Home Connect?",
		"/connections/delonghi/logout":    "Выйти из De&#39;Longhi?",
		"/devices/oven/delete":            "Удалить «Духовка»?",
	} {
		status, body := f.get(path)
		if status != http.StatusOK || !strings.Contains(body, want) || !strings.Contains(body, `<form method="post" action="`+path+`">`) {
			t.Fatalf("%s: %d", path, status)
		}
	}
	_, body := f.get("/connections")
	if !strings.Contains(body, `<form method="get" action="/connections/delonghi/logout"`) && strings.Contains(body, "Вход выполнен") {
		t.Fatal("выход из De'Longhi без подтверждения")
	}
}

// «Подключения»: сводка, свёрнутые разделы без настроек и адреса для консоли навыка.
func TestConnectionsPage(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	host := strings.TrimPrefix(f.hs.URL, "http://")
	for _, want := range []string{`<a href="#yandex">Яндекс</a>`, `<details class="field" id="thinq">`, `<details class="field" id="delonghi">`,
		"<code data-copy>https://" + host + "/auth/authorize</code>", "<code data-copy>https://" + host + "</code>",
		`<p data-generate hidden>`, `Идентификатор приложения <span class="tech">client_id</span>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	if gatewayHost(&http.Request{Host: `x" onmouseover="1`}) != "<адрес шлюза>" {
		t.Fatal("странный адрес шлюза не заменён заглушкой")
	}
}

// Профили типов: по умолчанию - только типы с приборами или переопределениями.
func TestProfilesFilter(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/profiles")
	if !strings.Contains(body, "Духовой шкаф") || strings.Contains(body, `href="/profiles/Dishwasher"`) || !strings.Contains(body, `href="/profiles?all=1"`) {
		t.Fatal("в списке нет своего типа, есть чужой или нет ссылки «Показать все»")
	}
	if _, body = f.get("/profiles?all=1"); !strings.Contains(body, `href="/profiles/Dishwasher"`) {
		t.Fatal("«Показать все» не показывает все типы")
	}
}

// Страница прибора - вкладки; ошибка формы открывает вкладку своего раздела.
func TestDeviceTabs(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/devices/oven")
	for _, want := range []string{`<nav class="tabs" data-tabs>`, `id="tab-device"`, `id="tab-state"`, `id="tab-map"`,
		"Режим «Программа»", "Выпечка (baking)", "Включение: <b>выключено</b>", " °C "} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	form := url.Values{"op": {"save"}, "m.0.instance": {"program"}, "m.0.source": {"programs"},
		"m.0.k.0": {"Cooking.Oven.Program.HeatingMode.HotAir"}, "m.0.v.0": {"one"},
		"m.0.k.1": {"Cooking.Oven.Program.HeatingMode.PizzaSetting"}, "m.0.v.1": {"one"}}
	if _, body, _ = f.post("/devices/oven/profile/modes", form); !strings.Contains(body, `data-active="tab-map" data-focus="modes"`) {
		t.Fatal("ошибка формы не открывает вкладку сопоставлений")
	}
	_, body = f.get("/devices/oven/info")
	if !strings.Contains(body, "от 30 до 275 °C, шаг 5") || !strings.Contains(body, "шлюз: разрешён ли удалённый старт") {
		t.Fatal("на «Всё о приборе» нет числа Алисы словами или пояснения значения шлюза")
	}
}

// Состояние одной строкой - словами: режим кофемашины, состояние LG и программа режимом Алисы.
func TestSummary(t *testing.T) {
	p := &profile.Profile{Power: &profile.Power{State: delonghi.StatusMode},
		Modes: []profile.Mode{{Instance: "coffee_mode", Programs: map[string]string{"DeLonghi.Recipe.6": "americano"}}}}
	st := appliance.State{Online: true, ActiveProgram: "DeLonghi.Recipe.6", Values: map[string]any{delonghi.StatusMode: 7.0}}
	if got := summary(&st, p); got != "готова, Американо" {
		t.Fatalf("кофемашина: %q", got)
	}
	p = &profile.Profile{Power: &profile.Power{State: "runState.currentState"}}
	st = appliance.State{Online: true, Values: map[string]any{"runState.currentState": "RUNNING"}}
	if got := summary(&st, p); got != "работает" {
		t.Fatalf("LG: %q", got)
	}
	st = appliance.State{Online: true, ActiveProgram: "Dishcare.Dishwasher.Program.Eco50", Values: map[string]any{
		appliance.StatusOperationState: appliance.OperationRun, "BSH.Common.Option.RemainingProgramTime": 5100.0}}
	if got := summary(&st, profile.BuiltinProfile("Dishwasher")); got != "работает, Экономичный, осталось 1 ч 25 мин" {
		t.Fatalf("посудомойка: %q", got)
	}
	for sec, want := range map[float64]string{0: "0 мин", 59: "1 мин", 3600: "1 ч", 5101: "1 ч 26 мин"} {
		if got := hcdevice.Duration(sec); got != want {
			t.Errorf("Duration(%v) = %q, нужно %q", sec, got, want)
		}
	}
}

// Своё свойство: своё число с пересчётом, своя формула и своё событие; что не из документации
// Яндекса, редактор помечает.
func TestCustomProperty(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	f.get("/devices/oven")
	form := url.Values{"op": {"save"},
		"p.0.kind": {"float/*"}, "p.0.instance": {"my_time"}, "p.0.status": {"Cooking.Oven.Status.CurrentCavityTemperature"},
		"p.0.unit": {"unit.time.seconds"}, "p.0.convert": {"minutes_to_seconds"},
		"p.1.kind": {"float/temperature"}, "p.1.status": {"Cooking.Oven.Status.CurrentCavityTemperature"},
		"p.1.unit": {"unit.temperature.celsius"}, "p.1.convert": {"custom"}, "p.1.scale": {"0,1"}, "p.1.offset": {"5"}, "p.1.min": {"0"}, "p.1.max": {"99,5"},
		"p.2.kind": {"event/*"}, "p.2.instance": {"my_door"}, "p.2.status": {"BSH.Common.Status.DoorState"},
		"p.2.k.0": {"BSH.Common.EnumType.DoorState.Open"}, "p.2.v.0": {"wide_open"},
		"p.3.kind": {"float/water_level"}, "p.3.status": {"Cooking.Oven.Status.CurrentCavityTemperature"},
		"p.3.unit": {"unit.percent"}, "p.3.convert": {"weight_to_percent"}, "p.3.volume": {"1,5"}, "p.4.kind": {""}}
	if status, _, _ := f.post("/devices/oven/profile/properties", form); status != http.StatusSeeOther {
		t.Fatalf("сохранение: %d", status)
	}
	props := f.config().Devices["oven"].Profile.Properties
	if len(props) != 4 || props[3].Convert != profile.WeightToPercent || *props[3].Volume != 1.5 || props[0].Instance != "my_time" || props[0].Convert != "minutes_to_seconds" ||
		*props[1].Scale != 0.1 || *props[1].Offset != 5 || *props[1].Min != 0 || *props[1].Max != 99.5 || props[2].Values["BSH.Common.EnumType.DoorState.Open"] != "wide_open" {
		t.Fatalf("свойства в файле: %+v", props)
	}
	_, body := f.get("/devices/oven")
	for _, want := range []string{`<option value="float/*" selected>Своё число (float)</option>`, `name="p.0.instance" value="my_time"`,
		"В документации Яндекса нет instance my_time и единицы unit.time.seconds", `<option value="custom" selected>своя формула</option>`, `name="p.1.min" value="0"`, `name="p.1.max" value="99.5"`, `name="p.3.volume" value="1.5"`,
		`name="p.1.scale" value="0.1"`, `name="p.2.instance" value="my_door"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	// Пересчёт виден в «Кто берёт» на «Всё о приборе».
	if _, body = f.get("/devices/oven/info"); !strings.Contains(body, "float my_time, минуты → секунды, ×60") {
		t.Fatal("нет пересчёта в «Кто берёт»")
	}
	f.get("/devices/oven")
	form.Set("p.1.scale", "много")
	if status, body, _ := f.post("/devices/oven/profile/properties", form); status != http.StatusBadRequest || !strings.Contains(body, "множитель") {
		t.Fatalf("неверный множитель: %d", status)
	}
}

// Тема: шлюз ставит тему из cookie в разметку, форма темы запоминает её и возвращает только
// на свою страницу.
func TestTheme(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	if _, body := f.get("/profiles"); !strings.Contains(body, `<html lang="ru"><head>`) ||
		!strings.Contains(body, `<option value="" selected>Тема: как в системе</option>`) || !strings.Contains(body, `name="back" value="/profiles"`) {
		t.Fatal("без cookie тема не как в системе или нет адреса возврата")
	}
	status, _, resp := f.do(http.MethodPost, "/theme", url.Values{"csrf": {f.csrf}, "theme": {"dark"}, "back": {"/profiles"}})
	var theme *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == themeCookie {
			theme = c
		}
	}
	if status != http.StatusSeeOther || resp.Header.Get("Location") != "/profiles" || theme == nil || theme.Value != "dark" || theme.MaxAge <= 0 {
		t.Fatalf("выбор темы: %d %s %+v", status, resp.Header.Get("Location"), theme)
	}
	req, _ := http.NewRequest(http.MethodGet, f.hs.URL+"/login", nil)
	req.AddCookie(theme)
	r, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(data), `<html lang="ru" data-theme="dark">`) {
		t.Fatal("тема из cookie не в разметке")
	}
	for back, want := range map[string]string{"//evil.example/": "/", "https://evil.example/": "/", "/devices/oven?x=1": "/devices/oven?x=1",
		"/v1.0/user/devices": "/", "/auth/authorize": "/", "\\\\evil.example": "/"} {
		if got := localPath(back); got != want {
			t.Errorf("localPath(%q) = %q, нужно %q", back, got, want)
		}
	}
	_, _, resp = f.do(http.MethodPost, "/theme", url.Values{"csrf": {f.csrf}, "theme": {""}})
	if c := resp.Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("тема как в системе не убрала cookie: %+v", c)
	}
}

// Значки и описание приложения - без входа и с верным типом; CSP пускает их только с шлюза.
func TestStatic(t *testing.T) {
	f := newFixture(t, demoConfig)
	for name, typ := range map[string]string{"manifest.webmanifest": "application/manifest+json", "icon.svg": "image/svg+xml",
		"icon-192.png": "image/png", "icon-512.png": "image/png", "apple-touch-icon.png": "image/png"} {
		status, body, resp := f.do(http.MethodGet, "/static/"+name, nil)
		if status != http.StatusOK || resp.Header.Get("Content-Type") != typ || len(body) == 0 {
			t.Fatalf("%s: %d %s", name, status, resp.Header.Get("Content-Type"))
		}
	}
	_, body, resp := f.do(http.MethodGet, "/static/manifest.webmanifest", nil)
	if !strings.Contains(body, `"display": "standalone"`) || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "manifest-src 'self'") {
		t.Fatal("описание приложения или CSP")
	}
	if status, _, _ := f.do(http.MethodGet, "/static/../server.go", nil); status == http.StatusOK {
		t.Fatal("отдан файл не из списка")
	}
	if status, _, _ := f.do(http.MethodGet, "/static/web_test.go", nil); status != http.StatusNotFound {
		t.Fatalf("чужой файл: %d", status)
	}
}

// Лишнего текста нет: на странице прибора имена Яндекса - в подсказках, нет дублей с «Всё о
// приборе» и пометок встроенного профиля, плашка о шагах настройки - только в списке приборов.
func TestQuietPages(t *testing.T) {
	f := newFixture(t, demoConfig+"  shelf:\n    name: Полка\n    source: wled\n    host: 127.0.0.1:1\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/devices/oven")
	for _, want := range []string{`<span title="on_off on: false">Включение: <b>выключено</b></span>`, `<option value="baking">Выпечка</option>`,
		`<option value="Oven" selected>Духовой шкаф</option>`, "Источник: демо-прибор</p>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице прибора нет %s", want)
		}
	}
	for _, extra := range []string{"Значения прибора", "обновляется сам", "встроенный профиль", "Изменения сохраняются в",
		"<h2>Сопоставления</h2>", "Не сделано шагов настройки"} {
		if strings.Contains(body, extra) {
			t.Fatalf("на странице прибора лишнее: %s", extra)
		}
	}
	if _, body = f.get("/"); !strings.Contains(body, "Не сделано шагов настройки: 2") || strings.Contains(body, "· oven") ||
		!strings.Contains(body, `<option value="demo">Демо-прибор в памяти шлюза</option>`) {
		t.Fatal("в списке приборов нет строки о шагах, есть идентификатор на плитке или имя источника в скобках")
	}
	// Лента без связи: таблица состояния скрыта, о связи - одна строка у пресетов.
	_, body = f.get("/devices/shelf")
	if !strings.Contains(body, `<div class="wrap" hidden>`) || strings.Contains(body, "DEVICE_UNREACHABLE") ||
		!strings.Contains(body, "Лента недоступна: видны только пресеты из настроек.") || strings.Contains(body, "Нет связи с лентой") {
		t.Fatal("лента без связи: лишние строки")
	}
}

// Названия рецептов: в строке режима, в состоянии прибора и в списке из облака. Номер без названия остаётся номером.
func TestRecipeNames(t *testing.T) {
	if got := programShort(delonghi.ProgramPrefix + "15"); got != "рецепт 15 - Капучино, сначала молоко" {
		t.Fatalf("строка режима: %s", got)
	}
	if got := programShort(delonghi.ProgramPrefix + "50"); got != "рецепт 50" {
		t.Fatalf("номер без названия: %s", got)
	}
	p := &profile.Profile{Modes: []profile.Mode{{Instance: "coffee_mode", Programs: map[string]string{delonghi.ProgramPrefix + "6": "americano"}}}}
	for program, want := range map[string]string{"6": nameOr(modeValueNames, "americano"), "8": "Латте макиато", "50": "рецепт 50"} {
		if got := programLabel(p, delonghi.ProgramPrefix+program); got != want {
			t.Fatalf("состояние, рецепт %s: %s", program, got)
		}
	}
	if got := i18n.Translate(i18n.EN, programShort(delonghi.ProgramPrefix+"8")); got != "recipe 8 - Latte macchiato" {
		t.Fatalf("английский текст: %s", got)
	}

	f := newFixture(t, "devices:\n  coffee:\n    name: Кофемашина\n    source: delonghi\n    serial: TESTSERIAL1\n    host: 127.0.0.1\n")
	f.writeUser(testPassword)
	f.login()
	f.ui.statsMu.Lock()
	f.ui.stats = map[string][]delonghi.RecipeStat{"coffee": {{ID: 6, Brew: 3}, {ID: 8, Brew: 2}, {ID: 50, Brew: 1}}}
	f.ui.statsMu.Unlock()
	_, body := f.get("/devices/coffee")
	for _, want := range []string{"<td>8</td><td>Латте макиато</td><td>2</td><td></td>", "<td>50</td><td></td><td>1</td>",
		`label="Латте макиато, 2 варок"`, `label="1 варок"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
}
