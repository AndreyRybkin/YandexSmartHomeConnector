package web

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/statedb"
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
