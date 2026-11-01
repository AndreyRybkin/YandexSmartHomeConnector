package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/auth"
	"hcgateway/internal/config"
	"hcgateway/internal/hcapi"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/hclocal"
	"hcgateway/internal/profile"
	"hcgateway/internal/thinq"
	"hcgateway/internal/wled"
)

// funcs - функции шаблонов.
var funcs = template.FuncMap{"short": short, "source": sourceName, "typeName": typeName, "yandexType": yandexTypeName,
	"stUsage": stUsageText, "stepOf": stepOf}

// stepView - заголовок шага подключения: номер, название и состояние шага.
type stepView struct {
	N     int
	Title string
	S     stepState
}

func stepOf(n int, title string, s stepState) stepView {
	return stepView{N: n, Title: title, S: s}
}

// typeRe - допустимое имя типа прибора в адресе страницы: у SmartThings - с точкой.
var typeRe = regexp.MustCompile(`^[A-Za-z0-9_.]{1,64}$`)

// errorTexts - коды ошибок команд и что они значат.
var errorTexts = map[string]string{
	alice.CodeDoorOpen:       "дверца открыта",
	alice.CodeRemoteDisabled: "удалённый старт на приборе не разрешён",
	alice.CodeUnreachable:    "прибор недоступен или не ответил за 3 с",
	alice.CodeBusy:           "прибор занят",
	alice.CodeInvalidValue:   "значение неверного типа, вне пределов или без пары в профиле",
	alice.CodeInvalidAction:  "умения нет в профиле или прибор не поддерживает команду",
	alice.CodeWrongMode:      "команда недоступна в текущем состоянии",
	alice.CodeNotFound:       "устройства нет в настройках",
	alice.CodeInternal:       "другая ошибка",
}

// Вход и выход.

type loginView struct {
	Error   string
	NoUser  bool
	Command string
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, errText string) {
	_, ok := s.auth.Users.Current()
	s.render(w, status, "login", &page{Title: "Вход", Data: loginView{Error: errText, NoUser: !ok,
		Command: "docker compose exec hcgateway /hcgateway -config /config/config.yaml web-user"}})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.session(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "")
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	res, u := s.auth.Login(r, auth.WhereWeb, r.PostForm.Get("username"), r.PostForm.Get("password"))
	switch res {
	case auth.Banned:
		s.renderLogin(w, http.StatusTooManyRequests, "Слишком много попыток, попробуйте позже")
		return
	case auth.Busy:
		s.renderLogin(w, http.StatusServiceUnavailable, "Сервер занят, попробуйте позже")
		return
	case auth.Wrong:
		s.renderLogin(w, http.StatusUnauthorized, "Неверный логин или пароль")
		return
	}
	token, err := s.sessions.create(u)
	if err != nil {
		s.log.Error("сессия веб-интерфейса", "err", err)
		http.Error(w, "Внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess *session) {
	s.sessions.remove(sess)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Приборы.

// deviceRow - прибор в списке. State - состояние одной строкой; у прибора, который ждёт
// данных облака (Waiting), - чего он ждёт. Switch - у устройства Алисы есть умение on_off,
// и прибор на связи: в списке есть кнопка питания, On - включён ли он. Problem и Notice -
// что требует внимания (status).
type deviceRow struct {
	ID     string `json:"id"`
	Name   string `json:"-"`
	Room   string `json:"-"`
	Source string `json:"-"`
	// Icon - значок прибора из набора интерфейса; Tone - вид метки состояния (stateTone).
	Icon    string `json:"-"`
	Tone    string `json:"tone"`
	Online  bool   `json:"online"`
	Waiting bool   `json:"waiting"`
	State   string `json:"state"`
	Switch  bool   `json:"switch"`
	On      bool   `json:"on"`
	Problem string `json:"problem"`
	Notice  string `json:"notice"`
}

// roomGroup - приборы одной комнаты по имени. Room пусто - приборы без комнаты.
type roomGroup struct {
	Room    string
	Devices []deviceRow
}

type devicesView struct {
	Rooms []roomGroup
	// Attention - полоса «Требует внимания» (attention), пусто - полосы нет. Problems -
	// показаны только приборы, которые требуют внимания. ProblemList - причины для скрипта.
	Attention   string
	Problems    bool
	ProblemList string
	// AddOpen - форма добавления открыта: она заполнена, в ней ошибка или приборов нет.
	AddOpen bool
	// Names - имена и комнаты приборов для предупреждения о таком же имени (namesJSON).
	Names string
	// Taken - занятые идентификаторы через пробел: по ним скрипт подбирает свободный.
	Taken string
	// RoomNames - комнаты приборов: подсказки для поля комнаты.
	RoomNames []string
	Form      config.Device
	FormID    string
	Error     string
	Sources   []choice
	DemoKinds []choice
	// HaIDs - приборы аккаунта Home Connect, которых ещё нет в настройках.
	HaIDs []choice
	// ThinQIDs - приборы аккаунта LG, которых ещё нет в настройках.
	ThinQIDs []choice
	// CoffeeSerials - кофемашины аккаунта De'Longhi, которых ещё нет в настройках.
	CoffeeSerials []choice
	// STIDs - устройства аккаунта SmartThings.
	STIDs []choice
}

// sourceLabels - источники, которые можно выбрать при добавлении прибора.
var sourceLabels = map[string]string{config.SourceDemo: "Демо-прибор в памяти шлюза",
	config.SourceHomeConnectCloud: "Прибор из облака Home Connect", config.SourceHomeConnectLocal: "Прибор Home Connect в домашней сети",
	config.SourceWLED: "Лента WLED в домашней сети", config.SourceThinQ: "Прибор LG из ThinQ",
	config.SourceDeLonghi: "Кофемашина De'Longhi в домашней сети", config.SourceWebOS: "Телевизор LG webOS в домашней сети",
	config.SourceSamsungTV: "Телевизор Samsung в домашней сети", config.SourceSmartThings: "Устройство из SmartThings"}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request, sess *session) {
	// Ссылка «Добавить» со страницы «Подключения» передаёт haId прибора аккаунта.
	var form config.Device
	if haID := r.URL.Query().Get("ha_id"); haID != "" {
		form = config.Device{Source: config.SourceHomeConnectCloud, HaID: haID}
		for _, h := range s.gw.Cloud.Account() {
			if h.HaID == haID {
				form.Name = h.Name
			}
		}
	}
	if haID := r.URL.Query().Get("local_id"); haID != "" {
		form = config.Device{Source: config.SourceHomeConnectLocal, HaID: haID}
	}
	if id := r.URL.Query().Get("thinq_id"); id != "" {
		form = config.Device{Source: config.SourceThinQ, DeviceID: id, Location: r.URL.Query().Get("location")}
		for _, d := range s.gw.ThinQ.Account() {
			if d.ID == id {
				form.Name = d.Alias
			}
		}
		if form.Location != "" {
			form.Name += " - " + locationLabel(form.Location)
		}
	}
	if serial := r.URL.Query().Get("delonghi_serial"); serial != "" {
		form = config.Device{Source: config.SourceDeLonghi, Serial: serial, Name: "Кофемашина"}
	}
	if id := r.URL.Query().Get("st_id"); id != "" {
		form = s.stFill(id, r.URL.Query().Get("component"))
	}
	// Ссылка «Добавить» из поиска в домашней сети передаёт номер находки, а не её адрес.
	if f, ok := s.foundAt(r.URL.Query().Get("found")); ok {
		form = foundForm(f)
	}
	s.renderDevices(w, sess, http.StatusOK, "", form, "", r.URL.Query().Get("problems") != "")
}

// renderDevices - страница «Приборы». problems - показать только приборы, которые требуют
// внимания.
func (s *Server) renderDevices(w http.ResponseWriter, sess *session, status int, id string, form config.Device, errText string, problems bool) {
	cfg, _ := s.file.Current()
	rows := s.deviceRows(cfg)
	v := devicesView{FormID: id, Form: form, Error: errText, Rooms: rooms(slices.Clone(rows)),
		Attention: attention(rows), Problems: problems, ProblemList: problemList(), Names: namesJSON(cfg),
		AddOpen: errText != "" || form.Source != "" || len(cfg.Devices) == 0,
		Taken:   strings.Join(slices.Sorted(maps.Keys(cfg.Devices)), " "), RoomNames: roomNames(cfg),
		Sources: plain([]string{config.SourceDemo, config.SourceHomeConnectCloud, config.SourceHomeConnectLocal,
			config.SourceWLED, config.SourceThinQ, config.SourceDeLonghi, config.SourceWebOS, config.SourceSamsungTV,
			config.SourceSmartThings}, form.Source, "", sourceLabels),
		DemoKinds: plain(appliance.DemoKinds, form.Demo, "", typeNames),
		HaIDs:     s.haIDChoices(cfg, "", form.HaID),
		ThinQIDs:  s.thinqChoices(cfg, "", form.DeviceID), CoffeeSerials: s.coffeeChoices(cfg, form.Serial),
		STIDs: s.stChoices(form.DeviceID)}
	p := s.newPage(sess, "devices", "Приборы", v)
	p.Steps = len(s.steps(cfg))
	s.render(w, status, "devices", p)
}

// roomNames - комнаты приборов без повторов: одна и та же комната пишется одинаково, и
// приборы в списке собираются вместе.
func roomNames(cfg *config.Config) []string {
	var out []string
	for _, d := range cfg.Devices {
		out = append(out, d.Room)
	}
	return uniq(out)
}

// rooms - приборы по комнатам: комнаты по алфавиту, приборы без комнаты - последними.
func rooms(rows []deviceRow) []roomGroup {
	slices.SortFunc(rows, func(a, b deviceRow) int {
		switch {
		case a.Room == "" && b.Room != "":
			return 1
		case a.Room != "" && b.Room == "":
			return -1
		}
		if c := strings.Compare(strings.ToLower(a.Room), strings.ToLower(b.Room)); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	var out []roomGroup
	for _, row := range rows {
		if len(out) == 0 || out[len(out)-1].Room != row.Room {
			out = append(out, roomGroup{Room: row.Room})
		}
		g := &out[len(out)-1]
		g.Devices = append(g.Devices, row)
	}
	return out
}

// deviceRows - строки списка приборов в порядке идентификаторов.
func (s *Server) deviceRows(cfg *config.Config) []deviceRow {
	var out []deviceRow
	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		d := cfg.Devices[id]
		st := s.status(id, d)
		out = append(out, deviceRow{ID: id, Name: d.Name, Room: d.Room, Source: d.Source, Icon: s.iconOf(id, d), Tone: st.Tone,
			Online: st.Online, Waiting: st.Waiting, State: st.Text, Switch: st.Switch, On: st.On,
			Problem: st.Problem, Notice: st.Notice})
	}
	return out
}

// iconOf - значок прибора: свой из настроек, иначе по типу прибора или устройства Алисы.
func (s *Server) iconOf(id string, d config.Device) string {
	if d.Icon != "" {
		return d.Icon
	}
	if hc, ok := s.gw.HomeConnect(id); ok {
		return deviceIcon(hc.Appliance.Describe().Type, hc.Profile.YandexType)
	}
	if dev, ok := s.gw.Device(id); ok {
		if infos := dev.Info(); len(infos) > 0 {
			return deviceIcon("", infos[0].Type)
		}
	}
	return "device"
}

// sameName - другой прибор той же комнаты с тем же именем: голосом их не различить. self -
// прибор, которому ищут пару; пусто - такого нет.
func sameName(cfg *config.Config, self, name, room string) string {
	key := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		if d := cfg.Devices[id]; id != self && key(d.Name) == key(name) && key(d.Room) == key(room) {
			return id
		}
	}
	return ""
}

// voiceText - как Алиса знает прибор: имя и комната.
func voiceText(name, room string) string {
	if room = strings.TrimSpace(room); room == "" {
		return "В Алисе: «" + strings.TrimSpace(name) + "», без комнаты"
	}
	return "В Алисе: «" + strings.TrimSpace(name) + "», комната «" + room + "»"
}

// dupeText - предупреждение о приборе other с тем же именем в той же комнате.
func dupeText(other, room string) string {
	if room = strings.TrimSpace(room); room == "" {
		return "Без комнаты уже есть прибор «" + other + "»: голосом их не различить."
	}
	return "В комнате «" + room + "» уже есть прибор «" + other + "»: голосом их не различить."
}

// namesJSON - имена и комнаты приборов для скрипта: он предупреждает о таком же имени в
// комнате, пока владелец вводит имя.
func namesJSON(cfg *config.Config) string {
	type entry struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Room string `json:"room"`
	}
	var list []entry
	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		list = append(list, entry{ID: id, Name: cfg.Devices[id].Name, Room: cfg.Devices[id].Room})
	}
	out, _ := json.Marshal(list)
	return string(out)
}

// power - есть ли у устройства Алисы умение on_off и включено ли оно.
func power(dev alice.Device, id string) (has, on bool) {
	for _, st := range dev.States() {
		if st.ID != id {
			continue
		}
		for _, c := range st.Capabilities {
			if sv, ok := c.State.(alice.StateValue); ok && c.Type == alice.CapOnOff {
				on, _ = sv.Value.(bool)
				return true, on
			}
		}
	}
	return false, false
}

// listStream - поток SSE списка приборов: событие list с JSON строк при каждом изменении.
func (s *Server) listStream(w http.ResponseWriter, r *http.Request, _ *session) {
	s.sse(w, r, "list", func() any {
		cfg, _ := s.file.Current()
		return s.deviceRows(cfg)
	})
}

// haIDChoices - приборы Home Connect для выбора: из аккаунта облака и с ключами для домашней
// сети. В списке те, что не заняты другими приборами настроек, и текущий прибор cur.
// self - идентификатор прибора, которому выбирают haId.
func (s *Server) haIDChoices(cfg *config.Config, self, cur string) []choice {
	used := map[string]bool{}
	for id, d := range cfg.Devices {
		if d.HaID != "" && id != self {
			used[d.HaID] = true
		}
	}
	var list []string
	labels := map[string]string{}
	for _, h := range s.gw.Cloud.Account() {
		if !used[h.HaID] {
			list = append(list, h.HaID)
			labels[h.HaID] = h.Name + ", " + h.Type
		}
	}
	for _, h := range s.gw.LocalAppliances() {
		if !used[h.HaID] && labels[h.HaID] == "" {
			list = append(list, h.HaID)
			labels[h.HaID] = h.Brand + " " + h.HaType + ", ключи для домашней сети"
		}
	}
	return choices(list, cur, blankIf(cur == "", "-"), labels)
}

// summary - состояние прибора одной строкой по-русски. У приборов LG и De'Longhi состояние
// работы - то, по которому профиль видит, что прибор включён. Программа - режимом Алисы,
// которому она сопоставлена; оставшееся время - если его сообщает прибор (timeLeft).
func summary(st *appliance.State, p *profile.Profile) string {
	if !st.Online {
		return "нет связи"
	}
	key := appliance.StatusOperationState
	op, ok := st.Values[key].(string)
	if pw := p.Power; !ok && pw != nil && pw.State != "" {
		key, op = pw.State, formatValue(st.Values[pw.State])
	}
	var parts []string
	if op != "" {
		parts = append(parts, stateLabel(key, op))
	}
	if st.ActiveProgram != "" {
		parts = append(parts, programLabel(p, st.ActiveProgram))
	} else if st.SelectedProgram != "" {
		parts = append(parts, "выбрана программа "+programLabel(p, st.SelectedProgram))
	}
	if v, ok := timeLeft(p, st); ok && v > 0 {
		parts = append(parts, "осталось "+hcdevice.Duration(v))
	}
	return strings.Join(parts, ", ")
}

// timeLeft - оставшееся время программы в секундах для страницы: по своему свойству
// remaining_time профиля, а без него - по значению прибора. Home Connect сообщает его в
// BSH.Common.Option.RemainingProgramTime, LG - часами и минутами, их шлюз складывает в
// thinq.RemainKey. Алисе это время уходит только своим свойством.
func timeLeft(p *profile.Profile, st *appliance.State) (float64, bool) {
	if v, ok := hcdevice.Remaining(p, st); ok {
		return v, true
	}
	if !hcdevice.Working(p, st) {
		return 0, false
	}
	for _, k := range []string{appliance.OptionRemainingTime, thinq.RemainKey} {
		if v, ok := st.Values[k].(float64); ok {
			return max(v, 0), true
		}
	}
	return 0, false
}

// thinqChoices - приборы аккаунта LG для выбора: не занятые другими приборами настроек и текущий
// cur. Прибор с камерами остаётся в списке: каждую камеру добавляют отдельно.
func (s *Server) thinqChoices(cfg *config.Config, self, cur string) []choice {
	used := map[string]bool{}
	for id, d := range cfg.Devices {
		if d.DeviceID != "" && id != self {
			used[d.DeviceID] = true
		}
	}
	var list []string
	labels := map[string]string{}
	for _, d := range s.gw.ThinQ.Account() {
		if !used[d.ID] || len(s.gw.ThinQ.Locations(d.ID)) > 0 {
			list = append(list, d.ID)
			labels[d.ID] = d.Alias + ", " + d.Type
		}
	}
	return choices(list, cur, blankIf(cur == "", "-"), labels)
}

// locationLabels - камеры приборов LG по-русски; остальные - как в профиле.
var locationLabels = map[string]string{"FRIDGE": "холодильная камера", "FREEZER": "морозильная камера",
	"CONVERTIBLE": "камера-трансформер", "UPPER": "верхняя камера", "LOWER": "нижняя камера"}

func locationLabel(loc string) string {
	if l, ok := locationLabels[loc]; ok {
		return l
	}
	return loc
}

// aliceSummary - связь и состояние устройства Алисы одной строкой: для лент и других не Home Connect.
func aliceSummary(dev alice.Device, id string) (bool, string) {
	for _, st := range dev.States() {
		if st.ID != id {
			continue
		}
		if st.ErrorCode != "" {
			return false, "нет связи"
		}
		var parts []string
		for _, c := range st.Capabilities {
			sv, _ := c.State.(alice.StateValue)
			switch {
			case c.Type == alice.CapOnOff && sv.Value == true:
				parts = append(parts, "включён")
			case c.Type == alice.CapOnOff:
				parts = append(parts, "выключен")
			case c.Type == alice.CapRange && sv.Instance == "brightness":
				parts = append(parts, "яркость "+formatValue(sv.Value)+" %")
			case c.Type == alice.CapColor && sv.Instance == "scene":
				parts = append(parts, "сцена «"+nameOr(sceneNames, formatValue(sv.Value))+"»")
			}
		}
		return true, strings.Join(parts, ", ")
	}
	return false, ""
}

// short - последняя часть имени Home Connect: HotAir из Cooking.Oven.Program.HeatingMode.HotAir.
func short(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// translit - русские буквы латиницей для идентификатора прибора. Тот же список - в скрипте
// интерфейса: он показывает идентификатор, пока владелец вводит имя.
var translit = map[rune]string{'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh",
	'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s",
	'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch", 'ъ': "", 'ы': "y",
	'ь': "", 'э': "e", 'ю': "yu", 'я': "ya"}

// slug - идентификатор прибора из имени: латиница, цифры и дефисы, до 64 знаков.
// «Кофемашина» - kofemashina. Имя без букв и цифр - device.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		part, ok := translit[r]
		switch {
		case ok:
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9'):
			part = string(r)
		default:
			dash = b.Len() > 0
			continue
		}
		if part == "" {
			continue
		}
		if dash {
			b.WriteByte('-')
			dash = false
		}
		b.WriteString(part)
	}
	out := b.String()
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-")
	}
	if out == "" {
		return "device"
	}
	return out
}

// freeID - slug имени, а если такой прибор уже есть, - с номером: kofemashina-2.
func freeID(devices map[string]config.Device, name string) string {
	base := slug(name)
	id := base
	for n := 2; ; n++ {
		if _, ok := devices[id]; !ok {
			return id
		}
		suffix := "-" + strconv.Itoa(n)
		id = strings.TrimRight(base[:min(len(base), 64-len(suffix))], "-") + suffix
	}
}

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	f := r.PostForm
	id := strings.TrimSpace(f.Get("id"))
	d := config.Device{Name: strings.TrimSpace(f.Get("name")), Room: strings.TrimSpace(f.Get("room")),
		Source: f.Get("source"), Demo: f.Get("demo"), HaID: strings.TrimSpace(f.Get("ha_id")), Host: strings.TrimSpace(f.Get("host")),
		DeviceID: strings.TrimSpace(f.Get("device_id")), Location: strings.TrimSpace(f.Get("location")),
		Serial: strings.TrimSpace(f.Get("serial")), MAC: strings.TrimSpace(f.Get("mac")), Broadcast: strings.TrimSpace(f.Get("broadcast"))}
	if id == "" && d.Name != "" {
		cfg, _ := s.file.Current()
		id = freeID(cfg.Devices, d.Name)
	}
	err := s.change(f.Get("version"), "devices."+id, func(c *config.Config) error {
		if !config.DeviceIDRe.MatchString(id) {
			return invalid("Идентификатор: от 1 до 64 латинских букв, цифр, _ и -")
		}
		if _, ok := c.Devices[id]; ok {
			return invalid("Прибор %s уже есть", id)
		}
		// Из двух полей источника сохраняется только поле выбранного источника.
		if d.Source != config.SourceDemo {
			d.Demo = ""
		}
		local := d.Source == config.SourceHomeConnectLocal
		if d.Source != config.SourceHomeConnectCloud && !local {
			d.HaID = ""
		}
		if d.Source != config.SourceWLED && d.Source != config.SourceDeLonghi && !local && !tvSource(d.Source) {
			d.Host = ""
		}
		if d.Source != config.SourceDeLonghi {
			d.Serial = ""
		}
		if d.Source != config.SourceThinQ && d.Source != config.SourceSmartThings {
			d.DeviceID, d.Location = "", ""
		}
		if !tvSource(d.Source) {
			d.MAC, d.Broadcast = "", ""
		}
		if c.Devices == nil {
			c.Devices = map[string]config.Device{}
		}
		c.Devices[id] = d
		return nil
	})
	if err != nil {
		s.renderDevices(w, sess, http.StatusBadRequest, id, d, s.explain("devices."+id, err), false)
		return
	}
	s.done(w, r, sess, "/devices/"+id, "Прибор добавлен. Алиса покажет его после обновления списка устройств.")
}

// Страница прибора.

type kv struct{ Key, Value string }

// aliceState - состояние устройства Алисы: имя, ошибка по-русски с кодом Яндекса и умения.
type aliceState struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Error string      `json:"error"`
	Code  string      `json:"code"`
	Items []aliceItem `json:"items"`
}

type stateView struct {
	Online  bool
	Summary string
	// Tone - вид метки состояния (stateTone); Notice - что требует внимания (status).
	Tone, Notice string
	Alice        []aliceState
}

type commandView struct {
	Type, Instance, Title string
	OnOff, Toggle, Pause  bool
	Modes                 []choice
	// Color - выбор цвета #RRGGBB, Value - текущий цвет.
	Color bool
	// Range - число: пределы, шаг, единица и начальное значение поля.
	Range                       bool
	Min, Max, Step, Unit, Value string
}

type deviceView struct {
	ID string
	// Icon и Head - значок и метка состояния в шапке страницы; Voice - как прибор знает
	// Алиса; Dupe - предупреждение о таком же имени в комнате; Names - имена приборов для
	// скрипта (namesJSON); Icons - выбор значка.
	Icon        string
	Head        status
	Voice, Dupe string
	Names       string
	Icons       []choice
	Coffee      *coffeeView
	Conf        config.Device
	Source      string
	RoomNames   []string
	DemoKinds   []choice
	HaIDs       []choice
	ThinQIDs    []choice
	STIDs       []choice
	// TV - связь с телевизором в домашней сети; nil - прибор не телевизор.
	TV *tvView
	// Waiting - прибор облака ждёт своего типа от облака.
	Waiting    bool
	BasicError string
	Strip      *stripView
	State      *stateView
	// Unmapped - значения прибора, которые Алиса не видит; Mappings - сводка сопоставлений;
	// Preview - устройство Алисы по профилю из формы, ещё не сохранённому.
	Unmapped []unmappedRow
	Mappings []mapRow
	Preview  *previewView
	Commands []commandView
	Editor   *editorView
	// Tab - вкладка, которую страница открывает без адреса раздела: с ошибкой формы или
	// предпросмотром; Focus - раздел, к которому она прокручивается.
	Tab, Focus string
}

// deviceTabs - вкладка страницы прибора каждого раздела, кроме разделов редактора: они все
// на вкладке «Сопоставления».
var deviceTabs = map[string]string{"basic": "tab-device", "coffee": "tab-device", "pin": "tab-device", "tv": "tab-device",
	"danger": "tab-device", "state": "tab-state", "commands": "tab-state"}

func tabOf(section string) string {
	if t, ok := deviceTabs[section]; ok {
		return t
	}
	return "tab-map"
}

// draft - введённое, что не сохранилось из-за ошибки: раздел, текст ошибки и значения формы.
type draft struct {
	section string
	err     string
	device  *config.Device
	profile *profile.Profile
	presets map[int]string
	// preview и previewInfo - устройство Алисы по профилю из формы: JSON и описание.
	preview     string
	previewInfo []alice.DeviceInfo
}

func (s *Server) devicePage(w http.ResponseWriter, r *http.Request, sess *session) {
	s.renderDevice(w, sess, http.StatusOK, r.PathValue("id"), nil)
}

func (s *Server) renderDevice(w http.ResponseWriter, sess *session, status int, id string, d *draft) {
	cfg, version := s.file.Current()
	dc, ok := cfg.Devices[id]
	if !ok {
		s.notFound(w, sess)
		return
	}
	if d == nil {
		d = &draft{}
	}
	v := deviceView{ID: id, Conf: dc, Source: dc.Source, RoomNames: roomNames(cfg), Icon: s.iconOf(id, dc), Head: s.status(id, dc),
		Names: namesJSON(cfg)}
	if d.device != nil {
		v.Conf = *d.device
	}
	v.Voice = voiceText(v.Conf.Name, v.Conf.Room)
	if other := sameName(cfg, id, v.Conf.Name, v.Conf.Room); other != "" {
		v.Dupe = dupeText(cfg.Devices[other].Name, v.Conf.Room)
	}
	v.Icons = named(config.Icons, v.Conf.Icon, "по типу прибора", iconNames)
	if d.section == "basic" {
		v.BasicError = d.err
	}
	if d.section != "" {
		v.Tab, v.Focus = tabOf(d.section), d.section
		if d.preview != "" {
			v.Focus = "preview"
		}
	}
	v.DemoKinds = plain(appliance.DemoKinds, v.Conf.Demo, "", typeNames)
	v.HaIDs, v.Waiting = s.haIDChoices(cfg, id, v.Conf.HaID), s.gw.Waiting(id)
	v.ThinQIDs = s.thinqChoices(cfg, id, v.Conf.DeviceID)
	if dc.Source == config.SourceSmartThings {
		v.STIDs = s.stChoices(v.Conf.DeviceID)
	}
	v.TV = s.tvView(id, dc.Source)
	if strip, ok := s.gw.Strip(id); ok {
		v.Strip = s.stripView(strip, dc, d)
	}
	if v.Conf.Source == config.SourceDeLonghi {
		v.Coffee = s.coffeeView(id, v.Conf)
	}
	if dev, ok := s.gw.Device(id); ok {
		v.State, v.Commands = deviceState(dev), commands(dev, id)
		st := s.status(id, dc)
		v.State.Online, v.State.Summary, v.State.Tone, v.State.Notice = st.Online, st.Text, st.Tone, st.Notice
	}
	if hc, ok := s.gw.HomeConnect(id); ok {
		st := hc.Appliance.State()
		haType := hc.Appliance.Describe().Type
		builtin := profile.BuiltinProfile(haType)
		if dc.Source == config.SourceDeLonghi {
			// Рецепты и напиток из настроек прибора - под его профилем, как в шлюзе.
			builtin = profile.Merge(builtin, profile.DeLonghiLayer(dc.Recipes, dc.DefaultDrink))
		}
		desc := hc.Appliance.Describe()
		if dc.Source == config.SourceDeLonghi {
			// Номера рецептов 1-255 - не программы машины: в редактор попадают только сопоставленные.
			desc.Programs = nil
		}
		v.Unmapped = unmapped(&st, hc.Profile, &desc)
		v.Mappings = mapRows(hc.Profile, &st, hc)
		v.Preview = previewOf(d.previewInfo, hc.Info(), d.preview)
		t := &editTarget{action: "/devices/" + id + "/profile", builtin: builtin, probe: "/devices/" + id + "/probe",
			forType: cfg.Profiles[haType], forDevice: dc.Profile, device: true, lg: propsSource(dc.Source),
			desc: desc, values: slices.Collect(maps.Keys(st.Values)), now: st.Values, program: st.ActiveProgram}
		v.Editor = buildEditor(t, sess.csrf, version, d.profile, d.section, d.err)
	}
	s.render(w, status, "device", s.newPage(sess, "devices", dc.Name, v))
}

// deviceState - состояние устройств Алисы прибора по-русски.
func deviceState(dev alice.Device) *stateView {
	v := &stateView{Online: true}
	infos := map[string]alice.DeviceInfo{}
	for _, info := range dev.Info() {
		infos[info.ID] = info
	}
	for _, st := range dev.States() {
		a := aliceState{ID: st.ID, Name: infos[st.ID].Name, Code: st.ErrorCode, Items: stateItems(st, units(infos[st.ID]))}
		if st.ErrorCode != "" {
			a.Error = nameOr(errorTexts, st.ErrorCode)
		}
		if st.ErrorCode == alice.CodeUnreachable {
			v.Online = false
		}
		v.Alice = append(v.Alice, a)
	}
	return v
}

// stripView - лента WLED на странице прибора: что она сообщила о себе и её пресеты со сценами.
type stripView struct {
	Online     bool
	Name, Arch string
	// PresetsError - пресеты с ленты не загружены.
	PresetsError string
	Presets      []presetRow
	Error        string
}

// presetRow - пресет ленты и сцена Алисы. Missing - пресета нет на ленте, он есть только в настройках.
type presetRow struct {
	Index   int
	ID      int
	Name    string
	Missing bool
	Scenes  []choice
}

func (s *Server) stripView(strip *wled.Device, dc config.Device, d *draft) *stripView {
	v := &stripView{}
	v.Online, v.Name, v.Arch = strip.Strip()
	if d != nil && d.section == "presets" {
		v.Error = d.err
	}
	presets := dc.Presets
	if d != nil && d.presets != nil {
		presets = d.presets
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	names, err := wled.LoadPresets(ctx, dc.Host)
	if err != nil {
		s.log.Info("пресеты ленты WLED не загружены", "host", dc.Host, "err", err)
		v.PresetsError = "Лента недоступна: видны только пресеты из настроек."
	}
	ids := slices.Collect(maps.Keys(names))
	for p := range presets {
		if _, ok := names[p]; !ok {
			ids = append(ids, p)
		}
	}
	slices.Sort(ids)
	for i, p := range slices.Compact(ids) {
		_, onStrip := names[p]
		v.Presets = append(v.Presets, presetRow{Index: i, ID: p, Name: names[p], Missing: !onStrip && err == nil,
			Scenes: named(alice.ColorScenes, presets[p], "-", sceneNames)})
	}
	v.Presets = append(v.Presets, presetRow{Index: len(v.Presets), Scenes: named(alice.ColorScenes, "", "-", sceneNames)})
	return v
}

// saveStripPresets сохраняет сопоставление пресетов ленты сценам Алисы.
func (s *Server) saveStripPresets(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	f := r.PostForm
	presets := map[int]string{}
	var parseErr error
	for i := 0; f.Has(fk("p", i, "id")); i++ {
		raw, scene := strings.TrimSpace(f.Get(fk("p", i, "id"))), f.Get(fk("p", i, "scene"))
		if raw == "" || scene == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			parseErr = invalid("Номер пресета %q - не число", raw)
			break
		}
		presets[n] = scene
	}
	err := parseErr
	if err == nil {
		err = s.change(f.Get("version"), "devices."+id+".presets", func(c *config.Config) error {
			d, ok := c.Devices[id]
			if !ok || d.Source != config.SourceWLED {
				return invalid("Ленты %s нет в настройках", id)
			}
			d.Presets = nil
			if len(presets) > 0 {
				d.Presets = presets
			}
			c.Devices[id] = d
			return nil
		})
	}
	if err != nil {
		s.renderDevice(w, sess, http.StatusBadRequest, id, &draft{section: "presets", err: s.explain("devices."+id, err), presets: presets})
		return
	}
	s.done(w, r, sess, "/devices/"+id+"#presets", "Сохранено. Алиса узнает о сценах после обновления списка устройств.")
}

// commands - кнопки команд по умениям устройства Алисы: те же команды, что у Алисы.
// Поле числа заполнено текущим значением.
func commands(dev alice.Device, id string) []commandView {
	current := map[string]any{}
	for _, st := range dev.States() {
		if st.ID != id {
			continue
		}
		for _, c := range st.Capabilities {
			if sv, ok := c.State.(alice.StateValue); ok {
				current[c.Type+"/"+sv.Instance] = sv.Value
			}
		}
	}
	rgb := "#000000"
	if v, ok := current[alice.CapColor+"/rgb"].(int); ok {
		rgb = fmt.Sprintf("#%06x", v)
	}
	var out []commandView
	for _, info := range dev.Info() {
		if info.ID != id {
			continue
		}
		for _, c := range info.Capabilities {
			cv := commandView{Type: c.Type}
			switch p := c.Parameters.(type) {
			case alice.ModeParams:
				cv.Instance, cv.Title = p.Instance, itemLabel(c.Type, p.Instance)
				cur := formatValue(current[c.Type+"/"+p.Instance])
				for _, m := range p.Modes {
					cv.Modes = append(cv.Modes, choice{Value: m.Value, Label: nameOr(modeValueNames, m.Value), Selected: m.Value == cur})
				}
			case alice.RangeParams:
				cv.Instance, cv.Title, cv.Range = p.Instance, itemLabel(c.Type, p.Instance), true
				cv.Min, cv.Max, cv.Step = formatValue(p.Range.Min), formatValue(p.Range.Max), formatValue(p.Range.Precision)
				cv.Unit = nameOr(unitNames, p.Unit)
				cv.Value = cv.Min
				if v, ok := current[c.Type+"/"+p.Instance]; ok {
					cv.Value = formatValue(v)
				}
			case alice.InstanceParams:
				cv.Instance, cv.Title, cv.Toggle, cv.Pause = p.Instance, itemLabel(c.Type, p.Instance), true, p.Instance == "pause"
			case alice.ColorParams:
				out = append(out, commandView{Type: c.Type, Instance: "rgb", Title: "Цвет", Color: true, Value: rgb})
				if p.ColorScene == nil {
					continue
				}
				cv.Instance, cv.Title = "scene", "Сцена"
				cur := formatValue(current[c.Type+"/scene"])
				for _, sc := range p.ColorScene.Scenes {
					cv.Modes = append(cv.Modes, choice{Value: sc.ID, Label: nameOr(sceneNames, sc.ID), Selected: sc.ID == cur})
				}
			default:
				if c.Type != alice.CapOnOff {
					continue
				}
				cv.Instance, cv.Title, cv.OnOff = "on", "Включение", true
			}
			out = append(out, cv)
		}
	}
	return out
}

func (s *Server) saveDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	f := r.PostForm
	var next config.Device
	err := s.change(f.Get("version"), "devices."+id, func(c *config.Config) error {
		d, ok := c.Devices[id]
		if !ok {
			return invalid("Прибора %s нет в настройках", id)
		}
		d.Name, d.Room, d.Icon = strings.TrimSpace(f.Get("name")), strings.TrimSpace(f.Get("room")), f.Get("icon")
		switch d.Source {
		case config.SourceDemo:
			d.Demo = f.Get("demo")
		case config.SourceHomeConnectCloud:
			// Прибор заменили новым: меняется только haId, сценарии Алисы продолжают работать.
			if haID := strings.TrimSpace(f.Get("ha_id")); haID != "" {
				d.HaID = haID
			}
		case config.SourceHomeConnectLocal:
			if haID := strings.TrimSpace(f.Get("ha_id")); haID != "" {
				d.HaID = haID
			}
			d.Host = strings.TrimSpace(f.Get("host"))
		case config.SourceWLED:
			d.Host = strings.TrimSpace(f.Get("host"))
		case config.SourceThinQ, config.SourceSmartThings:
			if deviceID := strings.TrimSpace(f.Get("device_id")); deviceID != "" {
				d.DeviceID = deviceID
			}
			d.Location = strings.TrimSpace(f.Get("location"))
		case config.SourceWebOS, config.SourceSamsungTV:
			d.Host = strings.TrimSpace(f.Get("host"))
			d.MAC, d.Broadcast = strings.TrimSpace(f.Get("mac")), strings.TrimSpace(f.Get("broadcast"))
		case config.SourceDeLonghi:
			if err := coffeeForm(&d, f); err != nil {
				next = d
				return err
			}
		}
		next = d
		c.Devices[id] = d
		return nil
	})
	if err != nil {
		s.renderDevice(w, sess, http.StatusBadRequest, id, &draft{section: "basic", err: s.explain("devices."+id, err), device: &next})
		return
	}
	s.done(w, r, sess, "/devices/"+id+"#basic", "Сохранено.")
}

// confirmView - страница подтверждения действия, которое нельзя отменить: что произойдёт,
// кнопка действия и куда вернуться без него.
type confirmView struct {
	Title, Text, Action, Button, Back string
}

func (s *Server) deletePage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	cfg, _ := s.file.Current()
	d, ok := cfg.Devices[id]
	if !ok {
		s.notFound(w, sess)
		return
	}
	s.render(w, http.StatusOK, "confirm", s.newPage(sess, "devices", "Удаление прибора", confirmView{
		Title: "Удалить «" + d.Name + "»?",
		Text: "Прибор " + id + " пропадёт из Алисы после обновления списка устройств, сценарии с ним перестанут работать. " +
			"Переопределение его профиля удалится вместе с ним.",
		Action: "/devices/" + id + "/delete", Button: "Удалить прибор", Back: "/devices/" + id}))
}

// confirms - подтверждения на странице «Подключения»: отвязка Яндекса и выходы из облаков.
var confirms = map[string]confirmView{
	"yandex/unlink": {Title: "Отвязать аккаунт Яндекса?",
		Text: "Шлюз отзовёт токены Яндекса: Алиса перестанет управлять приборами, пока аккаунт не привяжут заново " +
			"в приложении «Дом с Алисой».",
		Button: "Отвязать аккаунт", Back: "/connections#yandex"},
	"homeconnect/logout": {Title: "Выйти из Home Connect?",
		Text: "Шлюз удалит токены Home Connect: приборы облака будут не на связи, пока не выполнен новый вход " +
			"с кодом на сайте Home Connect.",
		Button: "Выйти и удалить токены", Back: "/connections#homeconnect"},
	"delonghi/logout": {Title: "Выйти из De'Longhi?",
		Text: "Шлюз удалит сессию De'Longhi: кофемашины будут не на связи, пока не выполнен новый вход с почтой " +
			"и паролем аккаунта.",
		Button: "Выйти", Back: "/connections#delonghi"},
	"smartthings/logout": {Title: "Выйти из SmartThings?",
		Text: "Шлюз удалит токены SmartThings: устройства SmartThings будут не на связи, пока не выполнен новый вход " +
			"в аккаунт Samsung.",
		Button: "Выйти и удалить токены", Back: "/connections#smartthings"},
}

// confirmPage показывает подтверждение действия на странице «Подключения».
func (s *Server) confirmPage(name string) handler {
	v := confirms[name]
	v.Action = "/connections/" + name
	return func(w http.ResponseWriter, r *http.Request, sess *session) {
		s.render(w, http.StatusOK, "confirm", s.newPage(sess, "connections", v.Title, v))
	}
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	err := s.change(r.PostForm.Get("version"), "devices."+id, func(c *config.Config) error {
		if _, ok := c.Devices[id]; !ok {
			return invalid("Прибора %s нет в настройках", id)
		}
		delete(c.Devices, id)
		return nil
	})
	if err != nil {
		s.failed(w, r, sess, "/", s.explain("devices."+id, err))
		return
	}
	s.done(w, r, sess, "/", "Прибор "+id+" удалён.")
}

// command выполняет команду кнопки тем же путём, что команду Алисы. Кнопка питания из
// списка приборов (from=list) возвращает к строке прибора в списке.
func (s *Server) command(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	f := r.PostForm
	to := "/devices/" + id + "#commands"
	if f.Get("from") == "list" {
		to = "/#d-" + id
	}
	a := alice.Action{Type: f.Get("type"), State: alice.ActionState{Instance: f.Get("instance")}}
	raw := strings.TrimSpace(f.Get("value"))
	switch a.Type {
	case alice.CapOnOff, alice.CapToggle:
		a.State.Value = raw == "true"
	case alice.CapMode:
		a.State.Value = raw
	case alice.CapColor:
		if a.State.Instance == "rgb" {
			v, err := strconv.ParseUint(strings.TrimPrefix(raw, "#"), 16, 32)
			if err != nil || len(raw) != 7 {
				s.answer(w, r, sess, to, false, "Не выполнено: цвет нужен в виде #RRGGBB.")
				return
			}
			a.State.Value = float64(v)
		} else {
			a.State.Value = raw
		}
	case alice.CapRange:
		v, err := strconv.ParseFloat(strings.Replace(raw, ",", ".", 1), 64)
		if err != nil {
			s.answer(w, r, sess, to, false, "Не выполнено: "+strconv.Quote(raw)+" - не число.")
			return
		}
		a.State.Value = v
	default:
		s.answer(w, r, sess, to, false, "Не выполнено: неизвестное умение.")
		return
	}
	if code := s.gw.Service.Command(r.Context(), id, &a); code != "" {
		s.answer(w, r, sess, to, false, "Не выполнено: "+errorTexts[code]+" ("+code+").")
		return
	}
	s.answer(w, r, sess, to, true, "Команда выполнена.")
}

// answer - итог команды: скрипту плитки, который ждёт JSON, - {"ok", "text"}, без скрипта -
// сообщение на следующей странице и переход на неё.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, sess *session, to string, ok bool, text string) {
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			OK   bool   `json:"ok"`
			Text string `json:"text"`
		}{ok, text})
		return
	}
	if ok {
		s.done(w, r, sess, to, text)
	} else {
		s.failed(w, r, sess, to, text)
	}
}

// saveDeviceProfile сохраняет поле переопределения профиля прибора.
func (s *Server) saveDeviceProfile(w http.ResponseWriter, r *http.Request, sess *session) {
	id, field := r.PathValue("id"), r.PathValue("field")
	hc, ok := s.gw.HomeConnect(id)
	if !ok || !knownField(field) {
		s.notFound(w, sess)
		return
	}
	patch, err := parseField(field, r.PostForm)
	var next *profile.Profile
	if err == nil && r.PostForm.Get("op") == "preview" {
		s.previewProfile(w, sess, id, field, patch, hc)
		return
	}
	if err == nil {
		err = s.change(r.PostForm.Get("version"), "devices."+id+".profile."+field, func(c *config.Config) error {
			d, ok := c.Devices[id]
			if !ok {
				return invalid("Прибора %s нет в настройках", id)
			}
			haType := hc.Appliance.Describe().Type
			t := &editTarget{builtin: profile.BuiltinProfile(haType), forType: c.Profiles[haType], forDevice: d.Profile, device: true}
			next = applyField(t, field, patch, r.PostForm.Get("op") == "reset")
			d.Profile = next
			c.Devices[id] = d
			return nil
		})
	}
	if err != nil {
		s.renderDevice(w, sess, http.StatusBadRequest, id, &draft{section: field, err: s.explain("devices."+id, err), profile: next})
		return
	}
	s.done(w, r, sess, "/devices/"+id+"#"+field, "Сохранено. Алиса узнает об изменении после обновления списка устройств.")
}

// Профили типов.

type typeRow struct {
	Type, Name, YandexType string
	Builtin                bool
	Overridden             []kv
	Devices                []string
}

// profilesView - типы приборов: по умолчанию только те, у которых есть приборы или
// переопределение; All - показаны все, Hidden - сколько скрыто.
type profilesView struct {
	Rows   []typeRow
	All    bool
	Hidden int
}

func (s *Server) profilesPage(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, _ := s.file.Current()
	v := profilesView{All: r.URL.Query().Get("all") == "1"}
	byType := map[string][]string{}
	for id, t := range s.gw.Types() {
		byType[t] = append(byType[t], id)
	}
	types := slices.Concat(profile.BuiltinTypes(), slices.Collect(maps.Keys(cfg.Profiles)), slices.Collect(maps.Keys(byType)))
	for _, t := range uniq(types) {
		b := profile.BuiltinProfile(t)
		row := typeRow{Type: t, Name: typeName(t), Builtin: b != nil, YandexType: profile.Merge(b, cfg.Profiles[t]).YandexType, Devices: byType[t]}
		slices.Sort(row.Devices)
		for _, f := range profileFields {
			if has(cfg.Profiles[t], f.name) {
				row.Overridden = append(row.Overridden, kv{f.name, f.title})
			}
		}
		if !v.All && len(row.Devices) == 0 && len(row.Overridden) == 0 {
			v.Hidden++
			continue
		}
		v.Rows = append(v.Rows, row)
	}
	slices.SortStableFunc(v.Rows, func(a, b typeRow) int { return strings.Compare(a.Name, b.Name) })
	s.render(w, http.StatusOK, "profiles", s.newPage(sess, "profiles", "Профили типов", v))
}

type typeView struct {
	Type, Name string
	Devices    []string
	Editor     *editorView
}

// typeTarget - редактор профиля типа: программы и параметры берутся у всех приборов этого типа.
func (s *Server) typeTarget(cfg *config.Config, haType string) (*editTarget, []string, bool) {
	t := &editTarget{action: "/profiles/" + url.PathEscape(haType), builtin: profile.BuiltinProfile(haType),
		forType: cfg.Profiles[haType], desc: appliance.Description{Type: haType, Options: map[string]appliance.Option{}},
		lg: propsType(haType)}
	var devices []string
	for id, typ := range s.gw.Types() {
		if typ != haType {
			continue
		}
		hc, ok := s.gw.HomeConnect(id)
		if !ok {
			continue
		}
		devices = append(devices, id)
		d := hc.Appliance.Describe()
		t.desc.Programs = append(t.desc.Programs, d.Programs...)
		t.desc.Commands = append(t.desc.Commands, d.Commands...)
		t.desc.Events = append(t.desc.Events, d.Events...)
		for k, o := range d.Options {
			t.desc.Options[k] = o
		}
		t.values = append(t.values, slices.Collect(maps.Keys(hc.Appliance.State().Values))...)
	}
	t.desc.Programs = uniq(t.desc.Programs)
	slices.Sort(devices)
	known := t.builtin != nil || t.forType != nil || len(devices) > 0
	return t, devices, typeRe.MatchString(haType) && known
}

func (s *Server) typePage(w http.ResponseWriter, r *http.Request, sess *session) {
	s.renderType(w, sess, http.StatusOK, r.PathValue("type"), nil)
}

func (s *Server) renderType(w http.ResponseWriter, sess *session, status int, haType string, d *draft) {
	cfg, version := s.file.Current()
	t, devices, ok := s.typeTarget(cfg, haType)
	if !ok {
		s.notFound(w, sess)
		return
	}
	if d == nil {
		d = &draft{}
	}
	v := typeView{Type: haType, Name: typeName(haType), Devices: devices, Editor: buildEditor(t, sess.csrf, version, d.profile, d.section, d.err)}
	s.render(w, status, "profile", s.newPage(sess, "profiles", "Профиль "+v.Name, v))
}

func (s *Server) saveTypeProfile(w http.ResponseWriter, r *http.Request, sess *session) {
	haType, field := r.PathValue("type"), r.PathValue("field")
	cfg, _ := s.file.Current()
	if _, _, ok := s.typeTarget(cfg, haType); !ok || !knownField(field) {
		s.notFound(w, sess)
		return
	}
	patch, err := parseField(field, r.PostForm)
	var next *profile.Profile
	if err == nil {
		err = s.change(r.PostForm.Get("version"), "profiles."+haType+"."+field, func(c *config.Config) error {
			t := &editTarget{builtin: profile.BuiltinProfile(haType), forType: c.Profiles[haType]}
			next = applyField(t, field, patch, r.PostForm.Get("op") == "reset")
			if next == nil {
				delete(c.Profiles, haType)
				return nil
			}
			// Профиль типа без приборов проверяется сам: при запуске его проверил бы только прибор.
			if _, err := profile.Resolve(haType, next, nil); err != nil && !hasDevices(c, s.gw.Types(), haType) {
				return &inputError{err.Error()}
			}
			if c.Profiles == nil {
				c.Profiles = map[string]*profile.Profile{}
			}
			c.Profiles[haType] = next
			return nil
		})
	}
	if err != nil {
		s.renderType(w, sess, http.StatusBadRequest, haType, &draft{section: field, err: s.explain("profiles."+haType, err), profile: next})
		return
	}
	s.done(w, r, sess, "/profiles/"+url.PathEscape(haType)+"#"+field, "Сохранено. Алиса узнает об изменении после обновления списка устройств.")
}

func hasDevices(c *config.Config, types map[string]string, haType string) bool {
	for id, t := range types {
		if _, ok := c.Devices[id]; ok && t == haType {
			return true
		}
	}
	return false
}

// Подключения.

type connectionsView struct {
	// Summary - сводка вверху страницы: карточка на каждое подключение.
	Summary []connRow
	// Flow - шаги подключения в разделах по разделу: yandex, homeconnect, thinq, delonghi,
	// smartthings.
	Flow map[string][]stepState
	// Steps - шаги настройки, которые ещё не сделаны.
	Steps []step
	// Open - разделы, открытые сразу: подключение настроено или у него есть приборы.
	Open map[string]bool
	// Адреса для консоли навыка: по адресу, с которого открыта страница.
	Endpoint, AuthURL, TokenURL string

	Yandex    config.Yandex
	TokenSet  bool
	SecretSet bool
	Redirects string
	Linked    bool
	Error     string
	TokenLink string
	// Метрики: задан ли токен, открыт ли порт с запуска шлюза, адрес порта и ошибка формы.
	MetricsTokenSet bool
	MetricsOpen     bool
	MetricsListen   string
	MetricsError    string
	HC              hcView
	LG              lgView
	Local           []localRow
	Coffee          coffeeAccount
	ST              stView
	// TVs - телевизоров в домашней сети в настройках.
	TVs int
	// Discovery - поиск приборов в домашней сети.
	Discovery discoveryView
}

// localRow - прибор с ключами для домашней сети и прибор настроек с тем же haId, если он есть.
type localRow struct {
	hclocal.Info
	Device string
}

// lgView - аккаунт LG ThinQ на странице «Подключения».
type lgView struct {
	Country   string
	APIURL    string
	PATSet    bool
	Ready     bool
	Connected bool
	Account   []lgRow
}

// lgRow - прибор аккаунта LG или его камера и прибор настроек, если он уже добавлен.
type lgRow struct {
	ID, Alias, Type, Model, Location, LocationLabel, Device string
}

// hcView - облако Home Connect на странице «Подключения».
type hcView struct {
	Config    config.HomeConnect
	SecretSet bool
	Ready     bool
	LoggedIn  bool
	Connected bool
	Login     hcapi.Login
	Pending   bool
	Expires   string
	Account   []accountRow
}

// accountRow - прибор аккаунта Home Connect и прибор настроек, если он уже добавлен.
type accountRow struct {
	HaID, Name, Type, Brand, Device string
	Connected                       bool
}

func (s *Server) connectionsPage(w http.ResponseWriter, r *http.Request, sess *session) {
	s.renderConnections(w, r, sess, http.StatusOK, nil, "", "")
}

// connRow - подключение карточкой: раздел страницы, где оно работает, состояние и вид его
// метки (stateTone), сколько приборов в шлюзе, пометка и одна главная кнопка Action. Post -
// кнопка отправляет форму на этот адрес, иначе Link - ссылка; NoteWarn - пометка янтарная.
type connRow struct {
	ID, Name, Where, State, Tone string
	Devices                      int
	Note                         string
	NoteWarn                     bool
	Action, Link, Post           string
}

// stepState - шаг подключения: сделан ли он, раскрыт ли и пометка рядом с названием.
type stepState struct {
	Done, Open bool
	Note       string
}

// flow - шаги подключения по порядку: раскрыт первый несделанный, остальные свёрнуты.
func flow(done ...bool) []stepState {
	out := make([]stepState, len(done))
	for i, d := range done {
		out[i].Done = d
	}
	for i := range out {
		if !out[i].Done {
			out[i].Open = true
			break
		}
	}
	return out
}

// added - сколько устройств аккаунта уже в шлюзе: last - шаг со списком устройств сделан,
// когда в шлюзе все; note - «в шлюзе 2 из 5».
func added(have, total int) (last bool, note string) {
	if total == 0 {
		return false, ""
	}
	return have == total, "в шлюзе " + strconv.Itoa(have) + " из " + strconv.Itoa(total)
}

// renderConnections показывает подключения. form - введённое в форму Яндекса, если оно
// не сохранилось; yandexErr и metricsErr - ошибки форм Яндекса и метрик.
func (s *Server) renderConnections(w http.ResponseWriter, r *http.Request, sess *session, status int, form *config.Yandex, yandexErr, metricsErr string) {
	cfg, _ := s.file.Current()
	y := cfg.Yandex
	v := connectionsView{TokenSet: y.CallbackToken != "", SecretSet: y.ClientSecret != "", Linked: s.gw.Tokens.Linked(),
		Error:           yandexErr,
		TokenLink:       "https://oauth.yandex.ru/authorize?response_type=token&client_id=c473ca268cd749d3a8371351a8f2bcbd",
		MetricsTokenSet: cfg.Metrics.Token != "", MetricsOpen: s.start.Metrics.Token != "", MetricsListen: s.start.Metrics.Listen,
		MetricsError: metricsErr, HC: s.hcView(cfg), LG: s.lgView(cfg), Local: s.localRows(cfg), Coffee: s.coffeeAccount(cfg),
		ST: s.stView(cfg, r, sess), Steps: s.steps(cfg), Discovery: s.discoveryView(cfg)}
	base := "https://" + gatewayHost(r)
	v.Endpoint, v.AuthURL, v.TokenURL = base, base+"/auth/authorize", base+"/auth/token"
	if form != nil {
		y = *form
	}
	// Секреты на страницу не попадают: видно только, заданы ли они.
	y.CallbackToken, y.ClientSecret = "", ""
	v.Yandex, v.Redirects = y, strings.Join(y.RedirectURIs, "\n")
	s.connSummary(cfg, &v)
	p := s.newPage(sess, "connections", "Подключения", v)
	if v.HC.Pending {
		// Пока владелец входит в Home Connect, страница обновляется сама и покажет итог.
		p.Refresh, p.RefreshTo = loginRefresh, refreshTo("/connections", "homeconnect")
	}
	if v.Discovery.Running {
		// Пока идёт поиск, страница обновляется сама и показывает находки.
		p.Refresh, p.RefreshTo = discoveryRefresh, refreshTo("/connections", "discovery")
	}
	s.render(w, status, "connections", p)
}

// hostRe - адрес шлюза из запроса, который можно показать в адресах для консоли: имя или IP
// с портом по желанию.
var hostRe = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]{2,45}\]|[A-Za-z0-9.-]{1,253})(:[0-9]{1,5})?$`)

// gatewayHost - адрес, по которому владелец открыл страницу. Его видит только сам владелец,
// поэтому заголовок Host не проверяется по настройкам; странный адрес заменяет заглушка.
func gatewayHost(r *http.Request) string {
	if hostRe.MatchString(r.Host) {
		return r.Host
	}
	return "<адрес шлюза>"
}

// connSummary заполняет сводку и решает, какие разделы открыть сразу: подключение
// настроено или у него есть приборы.
func (s *Server) connSummary(cfg *config.Config, v *connectionsView) {
	used := map[string]int{}
	for _, d := range cfg.Devices {
		used[d.Source]++
	}
	y := connRow{ID: "yandex", Name: "Яндекс", Where: "облако Яндекса", State: "аккаунт привязан", Tone: "run",
		Action: "Открыть", Link: "#yandex"}
	switch {
	case !cfg.Yandex.Ready():
		y.State, y.Tone, y.Action = "не заданы идентификатор и секрет приложения", "warn", "Настроить"
	case !v.Linked:
		y.State, y.Tone, y.Action, y.Link = "аккаунт не привязан", "warn", "Как привязать", "#yandex-link"
	}
	hc := connRow{ID: "homeconnect", Name: "Home Connect", Where: "облако", State: "не настроено", Tone: "idle",
		Devices: used[config.SourceHomeConnectCloud], Action: "Настроить", Link: "#homeconnect"}
	switch {
	case v.HC.Pending:
		hc.State, hc.Tone, hc.Action = "ждёт входа по коду", "warn", "Ввести код"
	case v.HC.LoggedIn && v.HC.Connected:
		hc.State, hc.Tone, hc.Action = "вход выполнен, события приходят", "run", "Открыть"
	case v.HC.LoggedIn:
		hc.State, hc.Tone, hc.Action = "вход выполнен, поток событий не подключён", "warn", "Открыть"
	case v.HC.Ready:
		hc.State, hc.Tone, hc.Action, hc.Post = "вход не выполнен", "warn", "Войти", "/connections/homeconnect/login"
	}
	local := connRow{ID: "local", Name: "Home Connect в домашней сети", Where: "домашняя сеть", State: "ключей нет",
		Tone: "idle", Devices: used[config.SourceHomeConnectLocal], Action: "Открыть", Link: "#local"}
	if len(v.Local) > 0 {
		local.State, local.Tone = "ключи приборов: "+strconv.Itoa(len(v.Local)), "run"
	}
	lg := connRow{ID: "thinq", Name: "LG ThinQ", Where: "облако", State: "не настроено", Tone: "idle",
		Devices: used[config.SourceThinQ], Action: "Настроить", Link: "#thinq"}
	switch {
	case v.LG.Ready && v.LG.Connected:
		lg.State, lg.Tone, lg.Action = "события приходят", "run", "Открыть"
	case v.LG.Ready:
		lg.State, lg.Tone, lg.Action = "события не подключены", "warn", "Открыть"
	}
	dl := connRow{ID: "delonghi", Name: "De'Longhi", Where: "облако и домашняя сеть", State: "вход не выполнен", Tone: "idle",
		Devices: used[config.SourceDeLonghi], Action: "Войти", Link: "#delonghi"}
	switch {
	case v.Coffee.LoginRequired:
		dl.State, dl.Tone, dl.Action = "токен не продлевается, нужен новый вход", "warn", "Войти заново"
	case v.Coffee.LoggedIn:
		dl.State, dl.Tone, dl.Action = "вход выполнен", "run", "Открыть"
	}
	st := connRow{ID: "smartthings", Name: "SmartThings", Where: "облако", State: "не настроено", Tone: "idle",
		Devices: used[config.SourceSmartThings], Action: "Настроить", Link: "#smartthings"}
	switch {
	case v.ST.LoggedIn && v.ST.Connected:
		st.State, st.Tone, st.Action = "вход выполнен, события приходят", "run", "Открыть"
	case v.ST.LoggedIn:
		st.State, st.Tone, st.Action = "вход выполнен, поток событий не подключён", "warn", "Открыть"
	case v.ST.Configured:
		st.State, st.Tone, st.Action, st.Post = "вход не выполнен", "warn", "Войти", "/connections/smartthings/login"
	}
	if v.ST.LoggedIn {
		st.Note, st.NoteWarn = "запросов: "+stUsageText(v.ST.Usage), v.ST.Warn
	}
	v.TVs = used[config.SourceWebOS] + used[config.SourceSamsungTV]
	tv := connRow{ID: "tv", Name: "Телевизоры", Where: "домашняя сеть", State: "не добавлены", Tone: "idle", Devices: v.TVs,
		Action: "Открыть", Link: "#tv"}
	if v.TVs > 0 {
		tv.State, tv.Tone = "добавлены", "run"
	}
	m := connRow{ID: "metrics", Name: "Метрики", Where: "сеть Docker", State: "порт закрыт", Tone: "idle", Action: "Настроить", Link: "#metrics"}
	if v.MetricsOpen {
		m.State, m.Tone, m.Action = "порт открыт", "run", "Открыть"
	}
	dv := connRow{ID: "discovery", Name: "Поиск в домашней сети", Where: "домашняя сеть", State: "не запускался", Tone: "idle",
		Action: "Найти", Link: "#discovery"}
	switch d := v.Discovery; {
	case d.Running:
		dv.State, dv.Tone, dv.Action = "идёт: проверено "+strconv.Itoa(d.Checked)+" из "+strconv.Itoa(d.Total), "run", "Открыть"
	case d.Searched:
		dv.State = "найдено: " + strconv.Itoa(len(d.Rows))
	}
	v.Summary = []connRow{y, hc, local, lg, dl, st, tv, dv, m}
	v.Open = map[string]bool{
		"homeconnect": v.HC.Ready || v.HC.LoggedIn || used[config.SourceHomeConnectCloud] > 0,
		"local":       len(v.Local) > 0 || used[config.SourceHomeConnectLocal] > 0,
		"thinq":       v.LG.PATSet || used[config.SourceThinQ] > 0,
		"delonghi":    v.Coffee.LoggedIn || used[config.SourceDeLonghi] > 0,
		"smartthings": v.ST.Configured || v.ST.LoggedIn || used[config.SourceSmartThings] > 0,
		"metrics":     v.MetricsTokenSet || v.MetricsOpen || v.MetricsError != "",
	}
	tally := func(n int, have func(i int) bool) (int, int) {
		h := 0
		for i := 0; i < n; i++ {
			if have(i) {
				h++
			}
		}
		return h, n
	}
	yf := flow(cfg.Yandex.Ready(), cfg.Yandex.SkillID != "" && v.TokenSet, v.Linked)
	if v.Error != "" {
		// Ошибка формы: её поля - в первых двух шагах.
		yf[0].Open, yf[1].Open = true, true
	}
	hcDone, hcNote := added(tally(len(v.HC.Account), func(i int) bool { return v.HC.Account[i].Device != "" }))
	hcf := flow(v.HC.Ready, v.HC.LoggedIn, hcDone)
	hcf[2].Note = hcNote
	lgDone, lgNote := added(tally(len(v.LG.Account), func(i int) bool { return v.LG.Account[i].Device != "" }))
	lgf := flow(v.LG.PATSet && v.LG.Country != "", lgDone)
	lgf[1].Note = lgNote
	dlDone, dlNote := added(tally(len(v.Coffee.Machines), func(i int) bool { return v.Coffee.Machines[i].Device != "" }))
	dlf := flow(v.Coffee.LoggedIn && !v.Coffee.LoginRequired, dlDone)
	dlf[1].Note = dlNote
	stDone, stNote := added(tally(len(v.ST.Devices), func(i int) bool { return v.ST.Devices[i].Device != "" }))
	stf := flow(v.ST.Configured, v.ST.LoggedIn, stDone)
	stf[2].Note = stNote
	v.Flow = map[string][]stepState{"yandex": yf, "homeconnect": hcf, "thinq": lgf, "delonghi": dlf, "smartthings": stf}
}

// refreshTo - адрес самообновления страницы path у раздела section: с новой меткой r, иначе
// браузер не загрузит страницу заново (page.RefreshTo).
func refreshTo(path, section string) string {
	return path + "?r=" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "#" + section
}

// loginRefresh - через сколько секунд страница со входом в Home Connect обновляется сама.
const loginRefresh = 5

func (s *Server) hcView(cfg *config.Config) hcView {
	cloud := s.gw.Cloud
	v := hcView{Config: cfg.HomeConnect, SecretSet: cfg.HomeConnect.ClientSecret != "", Ready: cfg.HomeConnect.Ready(),
		LoggedIn: cloud.API.LoggedIn(), Connected: cloud.Connected(), Login: cloud.API.LoginState()}
	v.Config.ClientSecret = ""
	v.Pending = v.Login.Status == hcapi.LoginPending
	if v.Pending {
		v.Expires = v.Login.Expires.Local().Format(time.TimeOnly)
	}
	devices := map[string]string{}
	for id, d := range cfg.Devices {
		if d.HaID != "" {
			devices[d.HaID] = id
		}
	}
	for _, h := range cloud.Account() {
		v.Account = append(v.Account, accountRow{HaID: h.HaID, Name: h.Name, Type: h.Type, Brand: h.Brand,
			Connected: h.Connected, Device: devices[h.HaID]})
	}
	return v
}

func (s *Server) localRows(cfg *config.Config) []localRow {
	devices := map[string]string{}
	for id, d := range cfg.Devices {
		if d.HaID != "" {
			devices[d.HaID] = id
		}
	}
	var rows []localRow
	for _, info := range s.gw.LocalAppliances() {
		rows = append(rows, localRow{Info: info, Device: devices[info.HaID]})
	}
	return rows
}

func (s *Server) lgView(cfg *config.Config) lgView {
	v := lgView{Country: cfg.ThinQ.Country, APIURL: cfg.ThinQ.APIURL, PATSet: cfg.ThinQ.PAT != "", Ready: cfg.ThinQ.Ready(),
		Connected: s.gw.ThinQ.Connected()}
	devices := map[string]string{}
	for id, d := range cfg.Devices {
		if d.DeviceID != "" {
			devices[d.DeviceID+"/"+d.Location] = id
		}
	}
	for _, d := range s.gw.ThinQ.Account() {
		locations := s.gw.ThinQ.Locations(d.ID)
		if len(locations) == 0 {
			locations = []string{""}
		}
		for _, loc := range locations {
			v.Account = append(v.Account, lgRow{ID: d.ID, Alias: d.Alias, Type: d.Type, Model: d.Model, Location: loc,
				LocationLabel: locationLabel(loc), Device: devices[d.ID+"/"+loc]})
		}
	}
	return v
}

func (s *Server) saveThinQ(w http.ResponseWriter, r *http.Request, sess *session) {
	f := r.PostForm
	err := s.change(f.Get("version"), "thinq", func(c *config.Config) error {
		c.ThinQ.Country = strings.ToUpper(strings.TrimSpace(f.Get("country")))
		c.ThinQ.PAT = secret(c.ThinQ.PAT, strings.TrimSpace(f.Get("pat")), f.Get("pat_clear") != "")
		c.ThinQ.APIURL = strings.TrimSpace(f.Get("api_url"))
		return nil
	})
	if err != nil {
		s.failed(w, r, sess, "/connections#thinq", s.explain("thinq", err))
		return
	}
	s.done(w, r, sess, "/connections#thinq", "Настройки LG ThinQ сохранены.")
}

// thinqDevices загружает список приборов аккаунта LG.
func (s *Server) thinqDevices(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.gw.ThinQ.RefreshAccount(ctx); err != nil {
		s.log.Warn("список приборов LG не загружен", "err", err)
		s.failed(w, r, sess, "/connections#thinq", "Список приборов LG не загружен, подробности в журнале шлюза.")
		return
	}
	s.done(w, r, sess, "/connections#thinq", "Список приборов LG обновлён.")
}

func (s *Server) saveHomeConnect(w http.ResponseWriter, r *http.Request, sess *session) {
	f := r.PostForm
	get := func(name string) string { return strings.TrimSpace(f.Get(name)) }
	err := s.change(f.Get("version"), "homeconnect", func(c *config.Config) error {
		h := &c.HomeConnect
		h.ClientID = get("client_id")
		h.ClientSecret = secret(h.ClientSecret, get("client_secret"), f.Get("client_secret_clear") != "")
		h.APIURL = get("api_url")
		if h.APIURL == "" {
			h.APIURL = hcapi.DefaultBaseURL
		}
		return nil
	})
	if err != nil {
		s.failed(w, r, sess, "/connections#homeconnect", s.explain("homeconnect", err))
		return
	}
	s.done(w, r, sess, "/connections#homeconnect", "Настройки Home Connect сохранены.")
}

// homeConnectLogin начинает вход по Device Flow: облако выдаёт код, страница показывает его со ссылкой.
func (s *Server) homeConnectLogin(w http.ResponseWriter, r *http.Request, sess *session) {
	cloud := s.gw.Cloud
	if _, err := cloud.API.StartLogin(r.Context(), cloud.LoggedIn); err != nil {
		s.log.Warn("вход в Home Connect не начат", "err", err)
		s.failed(w, r, sess, "/connections#homeconnect",
			"Вход не начат: проверьте client_id и адрес облака, подробности в журнале шлюза.")
		return
	}
	s.log.Info("вход в Home Connect начат из интерфейса")
	http.Redirect(w, r, "/connections#homeconnect", http.StatusSeeOther)
}

func (s *Server) homeConnectLogout(w http.ResponseWriter, r *http.Request, sess *session) {
	if err := s.gw.Cloud.API.Logout(); err != nil {
		s.log.Error("выход из Home Connect", "err", err)
		s.failed(w, r, sess, "/connections#homeconnect", "Не удалось удалить токены, подробности в журнале шлюза.")
		return
	}
	s.log.Info("выход из Home Connect из интерфейса")
	s.done(w, r, sess, "/connections#homeconnect", "Выход из Home Connect выполнен.")
}

// homeConnectAppliances загружает список приборов аккаунта: это запрос из суточного лимита облака.
func (s *Server) homeConnectAppliances(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.gw.Cloud.RefreshAccount(ctx); err != nil {
		s.log.Warn("список приборов Home Connect не загружен", "err", err)
		s.failed(w, r, sess, "/connections#homeconnect", "Список приборов не загружен, подробности в журнале шлюза.")
		return
	}
	s.done(w, r, sess, "/connections#homeconnect", "Список приборов аккаунта обновлён.")
}

func (s *Server) saveYandex(w http.ResponseWriter, r *http.Request, sess *session) {
	f := r.PostForm
	get := func(name string) string { return strings.TrimSpace(f.Get(name)) }
	var form config.Yandex
	err := s.change(f.Get("version"), "yandex", func(c *config.Config) error {
		y := &c.Yandex
		y.SkillID, y.ClientID = get("skill_id"), get("client_id")
		y.CallbackToken = secret(y.CallbackToken, get("callback_token"), f.Get("callback_token_clear") != "")
		y.ClientSecret = secret(y.ClientSecret, get("client_secret"), f.Get("client_secret_clear") != "")
		y.RedirectURIs = nil
		for _, line := range strings.Split(f.Get("redirect_uris"), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				y.RedirectURIs = append(y.RedirectURIs, line)
			}
		}
		form = *y
		return nil
	})
	if err != nil {
		s.renderConnections(w, r, sess, http.StatusBadRequest, &form, s.explain("yandex", err), "")
		return
	}
	s.done(w, r, sess, "/connections#yandex", "Настройки Яндекса сохранены.")
}

func (s *Server) saveMetrics(w http.ResponseWriter, r *http.Request, sess *session) {
	f := r.PostForm
	err := s.change(f.Get("version"), "metrics", func(c *config.Config) error {
		c.Metrics.Token = secret(c.Metrics.Token, strings.TrimSpace(f.Get("metrics_token")), f.Get("metrics_token_clear") != "")
		return nil
	})
	if err != nil {
		s.renderConnections(w, r, sess, http.StatusBadRequest, nil, "", s.explain("metrics", err))
		return
	}
	s.done(w, r, sess, "/connections#metrics", "Токен метрик сохранён.")
}

// secret - новое значение секрета: введённое, пустое, если отмечено «удалить», иначе прежнее.
func secret(cur, entered string, clear bool) string {
	switch {
	case clear:
		return ""
	case entered != "":
		return entered
	}
	return cur
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request, sess *session) {
	if err := s.gw.Tokens.RevokeAll(); err != nil {
		s.log.Error("отвязка аккаунта из интерфейса", "err", err)
		s.failed(w, r, sess, "/connections#yandex", "Не удалось отвязать аккаунт, подробности в журнале шлюза.")
		return
	}
	s.log.Info("аккаунт Яндекса отвязан из интерфейса")
	s.done(w, r, sess, "/connections#yandex", "Аккаунт Яндекса отвязан.")
}

// Входы.

type banRow struct {
	Prefix, Until string
}

type loginsView struct {
	Addr string
	Bans []banRow
}

func (s *Server) loginsPage(w http.ResponseWriter, r *http.Request, sess *session) {
	v := loginsView{Addr: s.auth.Addr(r).String()}
	for _, b := range s.auth.Bans.List() {
		v.Bans = append(v.Bans, banRow{Prefix: b.Prefix.String(), Until: b.Until.Local().Format(time.DateTime)})
	}
	s.render(w, http.StatusOK, "logins", s.newPage(sess, "logins", "Входы", v))
}

func (s *Server) lift(w http.ResponseWriter, r *http.Request, sess *session) {
	p, err := netip.ParsePrefix(r.PostForm.Get("prefix"))
	if err != nil || !s.auth.Bans.Lift(p) {
		s.failed(w, r, sess, "/logins", "Такого бана нет.")
		return
	}
	s.log.Info("бан снят из интерфейса", "addr", p)
	s.done(w, r, sess, "/logins", "Бан "+p.String()+" снят.")
}

// propsSource - источник приборов без программ: всё, что они умеют, - записать свойство.
func propsSource(source string) bool {
	switch source {
	case config.SourceThinQ, config.SourceSmartThings, config.SourceWebOS, config.SourceSamsungTV:
		return true
	}
	return false
}

// propsType - тип прибора без программ: LG, SmartThings или телевизор в домашней сети.
func propsType(haType string) bool {
	return profile.LGType(haType) || profile.SmartThingsType(haType) || haType == profile.LGTVType || haType == profile.SamsungTVType
}
