package web

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
	"hcgateway/internal/appliance"
	"hcgateway/internal/config"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/gateway"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/i18n"
	"hcgateway/internal/profile"
	"hcgateway/internal/statedb"
	"hcgateway/internal/wled"
)

// Страница «Всё о приборе» /devices/<id>/info и её поток SSE /devices/<id>/stream:
// раз в секунду шлюз сравнивает состояние прибора с отправленным и шлёт новое.

const (
	// streamTick - как часто поток проверяет прибор.
	streamTick = time.Second
	// streamKeepAlive - комментарий в поток, если состояние не менялось: прокси не закрывает
	// соединение без данных.
	streamKeepAlive = 15 * time.Second
	// streamMax - сколько живёт поток. Браузер переподключается сам, а шлюз при этом снова
	// проверяет сессию.
	streamMax = 10 * time.Minute
	// maxStreams - предел одновременных потоков.
	maxStreams = 8
)

// streams - открытые потоки SSE.
var streams atomic.Int32

type infoView struct {
	ID, Name, Room, Source, Type, Manufacturer string
	// Fields - адрес, haId, серийный номер и прочее, чем прибор подключён.
	Fields []kv
	// Alice - устройства Алисы прибора: умения и свойства по-русски.
	Alice []aliceInfo
	Live  snapshot
	// Что умеет прибор.
	Programs []string
	Options  []optionRow
	Commands []string
	Events   []string
	// Profile - действующий профиль в YAML, Origins - откуда каждое его поле.
	Profile string
	Origins []kv
}

type aliceInfo struct {
	ID, Type, Name string
	Items          []aliceItem
}

type optionRow struct {
	Key, Limits, Values string
	Setting             bool
}

// snapshot - то, что меняется: его отдаёт поток SSE.
type snapshot struct {
	Online  bool   `json:"online"`
	Summary string `json:"summary"`
	// Tone - вид метки состояния (stateTone); Notice - что требует внимания (status).
	Tone   string `json:"tone"`
	Notice string `json:"notice"`
	// Mappings - сводка сопоставлений для вкладки «Сопоставления» страницы прибора.
	Mappings []mapRow   `json:"mappings"`
	Values   []valueRow `json:"values"`
	// EmptyFavorites - значения незанятых избранных: их десятки, и страница их сворачивает.
	EmptyFavorites []valueRow   `json:"emptyFavorites"`
	Alice          []aliceState `json:"alice"`
	History        []historyRow `json:"history"`
}

type valueRow struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Mapping string `json:"mapping"`
	// Last - значение не текущее: «последнее, 3 ч назад»; пусто - прибор сообщает его сейчас.
	Last string `json:"last,omitempty"`
}

type historyRow struct {
	Time  string `json:"time"`
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

var kindLabels = map[string]string{"event": "событие", "value": "значение", "online": "связь", statedb.GapKind: "шлюз"}

func (s *Server) infoPage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	cfg, _ := s.file.Current()
	dc, ok := cfg.Devices[id]
	dev, built := s.gw.Device(id)
	if !ok || !built {
		s.notFound(w, sess)
		return
	}
	v := infoView{ID: id, Name: dc.Name, Room: dc.Room, Source: dc.Source, Live: s.snapshot(id)}
	v.Fields = deviceFields(dc)
	for _, info := range dev.Info() {
		a := aliceInfo{ID: info.ID, Type: info.Type, Name: info.Name}
		v.Manufacturer = info.DeviceInfo.Manufacturer
		for _, c := range append(slices.Clone(info.Capabilities), info.Properties...) {
			a.Items = append(a.Items, describe(c))
		}
		v.Alice = append(v.Alice, a)
	}
	if hc, ok := s.gw.HomeConnect(id); ok {
		desc := hc.Appliance.Describe()
		v.Type, v.Programs, v.Commands, v.Events = desc.Type, desc.Programs, desc.Commands, desc.Events
		if len(v.Programs) > 50 && dc.Source == config.SourceDeLonghi {
			// Рецепты 1-255 - не список программ машины, а допустимые номера.
			v.Programs = []string{delonghi.ProgramPrefix + "1 … " + delonghi.ProgramPrefix + "255"}
		}
		for _, k := range slices.Sorted(maps.Keys(desc.Options)) {
			o := desc.Options[k]
			row := optionRow{Key: k, Setting: o.Setting, Values: strings.Join(o.Values, ", ")}
			if o.Max > o.Min {
				row.Limits = fmt.Sprintf("от %s до %s, шаг %s", formatValue(o.Min), formatValue(o.Max), formatValue(o.Step))
			}
			v.Options = append(v.Options, row)
		}
		if out, err := yaml.Marshal(hc.Profile); err == nil {
			v.Profile = string(out)
		}
		t := s.infoTarget(cfg, dc, desc.Type)
		for _, f := range profileFields {
			v.Origins = append(v.Origins, kv{f.name, t.origin(f.name)})
		}
	}
	s.render(w, http.StatusOK, "info", s.newPage(sess, "devices", dc.Name, v))
}

// infoTarget - слои профиля прибора, как в редакторе: по ним видно, откуда поле.
func (s *Server) infoTarget(cfg *config.Config, dc config.Device, haType string) *editTarget {
	builtin := profile.BuiltinProfile(haType)
	if dc.Source == config.SourceDeLonghi {
		builtin = profile.Merge(builtin, profile.DeLonghiLayer(dc.Recipes, dc.DefaultDrink))
	}
	if dc.Source == config.SourceWebOS {
		builtin = profile.Merge(builtin, profile.TVAppsLayer(dc.Apps, nil))
	}
	return &editTarget{builtin: builtin, forType: cfg.Profiles[haType], forDevice: dc.Profile, device: true}
}

// deviceFields - чем прибор подключён: только заданные поля.
func deviceFields(dc config.Device) []kv {
	var out []kv
	add := func(k, v string) {
		if v != "" {
			out = append(out, kv{k, v})
		}
	}
	add("haId", dc.HaID)
	add("адрес", dc.Host)
	add("deviceId", dc.DeviceID)
	add("камера", dc.Location)
	add("серийный номер", dc.Serial)
	add("отпечаток сертификата", groups(dc.Pin))
	if dc.ProfileID != nil {
		add("профиль пользователя машины", fmt.Sprint(*dc.ProfileID))
	}
	return out
}

// snapshot - связь, значения с сопоставлениями, состояния устройств Алисы и история.
func (s *Server) snapshot(id string) snapshot {
	var sn snapshot
	dev, ok := s.gw.Device(id)
	if !ok {
		return sn
	}
	cfg, _ := s.file.Current()
	st := s.status(id, cfg.Devices[id])
	sn.Online, sn.Summary, sn.Tone, sn.Notice, sn.Alice = st.Online, st.Text, st.Tone, st.Notice, deviceState(dev).Alice
	hc, ok := dev.(*hcdevice.Device)
	if !ok {
		if strip, ok := dev.(*wled.Device); ok {
			current, online := strip.Values()
			sn.Values, _ = s.valueRows(id, current, online, stripMappings)
		}
		return sn
	}
	ast := hc.Appliance.State()
	desc := hc.Appliance.Describe()
	active := hcdevice.ActiveProfile(hc.Profile, &desc)
	sn.Mappings = mapRows(active, &ast, hc)
	mapping := mappings(active)
	current := maps.Clone(ast.Values)
	if current == nil {
		current = map[string]any{}
	}
	if ast.ActiveProgram != "" {
		current[gateway.ValueProgram] = ast.ActiveProgram
	}
	if ast.SelectedProgram != "" {
		current[gateway.ValueSelected] = ast.SelectedProgram
	}
	sn.Values, sn.EmptyFavorites = s.valueRows(id, current, ast.Online, mapping)
	for _, c := range hc.History() {
		// С датой: история переживает перезапуск и может быть длиннее суток.
		sn.History = append(sn.History, historyRow{Time: c.Time.Local().Format("02.01 15:04:05"), Kind: kindLabels[c.Kind], Key: c.Key, Value: c.Value})
	}
	return sn
}

// stripMappings - кто берёт значения ленты WLED.
var stripMappings = map[string]string{"on": "on_off: питание", "brightness": "range brightness", "rgb": "color_setting: цвет",
	"scene": "color_setting: сцена", "temperature_k": "color_setting: цветовая температура", "amperage": "float amperage"}

// valueRows - строки значений прибора: текущие значения и последние полученные с пометкой. Если
// прибор на связи, с пометкой идут только значения, которых он сейчас не сообщает.
func (s *Server) valueRows(id string, current map[string]any, online bool, mapping map[string]string) (rows, empty []valueRow) {
	all := make(map[string]any, len(current))
	maps.Copy(all, current)
	stale, gone := map[string]bool{}, map[string]time.Time{}
	for _, v := range s.gw.LastValues(id) {
		if _, ok := all[v.Key]; !ok {
			all[v.Key], stale[v.Key], gone[v.Key] = v.Value, true, v.Gone
		}
	}
	var since time.Time
	if !online {
		since, _, _ = s.gw.Offline(id)
	}
	row := func(k string) valueRow {
		r := valueRow{Key: k, Value: formatValue(all[k]), Mapping: mapping[k]}
		if !online || stale[k] {
			at := gone[k]
			if at.IsZero() {
				at = since
			}
			r.Last = lastText(at)
		}
		return r
	}
	// У программы подпись поля - по самой программе, и в таблице она идёт первой.
	for _, k := range []string{gateway.ValueProgram, gateway.ValueSelected} {
		if v, ok := all[k]; ok {
			r := row(k)
			r.Mapping = mapping[formatValue(v)]
			rows = append(rows, r)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(all)) {
		if k == gateway.ValueProgram || k == gateway.ValueSelected {
			continue
		}
		r := row(k)
		if r.Mapping == "" && emptyFavorite(all, k) {
			empty = append(empty, r)
			continue
		}
		rows = append(rows, r)
	}
	return rows, empty
}

// lastText - пометка значения, которое прибор сейчас не сообщает: «последнее, 3 ч назад». Часы, а не
// время: часовой пояс контейнера может не совпадать с домашним.
func lastText(at time.Time) string {
	if !at.IsZero() {
		if a := ago(time.Since(at)); a != "" {
			return "последнее," + a + " назад"
		}
	}
	return "последнее"
}

// favoriteRe - значение избранного прибора Home Connect и номер избранного.
var favoriteRe = regexp.MustCompile(`^BSH\.Common\.Setting\.Favorite\.([0-9]+)\.`)

// favoriteOff - значение Functionality незанятого избранного.
const favoriteOff = "BSH.Common.EnumType.Favorite.Functionality.Off"

// emptyFavorite сообщает, что key - значение незанятого избранного.
func emptyFavorite(values map[string]any, key string) bool {
	m := favoriteRe.FindStringSubmatch(key)
	return m != nil && values["BSH.Common.Setting.Favorite."+m[1]+".Functionality"] == favoriteOff
}

// implicit - значения Home Connect, которые шлюз читает сам, без полей профиля: идёт ли
// программа и можно ли её запустить. Если значение берёт поле профиля, видна подпись поля.
var implicit = map[string]string{
	appliance.StatusOperationState:     "шлюз: идёт ли программа",
	appliance.StatusRemoteStartAllowed: "шлюз: разрешён ли удалённый старт",
	appliance.StatusDoorState:          "шлюз: закрыта ли дверца перед стартом",
}

// mappings - имя функции прибора -> что из него берёт Алиса или сам шлюз.
func mappings(p *profile.Profile) map[string]string {
	out := map[string]string{}
	add := func(k, v string) {
		if k == "" {
			return
		}
		if out[k] != "" {
			out[k] += "; "
		}
		out[k] += v
	}
	if pw := p.Power; pw != nil {
		add(pw.Setting, "on_off: питание")
		add(pw.State, "on_off: включён, кроме "+strings.Join(pw.Idle, ", "))
		add(pw.Allowed, "on_off: разрешение")
	} else if p.OnOff == "power" {
		add(appliance.SettingPowerState, "on_off: питание")
	}
	if p.OnOff == "program" {
		add(appliance.StatusOperationState, "on_off: включён, пока идёт программа")
	}
	for _, m := range p.Modes {
		add(m.Option, "mode "+m.Instance)
		for k, v := range m.Programs {
			add(k, "mode "+m.Instance+": "+v)
		}
	}
	for _, r := range p.Ranges {
		add(r.Option, "range "+r.Instance)
	}
	for _, t := range p.Toggles {
		add(t.Setting, "toggle "+t.Instance)
		if t.Instance == "pause" && t.Setting == "" {
			add(appliance.StatusOperationState, "toggle pause: на паузе ли программа")
		}
	}
	if c := p.Color; c != nil {
		add(c.Setting, "color_setting: цвет")
		add(c.Hue, "color_setting: оттенок")
		add(c.Saturation, "color_setting: насыщенность")
		add(c.Temperature, "color_setting: цветовая температура")
	}
	for _, pr := range p.Properties {
		label := pr.Type + " " + pr.Instance
		if c := conversionLabel(&pr); c != "" {
			label += ", " + c
		}
		for _, name := range profile.StatusNames(pr.Status) {
			add(name, label)
		}
	}
	if e := p.Events; e != nil {
		for _, k := range slices.Concat(e.FoodLevel, e.FoodLevelIfKnown) {
			add(k, "события: food_level")
		}
		for _, k := range slices.Concat(e.WaterLevel, e.WaterLevelIfKnown) {
			add(k, "события: water_level")
		}
		for _, k := range slices.Sorted(maps.Keys(e.Button)) {
			name, _, _ := strings.Cut(k, "=")
			add(name, "события: button "+e.Button[k])
		}
	}
	for k, v := range implicit {
		if out[k] == "" {
			out[k] = v
		}
	}
	for k := range out {
		if alarm, ok := strings.CutPrefix(k, delonghi.AlarmPrefix); ok && alarmName(alarm) != "" {
			out[k] += "; " + alarmName(alarm)
		}
	}
	return out
}

// stream - поток SSE состояния прибора: событие state с JSON снимка при каждом изменении.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, _ *session) {
	id := r.PathValue("id")
	if _, ok := s.gw.Device(id); !ok {
		http.NotFound(w, r)
		return
	}
	s.sse(w, r, "state", func() any { return s.snapshot(id) })
}

// sse - поток SSE: раз в секунду снимок snap сравнивается с отправленным, новый уходит
// событием event с JSON.
func (s *Server) sse(w http.ResponseWriter, r *http.Request, event string, snap func() any) {
	if streams.Add(1) > maxStreams {
		streams.Add(-1)
		plainError(w, "Слишком много потоков", http.StatusServiceUnavailable)
		return
	}
	defer streams.Add(-1)
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	tick := time.NewTicker(streamTick)
	defer tick.Stop()
	end := time.Now().Add(streamMax)
	var last []byte
	quiet := time.Now()
	for {
		now := time.Now()
		// Сервер ограничивает ответ 30 с: поток продлевает срок перед каждой записью.
		_ = rc.SetWriteDeadline(now.Add(10 * time.Second))
		data, _ := json.Marshal(snap())
		data = i18n.JSON(langOf(w), data)
		var err error
		switch {
		case string(data) != string(last):
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			last, quiet = data, now
		case now.Sub(quiet) >= streamKeepAlive:
			_, err = fmt.Fprint(w, ": keep-alive\n\n")
			quiet = now
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil || now.After(end) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}
