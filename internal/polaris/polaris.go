// Package polaris - приборы Polaris IQ Home через встроенный брокер MQTT. Прибор подключается к
// брокеру сам: адрес и пароль вписывают в пункте «Настройки MQTT» приложения Polaris. Он публикует
// polaris/<id>/state/<имя> и слушает polaris/<id>/control/<имя>; у старых прошивок
// polaris/<тип>/<id>/... . Имена функций прибора в шлюзе - <имя>, например sensor/temperature.
package polaris

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/atomicfile"
	"hcgateway/internal/mqttbroker"
	"hcgateway/internal/profile"
)

const (
	// root - корень топиков приборов.
	root = "polaris"
	// maxFound - сколько приборов помнит поиск; maxValues - сколько значений на прибор; maxValueBytes -
	// предел значения: приборы шлют короткие строки, остальное шлюз не хранит.
	maxFound      = 64
	maxValues     = 256
	maxValueBytes = 1024
	maxFile       = 16 << 10
)

// partRe - допустимая часть идентификатора прибора: он становится именем файла и частью топика.
var partRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// kettles - номера типов чайников в топике state/devtype. Других приборов шлюз пока сам не знает:
// их можно сопоставить своим профилем.
var kettles = []string{"2", "6", "8", "29", "36", "37", "38", "51", "52", "53", "54", "56", "57", "58", "59", "60", "61", "62", "63",
	"67", "82", "83", "84", "85", "86", "97", "98", "105", "106", "117", "121", "139", "164", "165", "175", "176", "177", "185", "188",
	"189", "194", "196", "205", "208", "209", "223", "244", "245", "253", "254", "255", "260", "262", "263", "271", "275", "294", "308"}

// Значения топика mode чайника: 0 - выключен, 1 - кипячение, 2 - кипячение с удержанием, 3 - разогрев,
// 4 - разогрев с удержанием, 5 - IQ-кипячение, 6 - чайная церемония.
const (
	modeOff  = "0"
	modeHeat = "3"
)

// Функции чайника, которые шлюз разбирает сам.
const (
	KeyMode        = "mode"
	KeyTemperature = "temperature"
	KeyDevType     = "devtype"
	KeyErrorCode   = "error/code"
	// KeyTea - вид чая: виртуальная функция шлюза. Чай - это температура и режим разогрева.
	KeyTea = "tea"
	// KeyWaterAlarm - вода заканчивается: виртуальная функция из error/code.
	KeyWaterAlarm = "Polaris.Alarm.Water"
	// KeyEmptyBase - чайник снят с базы.
	KeyEmptyBase = "error/empty_base"
	// KeySensor - температура воды.
	KeySensor = "sensor/temperature"
	// EventBoilFinished - чайник перестал работать сам: виртуальное событие шлюза.
	EventBoilFinished = "Polaris.Event.BoilFinished"
)

const (
	// finishWait - сколько шлюз ждёт error/empty_base после перехода mode в 0: снятый с базы чайник
	// присылает оба значения, порядок шлюзу неизвестен.
	finishWait = 2 * time.Second
	// offWait - сколько после команды выключения переход mode в 0 считается её результатом.
	offWait = 10 * time.Second
)

// Пределы температуры нагрева.
const (
	minTemperature = 30
	maxTemperature = 100
)

// teaTemperatures - температура нагрева по виду чая.
var teaTemperatures = map[string]int{"black": 100, "green": 80, "flower": 80, "red": 90, "puerh": 95, "oolong": 90, "white": 65, "herbal": 90}

// numeric - функции с числовым значением. Остальные - строки: «00» и «05» в program_data не числа.
var numeric = map[string]bool{KeyTemperature: true, KeySensor: true, "weight": true, "diag/rssi": true, "timezone": true, "volume": true}

// Found - прибор, который подключался к брокеру.
type Found struct {
	ID, DevType, Client string
	Seen                time.Time
}

// Hub - приборы Polaris на встроенном брокере.
type Hub struct {
	dir     string
	log     *slog.Logger
	changes chan struct{}

	mu sync.Mutex
	// broker - nil, пока порт брокера закрыт: у прибора нет связи.
	broker *mqttbroker.Broker
	found  map[string]*Found
	list   map[string]*Appliance
}

// New создаёт приборы Polaris. dir - каталог файлов приборов, data_dir/polaris.
func New(dir string, log *slog.Logger) *Hub {
	return &Hub{dir: dir, log: log, changes: make(chan struct{}, 1), found: map[string]*Found{}, list: map[string]*Appliance{}}
}

// Attach подключает приборы к брокеру. Вызывается до запуска брокера.
func (h *Hub) Attach(b *mqttbroker.Broker) {
	h.mu.Lock()
	h.broker = b
	h.mu.Unlock()
	b.OnMessage(h.message)
	b.OnClientGone(h.clientGone)
}

// Changes сообщает, что шлюз узнал о приборе новое: тип или список значений. Алисе нужен новый список
// устройств.
func (h *Hub) Changes() <-chan struct{} {
	return h.changes
}

func (h *Hub) changed() {
	select {
	case h.changes <- struct{}{}:
	default:
	}
}

// Found - приборы, которые подключались к брокеру с запуска шлюза.
func (h *Hub) Found() []Found {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Found, 0, len(h.found))
	for _, id := range slices.Sorted(maps.Keys(h.found)) {
		out = append(out, *h.found[id])
	}
	return out
}

// Kettle сообщает, что номер типа - чайник.
func Kettle(devType string) bool {
	return slices.Contains(kettles, devType)
}

// Appliance - прибор с идентификатором id: <mac> или, у старых прошивок, <тип>/<id>.
func (h *Hub) Appliance(id string) *Appliance {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.list[id]
	if a == nil {
		a = newAppliance(h, id)
		h.list[id] = a
	}
	return a
}

// parseTopic разбирает топик состояния прибора. ok - топик прибора Polaris с допустимым идентификатором.
func parseTopic(topic string) (id, key string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) < 4 || parts[0] != root {
		return "", "", false
	}
	switch {
	case parts[2] == "state" && partRe.MatchString(parts[1]):
		return parts[1], strings.Join(parts[3:], "/"), true
	case len(parts) > 4 && parts[3] == "state" && partRe.MatchString(parts[1]) && partRe.MatchString(parts[2]):
		return parts[1] + "/" + parts[2], strings.Join(parts[4:], "/"), true
	}
	return "", "", false
}

// message разбирает сообщение от клиента брокера. Сохранённые сообщения из базы сюда не приходят:
// значение считается текущим, только если прибор сам прислал его с запуска шлюза.
func (h *Hub) message(m mqttbroker.Message) {
	id, key, ok := parseTopic(m.Topic)
	if !ok || len(key) > mqttbroker.MaxTopicBytes || len(m.Payload) > maxValueBytes {
		return
	}
	value := string(m.Payload)
	h.mu.Lock()
	f := h.found[id]
	if f == nil && len(h.found) < maxFound {
		f = &Found{ID: id}
		h.found[id] = f
	}
	if f != nil {
		f.Client, f.Seen = m.Client, time.Now()
		if key == KeyDevType {
			f.DevType = value
		}
	}
	a := h.list[id]
	h.mu.Unlock()
	if a != nil {
		a.message(m.Client, key, value)
	}
}

// clientGone - клиент брокера отключился: его приборы без связи.
func (h *Hub) clientGone(client string) {
	h.mu.Lock()
	list := slices.Collect(maps.Values(h.list))
	h.mu.Unlock()
	for _, a := range list {
		a.clientGone(client)
	}
}

// publish отправляет команду прибору.
func (h *Hub) publish(id, key, value string) error {
	h.mu.Lock()
	b := h.broker
	h.mu.Unlock()
	if b == nil {
		return appliance.ErrUnreachable
	}
	if err := b.Publish(root+"/"+id+"/control/"+key, []byte(value), false, 0); err != nil {
		return fmt.Errorf("%w: %v", appliance.ErrUnreachable, err)
	}
	return nil
}

// file - что шлюз помнит о приборе между запусками: без этого выключенный чайник пропал бы из Алисы.
type file struct {
	DevType string   `json:"devtype,omitempty"`
	Reports []string `json:"reports,omitempty"`
}

func (h *Hub) path(id string) string {
	return filepath.Join(h.dir, strings.ReplaceAll(id, "/", "_")+".json")
}

func (h *Hub) load(id string) file {
	var f file
	data, err := os.ReadFile(h.path(id))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		h.log.Warn("Polaris: файл прибора не прочитан", "device", id, "err", err)
	case len(data) > maxFile:
		h.log.Warn("Polaris: файл прибора больше предела", "device", id)
	default:
		if err := json.Unmarshal(data, &f); err != nil {
			h.log.Warn("Polaris: файл прибора не разобран", "device", id, "err", err)
		}
	}
	return f
}

func (h *Hub) save(id string, f file) {
	data, err := json.Marshal(f)
	if err == nil {
		err = atomicfile.Write(h.path(id), data)
	}
	if err != nil {
		h.log.Warn("Polaris: файл прибора не записан", "device", id, "err", err)
	}
}

// parseValue - значение прибора: флаг, число для числовых функций, иначе строка.
func parseValue(key, value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	}
	if numeric[key] {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return value
}

// profileType - тип прибора для профиля; пусто - прибор ещё не сообщил свой тип.
func profileType(devType string) string {
	switch {
	case devType == "":
		return ""
	case Kettle(devType):
		return profile.PolarisKettleType
	}
	return "Polaris" + devType
}
