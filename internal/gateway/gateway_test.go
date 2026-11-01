package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/auth"
	"hcgateway/internal/config"
	"hcgateway/internal/hcapi/hcfake"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/linking"
	"hcgateway/internal/profile"
)

type noLogin struct{}

func (noLogin) Login(*http.Request, string, string, string) (auth.Result, auth.User) {
	return auth.Wrong, auth.User{}
}

func newGateway(t *testing.T) *Gateway {
	t.Helper()
	store, err := linking.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(t.TempDir(), store, noLogin{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func baseConfig() *config.Config {
	return &config.Config{
		Yandex: config.Yandex{ClientID: "yandex", ClientSecret: "0123456789abcdef",
			RedirectURIs: []string{"https://social.yandex.net/broker/redirect"}},
		Devices: map[string]config.Device{
			"oven": {Name: "Духовка", Source: config.SourceDemo, Demo: "Oven"},
			"dish": {Name: "Посудомойка", Source: config.SourceDemo, Demo: "Dishwasher"},
		},
	}
}

func hc(t *testing.T, g *Gateway, id string) *hcdevice.Device {
	t.Helper()
	d, ok := g.HomeConnect(id)
	if !ok {
		t.Fatalf("нет прибора %s", id)
	}
	return d
}

func TestApplyKeepsUnchanged(t *testing.T) {
	g := newGateway(t)
	if err := g.Apply(baseConfig()); err != nil {
		t.Fatal(err)
	}
	oven, dish := hc(t, g, "oven"), hc(t, g, "dish")
	if err := oven.Appliance.Start(context.Background(), "Cooking.Oven.Program.HeatingMode.HotAir", nil); err != nil {
		t.Fatal(err)
	}

	// Новое имя - новое устройство Алисы, но тот же демо-прибор: программа идёт дальше.
	cfg := baseConfig()
	d := cfg.Devices["oven"]
	d.Name = "Духовка на кухне"
	cfg.Devices["oven"] = d
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	oven2 := hc(t, g, "oven")
	if oven2 == oven || oven2.Name != "Духовка на кухне" || oven2.Appliance != oven.Appliance || !oven2.Appliance.State().Running() {
		t.Fatal("смена имени сбросила прибор")
	}
	if hc(t, g, "dish") != dish {
		t.Fatal("неизменённый прибор пересоздан")
	}

	// Профиль типа пересоздаёт только приборы этого типа.
	cfg = baseConfig()
	cfg.Devices["oven"] = d
	cfg.Profiles = map[string]*profile.Profile{"Dishwasher": {DefaultProgram: "Dishcare.Dishwasher.Program.Eco50"}}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if hc(t, g, "oven") != oven2 || hc(t, g, "dish") == dish || hc(t, g, "dish").Profile.DefaultProgram != "Dishcare.Dishwasher.Program.Eco50" {
		t.Fatal("профиль типа")
	}

	// Другой тип демо-прибора - новый источник.
	cfg.Devices["oven"] = config.Device{Name: "Теперь посудомойка", Source: config.SourceDemo, Demo: "Dishwasher"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if a := hc(t, g, "oven").Appliance; a == oven.Appliance || a.Describe().Type != "Dishwasher" {
		t.Fatal("смена типа демо-прибора")
	}
	if types := g.Types(); types["oven"] != "Dishwasher" || types["dish"] != "Dishwasher" {
		t.Fatalf("типы: %v", types)
	}
}

func TestApplyError(t *testing.T) {
	g := newGateway(t)
	if err := g.Apply(baseConfig()); err != nil {
		t.Fatal(err)
	}
	oven := hc(t, g, "oven")
	cfg := baseConfig()
	cfg.Devices["oven"] = config.Device{Name: "Духовка", Source: config.SourceDemo, Demo: "Oven",
		Profile: &profile.Profile{OnOff: "toggle"}}
	if err := g.Check(cfg); err == nil {
		t.Fatal("Check пропустил неверный профиль")
	}
	if err := g.Apply(cfg); err == nil {
		t.Fatal("Apply пропустил неверный профиль")
	}
	cfg.Devices["oven"] = config.Device{Name: "Посудомойка", Source: config.SourceHomeConnectLocal, HaID: "X", Host: "192.168.1.5"}
	if err := g.Apply(cfg); err == nil || !strings.Contains(err.Error(), "fetch-keys") {
		t.Fatalf("прибор без ключей: %v", err)
	}
	if hc(t, g, "oven") != oven || g.Config().Devices["oven"].Profile != nil {
		t.Fatal("ошибка применения изменила шлюз")
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

func TestLocalDevice(t *testing.T) {
	dir := t.TempDir()
	writeLocalKeys(t, dir, "SIEMENS-DISH-1")
	store, err := linking.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(dir, store, noLogin{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if list := g.LocalAppliances(); len(list) != 1 || list[0].HaID != "SIEMENS-DISH-1" || list[0].Mode != "tls" {
		t.Fatalf("приборы с ключами: %+v", list)
	}
	cfg := baseConfig()
	cfg.Devices["local"] = config.Device{Name: "Посудомойка", Source: config.SourceHomeConnectLocal, HaID: "SIEMENS-DISH-1",
		Host: "192.168.1.5"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	// Тип известен из ключей: прибор сразу в Алисе и не на связи, пока шлюз не подключился.
	local := hc(t, g, "local")
	if local.Profile.YandexType != "dishwasher" || local.Appliance.State().Online || g.Waiting("local") {
		t.Fatalf("прибор: тип %s", local.Profile.YandexType)
	}
	cfg = baseConfig()
	cfg.Devices["local"] = config.Device{Name: "Посудомойка", Source: config.SourceHomeConnectLocal, HaID: "SIEMENS-DISH-1",
		Host: "192.168.1.6"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if hc(t, g, "local").Appliance == local.Appliance {
		t.Fatal("новый адрес не пересоздал прибор")
	}
}

func TestHandlerNotConfigured(t *testing.T) {
	g := newGateway(t)
	hs := httptest.NewServer(g.Handler())
	defer hs.Close()
	head := func() int {
		resp, err := http.Head(hs.URL + "/v1.0")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := head(); status != http.StatusNotFound {
		t.Fatalf("до настроек: %d", status)
	}
	cfg := baseConfig()
	cfg.Yandex = config.Yandex{RedirectURIs: cfg.Yandex.RedirectURIs}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if status := head(); status != http.StatusNotFound {
		t.Fatalf("без client_id: %d", status)
	}
	if err := g.Apply(baseConfig()); err != nil {
		t.Fatal(err)
	}
	if status := head(); status != http.StatusOK {
		t.Fatalf("с client_id: %d", status)
	}
}

func TestApplyWhileRunning(t *testing.T) {
	g := newGateway(t)
	g.demoDuration = time.Hour
	if err := g.Apply(baseConfig()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()
	cfg := baseConfig()
	delete(cfg.Devices, "dish")
	cfg.Devices["oven2"] = config.Device{Name: "Вторая духовка", Source: config.SourceDemo, Demo: "Oven"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Device("dish"); ok {
		t.Fatal("удалённый прибор остался")
	}
	on := &alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: true}}
	if code := g.Service.Command(context.Background(), "oven2", on); code != "" {
		t.Fatalf("команда новому прибору: %q", code)
	}
	if !hc(t, g, "oven2").Appliance.State().Running() {
		t.Fatal("программа не запущена")
	}
	if code := g.Service.Command(context.Background(), "dish", on); code != alice.CodeNotFound {
		t.Fatalf("команда удалённому прибору: %q", code)
	}
	cancel()
	<-done
}

func TestStatus(t *testing.T) {
	g := newGateway(t)
	if err := g.Apply(baseConfig()); err != nil {
		t.Fatal(err)
	}
	if err := hc(t, g, "oven").Appliance.Start(context.Background(), "Cooking.Oven.Program.HeatingMode.HotAir", nil); err != nil {
		t.Fatal(err)
	}
	st := g.Status()
	if len(st) != 2 || st[0].ID != "dish" || st[0].Operation != "Ready" || st[1].Operation != "Run" || !st[1].Online || st[1].Source != "demo" {
		t.Fatalf("состояние: %+v", st)
	}
}

func TestCloudDevice(t *testing.T) {
	fake := hcfake.New(t, &hcfake.Appliance{HaID: "BOSCH-OVEN-1", Name: "Духовка", Type: "Oven", Brand: "Bosch",
		Connected: true, Settings: map[string]any{}, Status: map[string]any{
			"BSH.Common.Status.OperationState":            "BSH.Common.EnumType.OperationState.Ready",
			"BSH.Common.Status.RemoteControlStartAllowed": true,
		}})
	dir := t.TempDir()
	fake.TokenFile(t, filepath.Join(dir, "homeconnect-tokens.json"))
	store, err := linking.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(dir, store, noLogin{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.HomeConnect = config.HomeConnect{ClientID: hcfake.ClientID, ClientSecret: hcfake.ClientSecret, APIURL: fake.URL}
	cfg.Devices["cloud"] = config.Device{Name: "Духовка в облаке", Source: config.SourceHomeConnectCloud, HaID: "BOSCH-OVEN-1"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	// Тип прибора облако ещё не сообщило: в Алисе прибора нет, ошибки нет.
	if _, ok := g.Device("cloud"); ok || !g.Waiting("cloud") {
		t.Fatal("прибор облака до описания")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if d, ok := g.HomeConnect("cloud"); ok && d.Appliance.State().Online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("прибор облака не появился")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if g.Waiting("cloud") || hc(t, g, "cloud").Profile.YandexType != "cooking" {
		t.Fatal("прибор облака собран не по своему типу")
	}
	on := &alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: true}}
	if code := g.Service.Command(context.Background(), "cloud", on); code != "" {
		t.Fatalf("команда прибору облака: %q", code)
	}
	if fake.Count("PUT", "/api/homeappliances/BOSCH-OVEN-1/programs/active") != 1 {
		t.Fatalf("запросы: %v", fake.Requests)
	}
	cancel()
	<-done
}

func TestStripRebuild(t *testing.T) {
	g := newGateway(t)
	cfg := baseConfig()
	cfg.Devices["strip"] = config.Device{Name: "Лента", Source: config.SourceWLED, Host: "127.0.0.1:1",
		Presets: map[int]string{1: "candle"}}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	s, ok := g.Strip("strip")
	if !ok || s.Presets[1] != "candle" {
		t.Fatal("лента не собрана")
	}
	if _, ok := g.HomeConnect("strip"); ok {
		t.Fatal("лента собрана как прибор Home Connect")
	}
	// Те же настройки - та же лента; новые пресеты - новая лента.
	same := baseConfig()
	same.Devices["strip"] = config.Device{Name: "Лента", Source: config.SourceWLED, Host: "127.0.0.1:1",
		Presets: map[int]string{1: "candle"}}
	if err := g.Apply(same); err != nil {
		t.Fatal(err)
	}
	if s2, _ := g.Strip("strip"); s2 != s {
		t.Fatal("неизменённая лента пересоздана")
	}
	same.Devices["strip"] = config.Device{Name: "Лента", Source: config.SourceWLED, Host: "127.0.0.1:1",
		Presets: map[int]string{1: "candle", 2: "party"}}
	if err := g.Apply(same); err != nil {
		t.Fatal(err)
	}
	if s3, _ := g.Strip("strip"); s3 == s || s3.Presets[2] != "party" {
		t.Fatal("новые пресеты не применены")
	}
	if st := g.Status(); st[len(st)-1].ID != "strip" || st[len(st)-1].Online || st[len(st)-1].Source != "wled" {
		t.Fatalf("состояние для метрик: %+v", st)
	}
}

// Прибор LG без профиля ждёт; шлюз сам загружает профиль и собирает прибор.
func TestThinQWaiting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"response":[{"deviceId":"lg-1","deviceInfo":{"deviceType":"DEVICE_WASHER","alias":"Стиралка"}}]}`)
	})
	mux.HandleFunc("GET /devices/lg-1/profile", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"response":{"property":[{"location":{"locationName":"MAIN"},`+
			`"operation":{"washerOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP"]}}}}]}}`)
	})
	lg := httptest.NewServer(mux)
	defer lg.Close()
	g := newGateway(t)
	cfg := baseConfig()
	cfg.ThinQ = config.ThinQ{PAT: "pat", Country: "NL", APIURL: lg.URL}
	cfg.Devices["washer"] = config.Device{Name: "Стиралка", Source: config.SourceThinQ, DeviceID: "lg-1"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Device("washer"); ok || !g.Waiting("washer") {
		t.Fatal("прибор LG до профиля")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if d, ok := g.HomeConnect("washer"); ok && d.Profile.YandexType == "washing_machine" && !g.Waiting("washer") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("прибор LG не собран после загрузки профиля")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Кофемашина собирается по встроенному профилю, а recipes из настроек - слой под profile прибора.
func TestCoffeeMaker(t *testing.T) {
	g := newGateway(t)
	cfg := baseConfig()
	cfg.Devices["coffee"] = config.Device{Name: "Кофемашина", Source: config.SourceDeLonghi, Host: "192.168.1.70",
		Serial: "SN1", Recipes: map[string]int{"espresso": 1, "latte": 9}, DefaultDrink: "latte"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	d := hc(t, g, "coffee")
	m := d.Profile.ProgramMode()
	if d.Manufacturer != "De'Longhi" || m == nil || m.Programs["DeLonghi.Recipe.9"] != "latte" || m.Programs["DeLonghi.Recipe.6"] != "" ||
		d.Profile.DefaultProgram != "DeLonghi.Recipe.9" || d.Profile.Power == nil {
		t.Fatalf("профиль: %+v", d.Profile)
	}
	// Новые рецепты не пересоздают связь с машиной.
	before := d.Appliance
	cfg = cfg.Clone()
	c := cfg.Devices["coffee"]
	c.Recipes = map[string]int{"americano": 6}
	c.DefaultDrink = ""
	cfg.Devices["coffee"] = c
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if d = hc(t, g, "coffee"); d.Appliance != before || d.Profile.ProgramMode().Programs["DeLonghi.Recipe.6"] != "americano" {
		t.Fatal("рецепты пересоздали машину или не применились")
	}
}
