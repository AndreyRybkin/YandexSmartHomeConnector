// Package config читает, проверяет и сохраняет настройки шлюза в YAML-файле.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/profile"
	"hcgateway/internal/thinq"
	"hcgateway/internal/tv"
)

const (
	// maxConfigSize - предел размера файла настроек.
	maxConfigSize = 1 << 20
	// maxDevices - предел числа приборов.
	maxDevices = 50
	// maxNameLen - предел длины имени прибора и комнаты в символах.
	maxNameLen = 64
	// minSecretLen - минимальная длина client_secret.
	minSecretLen = 16
	// minMetricsTokenLen - минимальная длина токена Prometheus.
	minMetricsTokenLen = 32
	// homeConnectURL - адрес облака Home Connect по умолчанию.
	homeConnectURL = "https://api.home-connect.com"
	// smartThingsURL - адрес облака SmartThings по умолчанию.
	smartThingsURL = "https://api.smartthings.com"
	// maxValueLen - предел длины строк Яндекса: идентификаторов, токенов и адресов.
	maxValueLen = 1024
	// maxListLen - предел длины списков redirect_uris и trusted_proxies.
	maxListLen = 16
	// yandexRedirect и yandexRedirectRU - адреса, на которые Яндекс ждёт возврата после привязки
	// аккаунта: Яндекс ведёт привязку через оба сайта.
	yandexRedirect   = "https://social.yandex.net/broker/redirect"
	yandexRedirectRU = "https://social.yandex.ru/broker/redirect"
)

// Источники приборов.
const (
	SourceDemo             = "demo"
	SourceHomeConnectCloud = "homeconnect-cloud"
	SourceHomeConnectLocal = "homeconnect-local"
	SourceWLED             = "wled"
	SourceThinQ            = "thinq"
	SourceDeLonghi         = "delonghi"
	SourceWebOS            = "webos"
	SourceSamsungTV        = "samsung-tv"
	SourceSmartThings      = "smartthings"
	SourcePolaris          = "polaris"
)

// Sources - допустимые значения source.
var Sources = []string{SourceDemo, SourceHomeConnectCloud, SourceHomeConnectLocal, SourceWLED, SourceThinQ, SourceDeLonghi,
	SourceWebOS, SourceSamsungTV, SourceSmartThings, SourcePolaris}

// polarisIDRe - идентификатор прибора Polaris из топика: <mac> или, у старых прошивок, <тип>/<id>.
var polarisIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}(/[A-Za-z0-9_-]{1,64})?$`)

// haIDRe - допустимый haId: он уходит в адреса облака и в имена файлов.
var haIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// thinqIDRe - допустимый deviceId прибора ThinQ: он уходит в адреса облака и в имя файла.
// locationRe - допустимое имя камеры прибора ThinQ.
var (
	thinqIDRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	locationRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
)

// stIDRe - deviceId устройства SmartThings, UUID: он уходит в адреса облака и в имя файла.
// componentRe - компонент устройства SmartThings, например main или freezer.
var (
	stIDRe      = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	componentRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// serialRe - серийный номер кофемашины De'Longhi: он уходит машине при входе.
var serialRe = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// pinRe - отпечаток SHA-256 сертификата: 64 шестнадцатеричных знака, двоеточия допускаются.
var pinRe = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:?[0-9A-Fa-f]{2}){31}$`)

// CoffeeDrinks - напитки Алисы, которым сопоставляются рецепты кофемашины.
var CoffeeDrinks = []string{"espresso", "double_espresso", "americano", "cappuccino", "latte"}

// TVAppModes - режимы Алисы, которым сопоставляются приложения телевизора LG.
var TVAppModes = []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}

// tvAppRe - идентификатор приложения webOS, например com.webos.app.livetv: он уходит телевизору.
var tvAppRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// maxProfileID и maxRecipe - пределы профиля пользователя и номера рецепта кофемашины.
const (
	maxProfileID = 15
	maxRecipe    = 255
)

// hostRe - адрес в домашней сети: имя или IPv4, IPv6 в квадратных скобках, порт по желанию.
var hostRe = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]{2,45}\]|[A-Za-z0-9.-]{1,253})(:[0-9]{1,5})?$`)

// maxPreset - самый большой номер пресета WLED.
const maxPreset = 250

// DefaultTrustedProxies - чьему X-Forwarded-For шлюз верит, если trusted_proxies не задан:
// частные сети, в которых работают Docker и Traefik, и localhost.
var DefaultTrustedProxies = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "::1/128", "fc00::/7"}

// DeviceIDRe - допустимый идентификатор прибора: он уходит в Яндекс и в имена файлов.
var DeviceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Config - настройки шлюза.
type Config struct {
	// DataDir - каталог для ключей и описаний приборов, в Docker - том.
	DataDir string `yaml:"data_dir,omitempty"`
	// Listen - адрес HTTP-сервера: запросы Яндекса и веб-интерфейс, за Traefik.
	Listen  string  `yaml:"listen,omitempty"`
	Metrics Metrics `yaml:"metrics,omitempty"`
	MQTT    MQTT    `yaml:"mqtt,omitempty"`
	// HomeConnect - облако Home Connect.
	HomeConnect HomeConnect `yaml:"homeconnect,omitempty"`
	// ThinQ - облако LG ThinQ Connect.
	ThinQ ThinQ `yaml:"thinq,omitempty"`
	// DeLonghi - адреса облака De'Longhi; пусто - адреса приложения. Для проверки без машины.
	DeLonghi DeLonghi `yaml:"delonghi,omitempty"`
	// SmartThings - приложение API Access в SmartThings и адрес облака.
	SmartThings SmartThings `yaml:"smartthings,omitempty"`
	// Discovery - поиск приборов в домашней сети.
	Discovery Discovery `yaml:"discovery,omitempty"`
	// TrustedProxies - адреса и сети прокси, чьему X-Forwarded-For шлюз верит.
	// nil - не задан, действует DefaultTrustedProxies; пустой список - не верить никому.
	TrustedProxies *[]string `yaml:"trusted_proxies,omitempty"`
	// Proxies - разобранный TrustedProxies.
	Proxies []netip.Prefix `yaml:"-"`
	Yandex  Yandex         `yaml:"yandex,omitempty"`
	// Devices - приборы по идентификатору в Алисе.
	Devices map[string]Device `yaml:"devices,omitempty"`
	// Profiles - переопределения встроенных профилей по типу Home Connect.
	Profiles map[string]*profile.Profile `yaml:"profiles,omitempty"`
}

// Icons - значки приборов из набора интерфейса. Своих файлов нет: в SVG можно спрятать скрипт.
var Icons = []string{"oven", "dishwasher", "washer", "dryer", "coffee", "hood", "fridge", "robot", "light", "ac", "air",
	"tv", "sensor", "socket", "device"}

// Discovery - поиск приборов в домашней сети.
type Discovery struct {
	// Network - сеть последнего поиска, например 192.168.1.0/24; пусто - шлюз предлагает
	// сеть по адресам приборов.
	Network string `yaml:"network,omitempty"`
}

// MinNetworkBits - самая большая сеть поиска, /22 - 1024 адреса: в большей поиск идёт
// дольше, а роутер может принять его за атаку.
const MinNetworkBits = 22

// ParseNetwork разбирает сеть поиска: частная IPv4-сеть не больше 1024 адресов. Адрес с
// битами узла, например 192.168.1.7/24, - это его сеть 192.168.1.0/24. Чужие сети шлюз не
// сканирует.
func ParseNetwork(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%q: нужна сеть вида 192.168.1.0/24", s)
	}
	if !p.Addr().IsPrivate() {
		return netip.Prefix{}, fmt.Errorf("%q: искать можно только в частной сети 10.0.0.0/8, 172.16.0.0/12 или 192.168.0.0/16", s)
	}
	if p.Bits() < MinNetworkBits {
		return netip.Prefix{}, fmt.Errorf("%q: сеть больше /%d - больше 1024 адресов", s, MinNetworkBits)
	}
	return p.Masked(), nil
}

// Metrics - метрики для Prometheus.
type Metrics struct {
	// Listen - адрес для Prometheus, без Traefik.
	Listen string `yaml:"listen,omitempty"`
	// Token - токен Prometheus. Без него порт метрик не открывается.
	Token string `yaml:"token,omitempty"`
}

// MQTT - встроенный брокер MQTT для приборов и других клиентов домашней сети.
type MQTT struct {
	// Listen - адрес брокера. Порт можно публиковать только в домашнюю сеть: шифрования нет.
	Listen string `yaml:"listen,omitempty"`
	// Username - логин клиентов брокера.
	Username string `yaml:"username,omitempty"`
	// Password - пароль клиентов брокера. Без него порт не открывается.
	Password string `yaml:"password,omitempty"`
}

// HomeConnect - приложение на портале разработчика Home Connect и адрес облака.
type HomeConnect struct {
	ClientID     string `yaml:"client_id,omitempty"`
	ClientSecret string `yaml:"client_secret,omitempty"`
	// APIURL - адрес облака; для проверки без приборов - симулятор.
	APIURL string `yaml:"api_url,omitempty"`
}

// Ready сообщает, заданы ли client_id и client_secret: без них нет входа в облако.
func (h *HomeConnect) Ready() bool {
	return h.ClientID != "" && h.ClientSecret != ""
}

// ThinQ - аккаунт LG ThinQ Connect.
type ThinQ struct {
	// PAT - личный токен доступа ThinQ Connect.
	PAT string `yaml:"pat,omitempty"`
	// Country - страна аккаунта LG, например NL: по ней шлюз выбирает регион облака.
	Country string `yaml:"country,omitempty"`
	// APIURL - адрес облака; пусто - по стране. Для проверки без приборов - поддельное облако.
	APIURL string `yaml:"api_url,omitempty"`
}

// Ready сообщает, заданы ли PAT и страна.
func (t *ThinQ) Ready() bool {
	return t.PAT != "" && t.Country != ""
}

// SmartThings - приложение API Access, которое владелец создаёт утилитой SmartThings CLI.
// Токены входа хранятся в data_dir, а не в настройках.
type SmartThings struct {
	ClientID     string `yaml:"client_id,omitempty"`
	ClientSecret string `yaml:"client_secret,omitempty"`
	// APIURL - адрес облака; для проверки без устройств - поддельное облако.
	APIURL string `yaml:"api_url,omitempty"`
}

// Ready сообщает, заданы ли client_id и client_secret: без них нет входа в SmartThings.
func (s *SmartThings) Ready() bool {
	return s.ClientID != "" && s.ClientSecret != ""
}

// DeLonghi - адреса Gigya, облака приборов и брокера AWS IoT. Вход в аккаунт хранится
// в data_dir/delonghi, а не в настройках.
type DeLonghi struct {
	GigyaURL string `yaml:"gigya_url,omitempty"`
	APIURL   string `yaml:"api_url,omitempty"`
	IoTURL   string `yaml:"iot_url,omitempty"`
}

// Yandex - навык умного дома и привязка аккаунта.
type Yandex struct {
	// SkillID - идентификатор навыка из консоли Яндекс Диалогов.
	SkillID string `yaml:"skill_id,omitempty"`
	// CallbackToken - OAuth-токен владельца навыка для уведомлений.
	CallbackToken string `yaml:"callback_token,omitempty"`
	// ClientID и ClientSecret указываются в консоли навыка в разделе привязки аккаунта.
	ClientID     string `yaml:"client_id,omitempty"`
	ClientSecret string `yaml:"client_secret,omitempty"`
	// RedirectURIs - разрешённые адреса возврата после привязки.
	RedirectURIs []string `yaml:"redirect_uris,omitempty"`
}

// Ready сообщает, заданы ли client_id и client_secret: без них Яндекс не привяжет аккаунт.
func (y *Yandex) Ready() bool {
	return y.ClientID != "" && y.ClientSecret != ""
}

// Device - прибор в Алисе.
type Device struct {
	Name string `yaml:"name"`
	Room string `yaml:"room,omitempty"`
	// Icon - значок прибора в интерфейсе из Icons; пусто - по типу прибора.
	Icon string `yaml:"icon,omitempty"`
	// Source - откуда шлюз берёт прибор, одно из Sources. Если не задан, а Demo задан, - SourceDemo.
	Source string `yaml:"source,omitempty"`
	// Demo - тип демо-прибора из appliance.DemoKinds, только для SourceDemo.
	Demo string `yaml:"demo,omitempty"`
	// HaID - haId прибора в аккаунте Home Connect, для источников Home Connect.
	HaID string `yaml:"ha_id,omitempty"`
	// Host - адрес ленты или прибора в домашней сети: 192.168.1.50 или wled.local:80.
	Host string `yaml:"host,omitempty"`
	// Presets - номер пресета WLED -> сцена Алисы, только для SourceWLED.
	Presets map[int]string `yaml:"presets,omitempty"`
	// DeviceID - deviceId прибора в ThinQ или устройства в SmartThings.
	DeviceID string `yaml:"device_id,omitempty"`
	// Location - камера прибора ThinQ из его профиля, например FREEZER, или компонент
	// устройства SmartThings, например freezer; пусто - прибор целиком или компонент main.
	Location string `yaml:"location,omitempty"`
	// MAC - MAC телевизора для включения по сети; пусто - тот, что сообщил телевизор.
	MAC string `yaml:"mac,omitempty"`
	// Broadcast - широковещательный адрес для включения телевизора по сети; пусто - 255.255.255.255.
	Broadcast string `yaml:"broadcast,omitempty"`
	// Apps - режим Алисы -> идентификатор приложения webOS, только для SourceWebOS; пусто - режима нет.
	Apps map[string]string `yaml:"apps,omitempty"`
	// Serial - серийный номер кофемашины De'Longhi, только для SourceDeLonghi.
	Serial string `yaml:"serial,omitempty"`
	// Pin - подтверждённый отпечаток SHA-256 сертификата кофемашины; пусто - не подтверждён.
	Pin string `yaml:"pin,omitempty"`
	// ProfileID - профиль пользователя кофемашины для напитков; nil - 1.
	ProfileID *int `yaml:"profile_id,omitempty"`
	// Recipes - напиток Алисы -> номер рецепта кофемашины; пусто - встроенные номера.
	Recipes map[string]int `yaml:"recipes,omitempty"`
	// DefaultDrink - напиток для «включи» включённой кофемашины; пусто - americano.
	DefaultDrink string `yaml:"default_drink,omitempty"`
	// Profile - переопределение профиля только для этого прибора.
	Profile *profile.Profile `yaml:"profile,omitempty"`
}

func defaults() *Config {
	return &Config{
		DataDir:     "/data",
		Listen:      ":8080",
		Metrics:     Metrics{Listen: ":8082"},
		MQTT:        MQTT{Listen: ":1883", Username: "hcgateway"},
		HomeConnect: HomeConnect{APIURL: homeConnectURL},
		SmartThings: SmartThings{APIURL: smartThingsURL},
		Yandex:      Yandex{RedirectURIs: []string{yandexRedirect, yandexRedirectRU}},
	}
}

// Load читает файл настроек, подставляет значения по умолчанию и проверяет результат.
func Load(path string) (*Config, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return parse(data)
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigSize {
		return nil, fmt.Errorf("файл настроек больше %d байт", maxConfigSize)
	}
	return data, nil
}

// parse разбирает YAML. Неизвестные поля - ошибка: опечатка в имени поля не должна молча менять поведение.
func parse(data []byte) (*Config, error) {
	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("разбор настроек: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Clone - копия настроек, которую можно менять. Профили общие: их не меняют, а заменяют целиком.
func (c *Config) Clone() *Config {
	n := *c
	if c.TrustedProxies != nil {
		list := slices.Clone(*c.TrustedProxies)
		n.TrustedProxies = &list
	}
	n.Proxies = slices.Clone(c.Proxies)
	n.Yandex.RedirectURIs = slices.Clone(c.Yandex.RedirectURIs)
	n.Devices = maps.Clone(c.Devices)
	n.Profiles = maps.Clone(c.Profiles)
	return &n
}

func (c *Config) validate() error {
	if c.DataDir == "" {
		return errors.New("data_dir: не задан")
	}
	proxies := DefaultTrustedProxies
	if c.TrustedProxies != nil {
		proxies = *c.TrustedProxies
	}
	if len(proxies) > maxListLen {
		return fmt.Errorf("trusted_proxies: больше %d адресов", maxListLen)
	}
	c.Proxies = nil
	for _, s := range proxies {
		p, err := parsePrefix(s)
		if err != nil {
			return fmt.Errorf("trusted_proxies: %q - не адрес и не сеть", s)
		}
		c.Proxies = append(c.Proxies, p)
	}
	y := c.Yandex
	h := c.HomeConnect
	for name, v := range map[string]string{"yandex.skill_id": y.SkillID, "yandex.callback_token": y.CallbackToken,
		"yandex.client_id": y.ClientID, "yandex.client_secret": y.ClientSecret, "metrics.token": c.Metrics.Token, "mqtt.username": c.MQTT.Username, "mqtt.password": c.MQTT.Password,
		"homeconnect.client_id": h.ClientID, "homeconnect.client_secret": h.ClientSecret, "homeconnect.api_url": h.APIURL,
		"thinq.pat": c.ThinQ.PAT, "thinq.api_url": c.ThinQ.APIURL, "delonghi.gigya_url": c.DeLonghi.GigyaURL,
		"delonghi.api_url": c.DeLonghi.APIURL, "delonghi.iot_url": c.DeLonghi.IoTURL,
		"smartthings.client_id": c.SmartThings.ClientID, "smartthings.client_secret": c.SmartThings.ClientSecret,
		"smartthings.api_url": c.SmartThings.APIURL} {
		if len(v) > maxValueLen {
			return fmt.Errorf("%s: больше %d байт", name, maxValueLen)
		}
	}
	h.APIURL = withHTTPS(h.APIURL)
	if err := checkAPIURL(h.APIURL); err != nil {
		return fmt.Errorf("homeconnect.api_url: %w", err)
	}
	c.HomeConnect.APIURL = strings.TrimSuffix(h.APIURL, "/")
	if err := checkAPIURL(c.SmartThings.APIURL); err != nil {
		return fmt.Errorf("smartthings.api_url: %w", err)
	}
	c.SmartThings.APIURL = strings.TrimSuffix(c.SmartThings.APIURL, "/")
	if c.ThinQ.APIURL != "" {
		if err := checkAPIURL(c.ThinQ.APIURL); err != nil {
			return fmt.Errorf("thinq.api_url: %w", err)
		}
		c.ThinQ.APIURL = strings.TrimSuffix(c.ThinQ.APIURL, "/")
	}
	for name, v := range map[string]*string{"delonghi.gigya_url": &c.DeLonghi.GigyaURL, "delonghi.api_url": &c.DeLonghi.APIURL} {
		if *v == "" {
			continue
		}
		if err := checkAPIURL(*v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*v = strings.TrimSuffix(*v, "/")
	}
	if c.DeLonghi.IoTURL != "" {
		if err := checkIoTURL(c.DeLonghi.IoTURL); err != nil {
			return fmt.Errorf("delonghi.iot_url: %w", err)
		}
	}
	if n := c.Discovery.Network; n != "" {
		p, err := ParseNetwork(n)
		if err != nil {
			return fmt.Errorf("discovery.network: %w", err)
		}
		c.Discovery.Network = p.String()
	}
	if country := c.ThinQ.Country; country != "" {
		if _, ok := thinq.Region(country); !ok {
			return fmt.Errorf("thinq.country: %q - страны нет в списке ThinQ Connect, нужен код вида NL", country)
		}
	}
	if len(y.RedirectURIs) > maxListLen {
		return fmt.Errorf("yandex.redirect_uris: больше %d адресов", maxListLen)
	}
	for _, u := range y.RedirectURIs {
		if u == "" || len(u) > maxValueLen {
			return fmt.Errorf("yandex.redirect_uris: адрес пустой или длиннее %d байт", maxValueLen)
		}
	}
	if len(c.Devices) > maxDevices {
		return fmt.Errorf("devices: больше %d приборов", maxDevices)
	}
	haIDs, thinqIDs, serials := map[string]string{}, map[string]string{}, map[string]string{}
	for _, id := range slices.Sorted(maps.Keys(c.Devices)) {
		d := c.Devices[id]
		if !DeviceIDRe.MatchString(id) {
			return fmt.Errorf("devices: недопустимый идентификатор %q", id)
		}
		if d.Name == "" || utf8.RuneCountInString(d.Name) > maxNameLen {
			return fmt.Errorf("devices.%s.name: нужно от 1 до %d символов", id, maxNameLen)
		}
		if utf8.RuneCountInString(d.Room) > maxNameLen {
			return fmt.Errorf("devices.%s.room: больше %d символов", id, maxNameLen)
		}
		if d.Icon != "" && !slices.Contains(Icons, d.Icon) {
			return fmt.Errorf("devices.%s.icon: %q, допустимо %v", id, d.Icon, Icons)
		}
		switch {
		case d.Source == "" && d.Demo != "":
			d.Source = SourceDemo
		case d.Source == "":
			return fmt.Errorf("devices.%s.source: не задан", id)
		case !slices.Contains(Sources, d.Source):
			return fmt.Errorf("devices.%s.source: %q, допустимо %v", id, d.Source, Sources)
		}
		switch {
		case d.Source == SourceDemo && !slices.Contains(appliance.DemoKinds, d.Demo):
			return fmt.Errorf("devices.%s.demo: %q, допустимо %v", id, d.Demo, appliance.DemoKinds)
		case d.Source != SourceDemo && d.Demo != "":
			return fmt.Errorf("devices.%s.demo: задаётся только при source: %s", id, SourceDemo)
		}
		hc := d.Source == SourceHomeConnectCloud || d.Source == SourceHomeConnectLocal
		switch {
		case hc && !haIDRe.MatchString(d.HaID):
			return fmt.Errorf("devices.%s.ha_id: нужно от 1 до 64 латинских букв, цифр, _ и -", id)
		case !hc && d.HaID != "":
			return fmt.Errorf("devices.%s.ha_id: задаётся только для источников Home Connect", id)
		case hc && haIDs[d.HaID] != "":
			return fmt.Errorf("devices.%s.ha_id: %s уже у прибора %s", id, d.HaID, haIDs[d.HaID])
		case hc:
			haIDs[d.HaID] = id
		}
		if err := checkStrip(id, &d); err != nil {
			return err
		}
		if err := checkCloudDevice(id, &d, thinqIDs); err != nil {
			return err
		}
		if err := checkCoffee(id, &d); err != nil {
			return err
		}
		if err := checkTV(id, &d); err != nil {
			return err
		}
		if d.Source == SourceDeLonghi {
			if other := serials[d.Serial]; other != "" {
				return fmt.Errorf("devices.%s.serial: %s уже у прибора %s", id, d.Serial, other)
			}
			serials[d.Serial] = id
		}
		if d.Profile != nil {
			d.Profile.Normalize()
		}
		c.Devices[id] = d
	}
	for _, p := range c.Profiles {
		if p != nil {
			p.Normalize()
		}
	}
	return nil
}

// checkStrip проверяет поля ленты WLED: адрес, пресеты и то, что профиль ленте не задаётся.
func checkStrip(id string, d *Device) error {
	strip := d.Source == SourceWLED
	switch {
	case strip && !hostRe.MatchString(d.Host):
		return fmt.Errorf("devices.%s.host: нужен адрес в домашней сети: имя или IP, порт по желанию", id)
	case d.Source == SourceHomeConnectLocal && !hostRe.MatchString(d.Host):
		return fmt.Errorf("devices.%s.host: нужен адрес прибора в домашней сети: имя или IP, порт по желанию", id)
	case !strip && !slices.Contains(hostSources, d.Source) && d.Host != "":
		return fmt.Errorf("devices.%s.host: задаётся только для источников %v", id, hostSources)
	case !strip && d.Presets != nil:
		return fmt.Errorf("devices.%s.presets: задаётся только для источника %s", id, SourceWLED)
	case strip && d.Profile != nil:
		return fmt.Errorf("devices.%s.profile: профиль ленте не задаётся", id)
	}
	scenes := map[string]int{}
	for _, p := range slices.Sorted(maps.Keys(d.Presets)) {
		scene := d.Presets[p]
		switch {
		case p < 1 || p > maxPreset:
			return fmt.Errorf("devices.%s.presets: пресет %d, допустимо от 1 до %d", id, p, maxPreset)
		case !slices.Contains(alice.ColorScenes, scene):
			return fmt.Errorf("devices.%s.presets: сцена %q, допустимо %v", id, scene, alice.ColorScenes)
		case scenes[scene] != 0:
			return fmt.Errorf("devices.%s.presets: сцена %s у пресетов %d и %d", id, scene, scenes[scene], p)
		}
		scenes[scene] = p
	}
	return nil
}

// hostSources - источники с адресом прибора в домашней сети.
var hostSources = []string{SourceWLED, SourceHomeConnectLocal, SourceDeLonghi, SourceWebOS, SourceSamsungTV}

// checkCloudDevice проверяет deviceId и камеру прибора ThinQ или компонент устройства
// SmartThings: одна пара - у одного прибора. Компонент main шлюз хранит пустым: это то же
// устройство, что без компонента.
func checkCloudDevice(id string, d *Device, used map[string]string) error {
	lg, st, pl := d.Source == SourceThinQ, d.Source == SourceSmartThings, d.Source == SourcePolaris
	if st && d.Location == "main" {
		d.Location = ""
	}
	key := d.Source + "/" + d.DeviceID + "/" + d.Location
	switch {
	case lg && !thinqIDRe.MatchString(d.DeviceID):
		return fmt.Errorf("devices.%s.device_id: нужно от 1 до 128 латинских букв, цифр, _ и -", id)
	case st && !stIDRe.MatchString(d.DeviceID):
		return fmt.Errorf("devices.%s.device_id: нужен deviceId устройства SmartThings: до 64 латинских букв, цифр и -", id)
	case pl && !polarisIDRe.MatchString(d.DeviceID):
		return fmt.Errorf("devices.%s.device_id: нужен идентификатор прибора Polaris из топика: латинские буквы, цифры, _ и -", id)
	case pl && d.Location != "":
		return fmt.Errorf("devices.%s.location: у прибора Polaris камер нет", id)
	case !lg && !st && !pl && d.DeviceID != "":
		return fmt.Errorf("devices.%s.device_id: задаётся только для источников %s и %s", id, SourceThinQ, SourceSmartThings)
	case !lg && !st && !pl && d.Location != "":
		return fmt.Errorf("devices.%s.location: задаётся только для источников %s и %s", id, SourceThinQ, SourceSmartThings)
	case lg && d.Location != "" && !locationRe.MatchString(d.Location):
		return fmt.Errorf("devices.%s.location: нужно от 1 до 32 латинских букв, цифр и _", id)
	case st && d.Location != "" && !componentRe.MatchString(d.Location):
		return fmt.Errorf("devices.%s.location: компонент - до 64 латинских букв, цифр, _, . и -", id)
	case (lg || st || pl) && used[key] != "":
		return fmt.Errorf("devices.%s: прибор %s %s уже у прибора %s", id, d.DeviceID, d.Location, used[key])
	case lg || st || pl:
		used[key] = id
	}
	return nil
}

// broadcastAll - широковещательный адрес всей сети.
var broadcastAll = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// checkTV проверяет поля телевизора. Адрес - только частный IP: телевизору уходит ключ
// клиента, а имя в DNS может указать на чужой компьютер. MAC приводится к виду
// AA:BB:CC:DD:EE:FF.
func checkTV(id string, d *Device) error {
	if err := checkTVApps(id, d); err != nil {
		return err
	}
	if d.Source != SourceWebOS && d.Source != SourceSamsungTV {
		if d.MAC != "" || d.Broadcast != "" {
			return fmt.Errorf("devices.%s: mac и broadcast задаются только для источников %s и %s", id, SourceWebOS, SourceSamsungTV)
		}
		return nil
	}
	if !privateHost(d.Host) {
		return fmt.Errorf("devices.%s.host: нужен IP телевизора в домашней сети", id)
	}
	if d.MAC != "" {
		mac, err := tv.ParseMAC(d.MAC)
		if err != nil {
			return fmt.Errorf("devices.%s.mac: нужен адрес вида AA:BB:CC:DD:EE:FF", id)
		}
		d.MAC = strings.ToUpper(mac.String())
	}
	if d.Broadcast != "" {
		a, err := netip.ParseAddr(d.Broadcast)
		if err != nil || !a.Is4() || !(a.IsPrivate() || a.IsLoopback() || a == broadcastAll) {
			return fmt.Errorf("devices.%s.broadcast: нужен IPv4 домашней сети, например 192.168.1.255, или 255.255.255.255", id)
		}
	}
	return nil
}

// checkTVApps проверяет приложения телевизора LG. Одно приложение - у одного режима: по приложению на
// экране шлюз называет Алисе режим.
func checkTVApps(id string, d *Device) error {
	if d.Apps == nil {
		return nil
	}
	if d.Source != SourceWebOS {
		return fmt.Errorf("devices.%s: apps задаётся только для источника %s", id, SourceWebOS)
	}
	modes := map[string]string{}
	for _, mode := range slices.Sorted(maps.Keys(d.Apps)) {
		app := d.Apps[mode]
		switch {
		case !slices.Contains(TVAppModes, mode):
			return fmt.Errorf("devices.%s.apps: режим %q, допустимо от one до ten", id, mode)
		case !tvAppRe.MatchString(app):
			return fmt.Errorf("devices.%s.apps.%s: нужно от 1 до 128 латинских букв, цифр, точек, _ и -", id, mode)
		case modes[app] != "":
			return fmt.Errorf("devices.%s.apps: приложение %s у режимов %s и %s", id, app, modes[app], mode)
		}
		modes[app] = mode
	}
	return nil
}

// checkCoffee проверяет поля кофемашины De'Longhi. Адрес - только частный IP: машине уходит
// токен аккаунта, а имя в DNS может указать на чужой компьютер. Отпечаток приводится к
// 64 знакам заглавными.
func checkCoffee(id string, d *Device) error {
	if d.Source != SourceDeLonghi {
		if d.Serial != "" || d.Pin != "" || d.ProfileID != nil || d.Recipes != nil || d.DefaultDrink != "" {
			return fmt.Errorf("devices.%s: serial, pin, profile_id, recipes и default_drink задаются только для источника %s",
				id, SourceDeLonghi)
		}
		return nil
	}
	if !privateHost(d.Host) {
		return fmt.Errorf("devices.%s.host: нужен IP кофемашины в домашней сети, порт по желанию", id)
	}
	if !serialRe.MatchString(d.Serial) {
		return fmt.Errorf("devices.%s.serial: нужно от 1 до 64 латинских букв и цифр", id)
	}
	if d.Pin != "" {
		if !pinRe.MatchString(d.Pin) {
			return fmt.Errorf("devices.%s.pin: нужен отпечаток SHA-256: 64 шестнадцатеричных знака", id)
		}
		d.Pin = strings.ToUpper(strings.ReplaceAll(d.Pin, ":", ""))
	}
	if p := d.ProfileID; p != nil && (*p < 0 || *p > maxProfileID) {
		return fmt.Errorf("devices.%s.profile_id: %d, допустимо от 0 до %d", id, *p, maxProfileID)
	}
	numbers := map[int]string{}
	for _, drink := range slices.Sorted(maps.Keys(d.Recipes)) {
		n := d.Recipes[drink]
		switch {
		case !slices.Contains(CoffeeDrinks, drink):
			return fmt.Errorf("devices.%s.recipes: напиток %q, допустимо %v", id, drink, CoffeeDrinks)
		case n < 1 || n > maxRecipe:
			return fmt.Errorf("devices.%s.recipes: рецепт %d, допустимо от 1 до %d", id, n, maxRecipe)
		case numbers[n] != "":
			return fmt.Errorf("devices.%s.recipes: рецепт %d у напитков %s и %s", id, n, numbers[n], drink)
		}
		numbers[n] = drink
	}
	if d.DefaultDrink != "" {
		recipes := d.Recipes
		if len(recipes) == 0 {
			recipes = profile.DeLonghiRecipes
		}
		if _, ok := recipes[d.DefaultDrink]; !ok {
			return fmt.Errorf("devices.%s.default_drink: %q нет в recipes", id, d.DefaultDrink)
		}
	}
	return nil
}

// privateHost - IP в домашней сети: частный, локальный канальный или localhost для
// проверки, с портом по желанию.
func privateHost(host string) bool {
	if !hostRe.MatchString(host) {
		return false
	}
	h := host
	if ap, err := netip.ParseAddrPort(host); err == nil {
		h = ap.Addr().String()
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	if err != nil {
		return false
	}
	return a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLoopback()
}

// checkIoTURL проверяет адрес брокера AWS IoT: wss, а ws - только к своему компьютеру.
func checkIoTURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("нужен адрес вида wss://example.iot.eu-central-1.amazonaws.com/mqtt")
	}
	if u.Scheme == "wss" {
		return nil
	}
	if u.Scheme == "ws" {
		if a, err := netip.ParseAddr(u.Hostname()); (err == nil && a.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return errors.New("нужен wss: токен аккаунта не должен идти по сети открытым текстом")
}

// withHTTPS дополняет адрес без схемы, например api.home-connect.cn, схемой https.
func withHTTPS(s string) string {
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	return "https://" + s
}

// ValidHost сообщает, годится ли адрес прибора в домашней сети: имя или IP, порт по желанию.
func ValidHost(host string) bool {
	return hostRe.MatchString(host)
}

// checkAPIURL проверяет адрес облака: https, а http - только к своему компьютеру, для проверки.
func checkAPIURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("нужен адрес вида https://сервер")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if a, err := netip.ParseAddr(u.Hostname()); (err == nil && a.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return errors.New("нужен https: токены облака не должны идти по сети открытым текстом")
}

// parsePrefix разбирает сеть 10.0.0.0/8 или отдельный адрес 10.0.0.5.
func parsePrefix(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ValidateServe проверяет то, без чего шлюз не может работать. Настройки Яндекса
// необязательны: пока нет client_id и client_secret, шлюз отвечает Яндексу 404.
func (c *Config) ValidateServe() error {
	y := c.Yandex
	switch {
	case c.Listen == "":
		return errors.New("listen: не задан")
	case c.Metrics.Listen == "":
		return errors.New("metrics.listen: не задан")
	case c.Metrics.Listen == c.Listen:
		return errors.New("metrics.listen: совпадает с listen, метрикам нужен свой порт")
	case c.Metrics.Token != "" && len(c.Metrics.Token) < minMetricsTokenLen:
		return fmt.Errorf("metrics.token: нужно не меньше %d символов", minMetricsTokenLen)
	case c.MQTT.Listen == "":
		return errors.New("mqtt.listen: не задан")
	case c.MQTT.Listen == c.Listen || c.MQTT.Listen == c.Metrics.Listen:
		return errors.New("mqtt.listen: совпадает с listen или metrics.listen, брокеру нужен свой порт")
	case c.MQTT.Username == "":
		return errors.New("mqtt.username: не задан")
	case c.MQTT.Password != "" && len(c.MQTT.Password) < minSecretLen:
		return fmt.Errorf("mqtt.password: нужно не меньше %d символов", minSecretLen)
	case (y.ClientID == "") != (y.ClientSecret == ""):
		return errors.New("yandex: client_id и client_secret задаются вместе")
	case y.ClientSecret != "" && len(y.ClientSecret) < minSecretLen:
		return fmt.Errorf("yandex.client_secret: нужно не меньше %d символов", minSecretLen)
	case len(y.RedirectURIs) == 0:
		return errors.New("yandex.redirect_uris: пустой список")
	case (y.SkillID == "") != (y.CallbackToken == ""):
		return errors.New("yandex: skill_id и callback_token задаются вместе")
	case (c.HomeConnect.ClientID == "") != (c.HomeConnect.ClientSecret == ""):
		return errors.New("homeconnect: client_id и client_secret задаются вместе")
	case (c.ThinQ.PAT == "") != (c.ThinQ.Country == ""):
		return errors.New("thinq: pat и country задаются вместе")
	case (c.SmartThings.ClientID == "") != (c.SmartThings.ClientSecret == ""):
		return errors.New("smartthings: client_id и client_secret задаются вместе")
	}
	for _, id := range slices.Sorted(maps.Keys(c.Devices)) {
		switch d := c.Devices[id]; d.Source {
		case SourceDemo:
		case SourceHomeConnectCloud:
			// Приложение с портала не обязательно: вход может быть по токену приложения Home Connect.
		case SourceWLED:
		case SourceHomeConnectLocal:
			// Ключи прибора шлюз проверяет, когда собирает прибор.
		case SourceThinQ:
			if !c.ThinQ.Ready() {
				return fmt.Errorf("devices.%s: источнику %s нужны thinq.pat и thinq.country", id, d.Source)
			}
		case SourceDeLonghi:
			// Вход в аккаунт De'Longhi - на странице «Подключения»: без него машина не на связи.
		case SourcePolaris:
			if c.MQTT.Password == "" {
				return fmt.Errorf("devices.%s: источнику %s нужен встроенный брокер: создайте пароль на странице MQTT", id, d.Source)
			}
		case SourceWebOS, SourceSamsungTV:
			// Разрешение телевизор спрашивает на экране при первом подключении.
		case SourceSmartThings:
			if !c.SmartThings.Ready() {
				return fmt.Errorf("devices.%s: источнику %s нужны smartthings.client_id и client_secret", id, d.Source)
			}
		case "":
			return fmt.Errorf("devices.%s.source: не задан", id)
		default:
			return fmt.Errorf("devices.%s: источник %s ещё не готов", id, d.Source)
		}
	}
	return nil
}

// Check - все проверки настроек перед сохранением: те же, что при чтении файла и при запуске.
func (c *Config) Check() error {
	if err := c.validate(); err != nil {
		return err
	}
	return c.ValidateServe()
}

// savedHeader - первая строка сохранённого файла.
const savedHeader = "# Сохранено веб-интерфейсом hcgateway. Предыдущая версия - в файле .bak рядом.\n"

// marshal - YAML для файла: без значений по умолчанию и без незаданных полей.
func (c *Config) marshal() ([]byte, error) {
	v := c.Clone()
	d := defaults()
	if v.DataDir == d.DataDir {
		v.DataDir = ""
	}
	if v.Listen == d.Listen {
		v.Listen = ""
	}
	if v.Metrics.Listen == d.Metrics.Listen {
		v.Metrics.Listen = ""
	}
	if v.MQTT.Listen == d.MQTT.Listen {
		v.MQTT.Listen = ""
	}
	if v.MQTT.Username == d.MQTT.Username {
		v.MQTT.Username = ""
	}
	if v.HomeConnect.APIURL == d.HomeConnect.APIURL {
		v.HomeConnect.APIURL = ""
	}
	if v.SmartThings.APIURL == d.SmartThings.APIURL {
		v.SmartThings.APIURL = ""
	}
	if slices.Equal(v.Yandex.RedirectURIs, d.Yandex.RedirectURIs) {
		v.Yandex.RedirectURIs = nil
	}
	var buf bytes.Buffer
	buf.WriteString(savedHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
