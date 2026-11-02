package config

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hcgateway/internal/profile"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/data" {
		t.Fatalf("data_dir = %q", cfg.DataDir)
	}
}

func TestLoadDataDir(t *testing.T) {
	cfg, err := Load(writeConfig(t, "data_dir: ./data\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "./data" {
		t.Fatalf("data_dir = %q", cfg.DataDir)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"пустой data_dir":     "data_dir: \"\"\n",
		"неизвестное поле":    "data_dir: ./data\ndatadir: x\n",
		"убранный пароль":     "yandex:\n  password: fedcba9876543210\n",
		"неверный прокси":     "trusted_proxies: [10.0.0.0/33]\n",
		"пустой redirect_uri": "yandex:\n  redirect_uris: [\"\"]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
}

func TestLoadTooBig(t *testing.T) {
	body := "data_dir: ./data\n#" + strings.Repeat("a", maxConfigSize)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("ожидалась ошибка размера")
	}
}

const serveConfig = `
data_dir: ./data
yandex:
  client_id: yandex
  client_secret: 0123456789abcdef
devices:
  demo-oven:
    name: Духовка
    room: Кухня
    demo: Oven
    profile:
      default_program: Cooking.Oven.Program.HeatingMode.PizzaSetting
profiles:
  Dishwasher:
    default_program: Dishcare.Dishwasher.Program.Eco50
`

func TestLoadServe(t *testing.T) {
	cfg, err := Load(writeConfig(t, serveConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.Metrics.Listen != ":8082" || cfg.Metrics.Token != "" ||
		cfg.Yandex.RedirectURIs[0] != yandexRedirect || !cfg.Yandex.Ready() {
		t.Fatalf("умолчания: %+v", cfg)
	}
	d := cfg.Devices["demo-oven"]
	if d.Source != SourceDemo || d.Demo != "Oven" || d.Profile.DefaultProgram != "Cooking.Oven.Program.HeatingMode.PizzaSetting" {
		t.Fatalf("прибор: %+v", d)
	}
	if cfg.Profiles["Dishwasher"].DefaultProgram != "Dishcare.Dishwasher.Program.Eco50" {
		t.Fatalf("профиль: %+v", cfg.Profiles["Dishwasher"])
	}
}

func TestLoadSource(t *testing.T) {
	cfg, err := Load(writeConfig(t, "devices:\n  a:\n    name: A\n    source: demo\n    demo: Dishwasher\n  b:\n    name: B\n    source: wled\n    host: \"192.168.1.50:80\"\n    presets: {3: candle, 5: party}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := cfg.Devices["a"], cfg.Devices["b"]; a.Source != SourceDemo || a.Demo != "Dishwasher" || b.Source != SourceWLED ||
		b.Host != "192.168.1.50:80" || b.Presets[3] != "candle" || b.Presets[5] != "party" {
		t.Fatalf("приборы: %+v", cfg.Devices)
	}
}

func TestLoadDeviceErrors(t *testing.T) {
	cases := map[string]string{
		"недопустимый id":          "devices:\n  ../x:\n    name: A\n",
		"нет имени":                "devices:\n  a:\n    room: B\n",
		"длинное имя":              "devices:\n  a:\n    name: " + strings.Repeat("я", maxNameLen+1) + "\n",
		"неизвестный демо":         "devices:\n  a:\n    name: A\n    demo: Kettle\n",
		"неизвестное поле профиля": "devices:\n  a:\n    name: A\n    demo: Oven\n    profile:\n      programs: {}\n",
		"нет источника":            "devices:\n  a:\n    name: A\n",
		"неизвестный источник":     "devices:\n  a:\n    name: A\n    source: zigbee\n",
		"demo без типа":            "devices:\n  a:\n    name: A\n    source: demo\n",
		"тип демо у другого":       "devices:\n  a:\n    name: A\n    source: wled\n    host: wled.local\n    demo: Oven\n",
		"лента без адреса":         "devices:\n  a:\n    name: A\n    source: wled\n",
		"адрес со схемой":          "devices:\n  a:\n    name: A\n    source: wled\n    host: http://wled.local\n",
		"пресет вне пределов":      "devices:\n  a:\n    name: A\n    source: wled\n    host: wled.local\n    presets: {251: candle}\n",
		"чужая сцена":              "devices:\n  a:\n    name: A\n    source: wled\n    host: wled.local\n    presets: {1: sunset}\n",
		"сцена дважды":             "devices:\n  a:\n    name: A\n    source: wled\n    host: wled.local\n    presets: {1: candle, 2: candle}\n",
		"профиль ленты":            "devices:\n  a:\n    name: A\n    source: wled\n    host: wled.local\n    profile:\n      on_off: power\n",
		"пресеты у демо":           "devices:\n  a:\n    name: A\n    demo: Oven\n    presets: {1: candle}\n",
		"нет device_id":            "devices:\n  a:\n    name: A\n    source: thinq\n",
		"device_id у демо":         "devices:\n  a:\n    name: A\n    demo: Oven\n    device_id: x\n",
		"device_id дважды":         "devices:\n  a:\n    name: A\n    source: thinq\n    device_id: x\n  b:\n    name: B\n    source: thinq\n    device_id: x\n",
		"чужая страна ThinQ":       "thinq:\n  pat: p\n  country: XX\n",
		"локальный без адреса":     "devices:\n  a:\n    name: A\n    source: homeconnect-local\n    ha_id: X\n",
		"камера у демо":            "devices:\n  a:\n    name: A\n    demo: Oven\n    location: FREEZER\n",
		"неверная камера":          "devices:\n  a:\n    name: A\n    source: thinq\n    device_id: x\n    location: морозилка\n",
		"камера дважды":            "devices:\n  a:\n    name: A\n    source: thinq\n    device_id: x\n    location: FREEZER\n  b:\n    name: B\n    source: thinq\n    device_id: x\n    location: FREEZER\n",
		"локальный без haId":       "devices:\n  a:\n    name: A\n    source: homeconnect-local\n    host: 192.168.1.5\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
}

func TestValidateServeErrors(t *testing.T) {
	cases := map[string]func(c *Config){
		"нет client_id":         func(c *Config) { c.Yandex.ClientID = "" },
		"нет client_secret":     func(c *Config) { c.Yandex.ClientSecret = "" },
		"короткий secret":       func(c *Config) { c.Yandex.ClientSecret = "short" },
		"skill_id без токена":   func(c *Config) { c.Yandex.SkillID = "skill" },
		"порт метрик":           func(c *Config) { c.Metrics.Listen = c.Listen },
		"короткий токен метрик": func(c *Config) { c.Metrics.Token = "short" },
		"прибор без источника":  func(c *Config) { c.Devices["real"] = Device{Name: "Р"} },
		"ThinQ без PAT":         func(c *Config) { c.Devices["real"] = Device{Name: "Р", Source: SourceThinQ, DeviceID: "d"} },
		"PAT без страны":        func(c *Config) { c.ThinQ.PAT = "pat" },
		"пустой redirect_uris":  func(c *Config) { c.Yandex.RedirectURIs = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, serveConfig))
			if err != nil {
				t.Fatal(err)
			}
			change(cfg)
			if err := cfg.ValidateServe(); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
}

func TestLoadEmptyServe(t *testing.T) {
	// Шлюз запускается и с пустым файлом: настройки задаются в веб-интерфейсе.
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateServe(); err != nil || cfg.Yandex.Ready() {
		t.Fatalf("пустые настройки: %v", err)
	}
}

func TestTrustedProxies(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Proxies) != len(DefaultTrustedProxies) || !cfg.Proxies[0].Contains(netip.MustParseAddr("10.1.2.3")) {
		t.Fatalf("по умолчанию: %v", cfg.Proxies)
	}
	cfg, err = Load(writeConfig(t, "trusted_proxies: [172.18.0.2, \"2001:db8::/32\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Proxies) != 2 || cfg.Proxies[0] != netip.MustParsePrefix("172.18.0.2/32") {
		t.Fatalf("свои: %v", cfg.Proxies)
	}
	cfg, err = Load(writeConfig(t, "trusted_proxies: []\n"))
	if err != nil || len(cfg.Proxies) != 0 {
		t.Fatalf("пустой список: %v %v", cfg.Proxies, err)
	}
}

func TestNormalizeOptions(t *testing.T) {
	cfg, err := Load(writeConfig(t, "devices:\n  a:\n    name: A\n    demo: Oven\n    profile:\n      default_options:\n        X: 200\n        Y: on\n"))
	if err != nil {
		t.Fatal(err)
	}
	opts := cfg.Devices["a"].Profile.DefaultOptions
	if v, ok := opts["X"].(float64); !ok || v != 200 || opts["Y"] != "on" {
		t.Fatalf("default_options: %#v", opts)
	}
}

func TestFileSave(t *testing.T) {
	path := writeConfig(t, "# комментарий\n"+serveConfig)
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cur, version := f.Current()
	next := cur.Clone()
	empty := []string{}
	next.TrustedProxies = &empty
	d := next.Devices["demo-oven"]
	d.Name = "Новое имя"
	d.Profile = &profile.Profile{Toggles: []profile.Toggle{}}
	next.Devices["demo-oven"] = d
	if cur.Devices["demo-oven"].Name != "Духовка" {
		t.Fatal("Clone делит приборы с исходными настройками")
	}
	saved, version2, err := f.Save(next, version)
	if err != nil {
		t.Fatal(err)
	}
	if version2 == version || saved.Devices["demo-oven"].Name != "Новое имя" {
		t.Fatalf("сохранено: %s %+v", version2, saved.Devices)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	// Значения по умолчанию не пишутся, пустой список переопределения сохраняется.
	if strings.Contains(text, "listen") || strings.Contains(text, "redirect_uris") || !strings.Contains(text, "toggles: []") ||
		!strings.Contains(text, "trusted_proxies: []") || strings.Contains(text, "комментарий") {
		t.Fatalf("файл:\n%s", text)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := again.Devices["demo-oven"].Profile; p == nil || p.Toggles == nil || len(p.Toggles) != 0 || p.Modes != nil {
		t.Fatalf("профиль после чтения: %+v", p)
	}
	if len(again.Proxies) != 0 || again.Listen != ":8080" {
		t.Fatalf("после чтения: %+v", again)
	}
	bak, _ := os.ReadFile(path + ".bak")
	if !strings.Contains(string(bak), "комментарий") {
		t.Fatalf("в .bak нет прежней версии:\n%s", bak)
	}

	// Правка по старой версии не сохраняется.
	if _, _, err := f.Save(next, version); !errors.Is(err, ErrStale) {
		t.Fatalf("старая версия: %v", err)
	}
	// Ручная правка файла после загрузки - тоже.
	if err := os.WriteFile(path, append(data, []byte("\n# правка\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Save(next, version2); !errors.Is(err, ErrChanged) {
		t.Fatalf("ручная правка: %v", err)
	}
	cfg, version3, err := f.Read()
	if err != nil {
		t.Fatal(err)
	}
	f.Use(cfg, version3)
	if _, _, err := f.Save(next, version3); err != nil {
		t.Fatalf("после перечитывания: %v", err)
	}
}

func TestSampleConfig(t *testing.T) {
	// Пример настроек из репозитория читается и проходит проверки запуска.
	cfg, err := Load(filepath.Join("..", "..", "config.sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	if cfg.Yandex.Ready() || len(cfg.Devices) != 2 || cfg.Listen != ":8080" {
		t.Fatalf("пример: %+v", cfg)
	}
}

func TestThinQCameras(t *testing.T) {
	// Камеры одного прибора LG - разные приборы настроек, а профиль прибору LG задаётся.
	cfg, err := Load(writeConfig(t, "devices:\n  fridge:\n    name: Х\n    source: thinq\n    device_id: x\n"+
		"  freezer:\n    name: М\n    source: thinq\n    device_id: x\n    location: FREEZER\n"+
		"    profile:\n      toggles: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Devices["freezer"]; d.Location != "FREEZER" || d.Profile == nil {
		t.Fatalf("морозильная камера: %+v", d)
	}
}

func TestLoadCoffee(t *testing.T) {
	cfg, err := Load(writeConfig(t, "devices:\n  coffee:\n    name: Кофемашина\n    source: delonghi\n    host: 192.168.1.70\n"+
		"    serial: 000000ZZ00000000000\n    pin: \"aa:bb:"+strings.Repeat("00:", 29)+"cc\"\n    profile_id: 2\n"+
		"    recipes: {espresso: 1, americano: 6}\n    default_drink: espresso\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Devices["coffee"]
	if d.Pin != "AABB"+strings.Repeat("00", 29)+"CC" || *d.ProfileID != 2 || d.Recipes["americano"] != 6 || d.DefaultDrink != "espresso" {
		t.Fatalf("кофемашина: %+v", d)
	}
	base := "devices:\n  coffee:\n    name: К\n    source: delonghi\n    serial: SN1\n"
	cases := map[string]string{
		"нет адреса":          base,
		"имя вместо IP":       base + "    host: coffee.local\n",
		"внешний IP":          base + "    host: 8.8.8.8\n",
		"нет номера":          "devices:\n  coffee:\n    name: К\n    source: delonghi\n    host: 192.168.1.5\n",
		"номер с пробелом":    base + "    host: 192.168.1.5\n    serial: \"SN 1\"\n",
		"короткий pin":        base + "    host: 192.168.1.5\n    pin: AB:CD\n",
		"чужой напиток":       base + "    host: 192.168.1.5\n    recipes: {mocha: 3}\n",
		"рецепт вне границ":   base + "    host: 192.168.1.5\n    recipes: {espresso: 300}\n",
		"рецепт дважды":       base + "    host: 192.168.1.5\n    recipes: {espresso: 1, latte: 1}\n",
		"напиток без рецепта": base + "    host: 192.168.1.5\n    recipes: {espresso: 1}\n    default_drink: latte\n",
		"профиль вне границ":  base + "    host: 192.168.1.5\n    profile_id: 99\n",
		"номер дважды":        base + "    host: 192.168.1.5\n  b:\n    name: Б\n    source: delonghi\n    serial: SN1\n    host: 192.168.1.6\n",
		"номер у демо":        "devices:\n  a:\n    name: A\n    demo: Oven\n    serial: SN1\n",
		"рецепты у демо":      "devices:\n  a:\n    name: A\n    demo: Oven\n    recipes: {espresso: 1}\n",
		"iot без wss":         "delonghi:\n  iot_url: ws://example.com/mqtt\n",
		"gigya без https":     "delonghi:\n  gigya_url: http://example.com\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
}

// Телевизор: только IP, MAC приводится к одному виду; устройство SmartThings: компонент main
// хранится пустым, пара deviceId и компонента - у одного прибора, без приложения шлюз не
// запускается.
func TestNewSources(t *testing.T) {
	cfg, err := parse([]byte(`smartthings:
  client_id: st
  client_secret: st-secret
devices:
  tv:
    name: Телевизор
    source: webos
    host: 192.168.1.80
    mac: aa-bb-cc-dd-ee-ff
    broadcast: 192.168.1.255
  lamp:
    name: Лампа
    source: smartthings
    device_id: 0f0e-1d2c
    location: main
  freezer:
    name: Морозилка
    source: smartthings
    device_id: 0f0e-1d2c
    location: freezer
`))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Devices["tv"]; d.MAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("MAC: %q", d.MAC)
	}
	if d := cfg.Devices["lamp"]; d.Location != "" {
		t.Fatalf("компонент main: %q", d.Location)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	if cfg.SmartThings.APIURL != smartThingsURL {
		t.Fatalf("адрес SmartThings: %q", cfg.SmartThings.APIURL)
	}
	for name, text := range map[string]string{
		"имя вместо IP":     "devices:\n  tv: {name: TV, source: samsung-tv, host: tv.local}\n",
		"публичный IP":      "devices:\n  tv: {name: TV, source: webos, host: 8.8.8.8}\n",
		"неверный MAC":      "devices:\n  tv: {name: TV, source: webos, host: 192.168.1.80, mac: zz}\n",
		"чужой broadcast":   "devices:\n  tv: {name: TV, source: webos, host: 192.168.1.80, broadcast: 8.8.8.255}\n",
		"MAC не телевизору": "devices:\n  s: {name: S, source: wled, host: 192.168.1.5, mac: aa:bb:cc:dd:ee:ff}\n",
		"компонент дважды": "devices:\n  a: {name: A, source: smartthings, device_id: x1}\n" +
			"  b: {name: B, source: smartthings, device_id: x1, location: main}\n",
		"плохой компонент": "devices:\n  a: {name: A, source: smartthings, device_id: x1, location: 'a/b'}\n",
		"без deviceId":     "devices:\n  a: {name: A, source: smartthings}\n",
		"api_url по http":  "smartthings:\n  api_url: http://example.com\n",
	} {
		if _, err := parse([]byte(text)); err == nil {
			t.Errorf("%s: настройки приняты", name)
		}
	}
	cfg, err = parse([]byte("devices:\n  a: {name: A, source: smartthings, device_id: x1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateServe(); err == nil {
		t.Fatal("устройство SmartThings без приложения")
	}
}

// Сеть поиска - только частная IPv4-сеть не больше 1024 адресов; адрес с битами узла - его сеть.
func TestParseNetwork(t *testing.T) {
	for in, want := range map[string]string{"192.168.1.0/24": "192.168.1.0/24", "10.20.30.7/22": "10.20.28.0/22",
		" 10.1.2.3/32 ": "10.1.2.3/32"} {
		if p, err := ParseNetwork(in); err != nil || p.String() != want {
			t.Errorf("%q: %v %v, нужно %s", in, p, err, want)
		}
	}
	for _, in := range []string{"8.8.8.0/24", "192.168.0.0/21", "fd00::/64", "192.168.1.1", "сеть", ""} {
		if _, err := ParseNetwork(in); err == nil {
			t.Errorf("%q принята", in)
		}
	}
	cfg, err := Load(writeConfig(t, "discovery:\n  network: 192.168.1.9/24\n"))
	if err != nil || cfg.Discovery.Network != "192.168.1.0/24" {
		t.Fatalf("настройки: %+v %v", cfg.Discovery, err)
	}
	if _, err := Load(writeConfig(t, "discovery:\n  network: 1.2.3.0/24\n")); err == nil {
		t.Fatal("чужая сеть в настройках принята")
	}
}

// Адрес облака Home Connect без схемы шлюз дополняет https, а http к чужому серверу не принимает.
func TestHomeConnectAddress(t *testing.T) {
	for in, want := range map[string]string{"api.home-connect.cn": "https://api.home-connect.cn",
		"api.home-connect.cn/": "https://api.home-connect.cn", "https://api.home-connect.cn/": "https://api.home-connect.cn",
		"http://localhost:8080": "http://localhost:8080"} {
		cfg, err := parse([]byte("homeconnect:\n  api_url: " + in + "\n"))
		if err != nil || cfg.HomeConnect.APIURL != want {
			t.Errorf("%s: %+v %v, нужно %s", in, cfg, err, want)
		}
	}
	for _, in := range []string{"http://api.home-connect.cn", "ftp://api.home-connect.cn", "/api"} {
		if _, err := parse([]byte("homeconnect:\n  api_url: " + in + "\n")); err == nil {
			t.Errorf("%s: адрес принят", in)
		}
	}
}
