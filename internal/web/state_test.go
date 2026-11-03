package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/mqttbroker"
	"hcgateway/internal/statedb"
	"hcgateway/internal/tv"
)

const stateDevices = `
devices:
  oven:
    name: Духовка
    source: demo
    demo: Oven
  strip:
    name: Лента
    source: wled
    host: 127.0.0.1:1
`

// На странице «Входы» - журнал банов с итогами и ссылкой на старые строки.
func TestBanHistoryPage(t *testing.T) {
	f := newFixture(t, "")
	f.writeUser(testPassword)
	f.login()
	now := time.Now()
	if _, body := f.get("/logins"); !strings.Contains(body, "Записей нет") {
		t.Fatal("пустой журнал")
	}
	active := netip.MustParsePrefix("203.0.113.7/32")
	lifted := netip.MustParsePrefix("203.0.113.8/32")
	expired := netip.MustParsePrefix("203.0.113.9/32")
	command := netip.MustParsePrefix("203.0.113.10/32")
	f.state.BanStarted(expired, now.Add(-2*time.Hour), now.Add(-time.Hour), "привязка")
	f.state.BanStarted(lifted, now, now.Add(time.Hour), "веб-интерфейс")
	f.state.BanEnded(lifted, now.Add(time.Hour), statedb.EndedWeb, now)
	f.state.BanStarted(command, now, now.Add(2*time.Hour), "веб-интерфейс")
	f.state.BanEnded(command, now.Add(2*time.Hour), statedb.EndedCommand, now)
	f.state.BanStarted(active, now, now.Add(time.Hour), "привязка")
	f.state.Flush()
	_, body := f.get("/logins")
	for _, want := range []string{"История банов", active.String(), "действует", "снят в интерфейсе", "снят командой", "истёк", "привязка"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %q", want)
		}
	}
	if strings.Contains(body, "Более старые записи") {
		t.Fatal("ссылка на старые строки без старых строк")
	}

	for i := range banHistoryPage {
		f.state.BanStarted(netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}), 32), now, now.Add(time.Minute), "привязка")
	}
	f.state.Flush()
	_, body = f.get("/logins")
	if strings.Contains(body, "истёк") {
		t.Fatal("на первой странице лишние строки")
	}
	i := strings.Index(body, "/logins?before=")
	if i < 0 || !strings.Contains(body, "Более старые записи") {
		t.Fatal("нет ссылки на старые строки")
	}
	older := body[i : i+strings.IndexByte(body[i:], '"')]
	_, page := f.get(older)
	if !strings.Contains(page, "истёк") || !strings.Contains(page, "К последним записям") {
		t.Fatal("старые строки не показаны")
	}
	// Мусор в адресе - первая страница.
	if status, page := f.get("/logins?before=abc%27"); status != 200 || !strings.Contains(page, "Более старые записи") {
		t.Fatalf("мусор: %d", status)
	}
}

// Прибор без связи показывает последние значения с пометкой, прибор на связи - только те, которых
// сейчас не сообщает. Алиса старых значений не видит.
func TestLastValuesOnInfoPage(t *testing.T) {
	f := newFixture(t, stateDevices)
	f.writeUser(testPassword)
	f.login()
	past := time.Now().Add(-3 * time.Hour)
	f.state.SetLink("strip", statedb.Link{First: past, Off: past, Seen: true})
	f.state.SetValues("strip", map[string]any{"on": true, "brightness": 40.0, "rgb": "#FF0000"}, past)
	f.state.SetValues("oven", map[string]any{"Old.Name": "before"}, past)
	f.state.SetValues("oven", map[string]any{"Other": 1.0}, past)

	_, body := f.get("/devices/strip/info")
	if !strings.Contains(body, `class="stale"`) || !strings.Contains(body, "#FF0000") || !strings.Contains(body, "последнее") || !strings.Contains(body, "color_setting: цвет") {
		t.Fatal("нет последних значений ленты")
	}
	if dev, _ := f.gw.Device("strip"); fmt.Sprint(dev.States()[0].ErrorCode) != "DEVICE_UNREACHABLE" {
		t.Fatal("Алиса видит старые значения ленты")
	}

	_, body = f.get("/devices/oven/info")
	if !strings.Contains(body, "Old.Name") || !strings.Contains(body, "последнее, 3 ч назад") {
		t.Fatal("нет последнего значения духовки с пометкой")
	}
	if strings.Count(body, `class="stale"`) != 2 {
		t.Fatalf("текущие значения помечены как старые: %d", strings.Count(body, `class="stale"`))
	}
	hc, _ := f.gw.HomeConnect("oven")
	if _, ok := hc.Appliance.State().Values["Old.Name"]; ok {
		t.Fatal("старое значение попало в состояние прибора")
	}
}

// История на странице - с датой, простой шлюза подписан, а пересборка прибора её не стирает.
func TestHistoryRowsWithDate(t *testing.T) {
	f := newFixture(t, stateDevices)
	f.writeUser(testPassword)
	f.login()
	at := time.Now().Add(-48 * time.Hour)
	f.state.AddChanges("oven", []statedb.Change{{Time: at, Kind: statedb.GapKind, Value: "не работал 3 ч"}})
	cfg := f.config()
	d := cfg.Devices["oven"]
	d.Name = "Другое имя"
	cfg.Devices["oven"] = d
	if err := f.gw.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	_, body := f.get("/devices/oven/info")
	for _, want := range []string{at.Local().Format("02.01 15:04:05"), "шлюз", "не работал 3 ч", "До 50 последних изменений"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %q", want)
		}
	}
}

// Если база не принимает запись, интерфейс об этом говорит.
func TestStateFailingStep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root пишет в файл без прав записи")
	}
	f := newFixture(t, "", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, statedb.FileName), nil, 0o400); err != nil {
			t.Fatal(err)
		}
	})
	f.writeUser(testPassword)
	f.login()
	if _, body := f.get("/connections"); !strings.Contains(body, "База состояния не принимает запись") {
		t.Fatal("нет предупреждения")
	}
}

// Все состояния стиральной машины LG подписаны по-русски.
func TestWasherStateLabels(t *testing.T) {
	for _, state := range []string{"RINSING", "DETECTING", "INITIAL", "RINSE_HOLD", "RESERVED", "REFRESHING", "SPINNING",
		"POWER_OFF", "PAUSE", "DRYING", "SLEEP", "COOL_DOWN", "END", "RUNNING", "ERROR"} {
		if _, ok := stateNames[state]; !ok {
			t.Errorf("нет подписи %s", state)
		}
	}
}

// Сертификат телевизора изменился: метка на плитке, отпечаток и кнопка на странице, решение владельца.
func TestTVCertChanged(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	want := tv.Fingerprint(srv.Certificate().Raw)
	const old = "00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF"
	f := newFixture(t, "\ndevices:\n  tv:\n    name: Телевизор\n    source: webos\n    host: "+strings.TrimPrefix(srv.URL, "https://")+"\n", func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, "webos"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "webos", "tv.json"), []byte(`{"client_key":"k","cert":"`+old+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	f.writeUser(testPassword)
	f.login()
	hc, ok := f.gw.HomeConnect("tv")
	if !ok {
		t.Fatal("нет телевизора")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hc.Appliance.Run(ctx, func([]string) {})
	set, _ := f.gw.Television("tv")
	for end := time.Now().Add(5 * time.Second); set.Link().NewCert == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("шлюз не заметил чужой сертификат")
		}
	}
	if _, body := f.get("/devices/tv"); !strings.Contains(body, "Сертификат телевизора изменился") || !strings.Contains(body, groups(want)) || !strings.Contains(body, "/devices/tv/tv/cert") {
		t.Fatal("на странице прибора нет предупреждения")
	}
	if _, body := f.get("/"); !strings.Contains(body, "сертификат изменился, подтвердите") {
		t.Fatal("на плитке нет метки")
	}
	if _, body := f.get("/devices/tv/tv/cert"); !strings.Contains(body, groups(want)) || !strings.Contains(body, want) {
		t.Fatal("на странице подтверждения нет отпечатка")
	}
	if _, _, _ = f.post("/devices/tv/tv/cert", url.Values{"cert": {old}}); set.Link().Cert != old {
		t.Fatal("принят не тот отпечаток")
	}
	if status, _, _ := f.post("/devices/tv/tv/cert", url.Values{"cert": {want}}); status != http.StatusSeeOther || set.Link().Cert != want || set.Link().NewCert != "" {
		t.Fatalf("решение владельца: %d %+v", status, set.Link())
	}
}

// Страница брокера: порт закрыт без пароля, пароль создаётся с подтверждением, сообщение уходит
// с подтверждением и видно в журнале и фильтре, а неверное отклоняется до брокера.
func TestMQTTPage(t *testing.T) {
	f := newFixture(t, "")
	f.writeUser(testPassword)
	f.login()
	if _, body := f.get("/mqtt"); !strings.Contains(body, "Порт закрыт") || !strings.Contains(body, "не задан") {
		t.Fatal("страница без пароля")
	}
	if status, body := f.get("/mqtt/password"); status != http.StatusOK || !strings.Contains(body, "Создать пароль") {
		t.Fatalf("подтверждение пароля: %d", status)
	}
	if status, _, _ := f.post("/mqtt/password", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("создание пароля: %d", status)
	}
	password := f.config().MQTT.Password
	if len(password) < 32 {
		t.Fatalf("пароль: %d знаков", len(password))
	}
	if _, body := f.get("/mqtt"); !strings.Contains(body, password) || !strings.Contains(body, "откроется после перезапуска") {
		t.Fatal("страница не показывает пароль и что порт закрыт")
	}

	addr := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}()
	b := mqttbroker.New(mqttbroker.Config{Listen: addr, Credentials: func() (string, string) { return "hcgateway", password }}, f.state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	for end := time.Now().Add(5 * time.Second); !b.Status("").Open; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("брокер не открыл порт")
		}
	}
	f.ui.AttachBroker(b)
	q := url.Values{"topic": {"polaris/1/control/mode"}, "payload": {"<1>"}, "qos": {"1"}, "retain": {"on"}}
	if status, body := f.get("/mqtt/publish?" + q.Encode()); status != http.StatusOK || !strings.Contains(body, "polaris/1/control/mode") || !strings.Contains(body, `name="retain"`) || !strings.Contains(body, "выполнит команду") {
		t.Fatalf("подтверждение: %d", status)
	}
	if status, _, _ := f.post("/mqtt/publish", q); status != http.StatusSeeOther {
		t.Fatalf("отправка: %d", status)
	}
	_, body := f.get("/mqtt?topic=polaris/")
	for _, want := range []string{"порт открыт", "от шлюза", "polaris/1/control/mode", "&lt;1&gt;", "сохраняется"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %q", want)
		}
	}
	if _, body := f.get("/mqtt?topic=other/"); strings.Contains(body, "от шлюза") {
		t.Fatal("фильтр не сработал")
	}
	for _, bad := range []url.Values{{"topic": {""}}, {"topic": {"a/#"}}, {"topic": {"$SYS/x"}}, {"topic": {"a"}, "qos": {"3"}}, {"topic": {"a"}, "payload": {strings.Repeat("x", mqttbroker.MaxPayloadBytes+1)}}} {
		if status, body, _ := f.post("/mqtt/publish", bad); status == http.StatusSeeOther && strings.Contains(body, "отправлено") {
			t.Fatalf("принято %v", bad)
		}
	}
	if st := b.Status(""); len(st.Journal) != 1 {
		t.Fatalf("неверные сообщения дошли до брокера: %+v", st.Journal)
	}
}

// Чайник Polaris: без пароля брокера не добавляется, а с паролем ждёт прибор, и на странице об этом сказано.
func TestPolarisDevice(t *testing.T) {
	f := newFixture(t, "")
	f.writeUser(testPassword)
	f.login()
	add := func() url.Values {
		return url.Values{"name": {"Чайник"}, "source": {"polaris"}, "device_id": {"aabbccddeeff"}}
	}
	f.get("/")
	if status, body, _ := f.post("/devices", add()); status == http.StatusSeeOther || !strings.Contains(body, "создайте пароль на странице MQTT") {
		t.Fatalf("прибор без пароля брокера: %d", status)
	}
	if status, _, _ := f.post("/mqtt/password", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("пароль: %d", status)
	}
	if _, body := f.get("/?polaris_id=aabbccddeeff"); !strings.Contains(body, "aabbccddeeff") {
		t.Fatal("форма добавления без идентификатора")
	}
	if status, _, _ := f.post("/devices", add()); status != http.StatusSeeOther {
		t.Fatalf("добавление: %d", status)
	}
	var id string
	for k, d := range f.config().Devices {
		if d.DeviceID == "aabbccddeeff" {
			id = k
		}
	}
	if id == "" {
		t.Fatal("прибор не сохранён")
	}
	if !f.gw.Waiting(id) {
		t.Fatal("прибор не ждёт данных")
	}
	if _, body := f.get("/devices/" + id); !strings.Contains(body, "Шлюз ещё не знает этот прибор Polaris") {
		t.Fatal("на странице нет объяснения")
	}
	if _, body := f.get("/"); !strings.Contains(body, "ждёт прибор Polaris") {
		t.Fatal("на плитке нет причины")
	}
}
