package web

import (
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/config"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/hcdevice"
)

// Состояние прибора одной меткой и что в нём требует внимания: для плитки, страницы прибора
// и «Всё о приборе».

// batteryLow - заряд в процентах, с которого прибор требует внимания.
const batteryLow = 20

// status - связь и состояние прибора.
type status struct {
	Online, Waiting bool
	// Text - состояние словами: «работает, Выпечка», «нет связи 5 мин».
	Text string
	// Tone - вид метки состояния (stateTone).
	Tone string
	// Problem - что требует внимания (problemLabels); Notice - то же словами на метке
	// прибора, пусто - хватает основной метки. Problem пусто - ничего.
	Problem, Notice string
	// Switch - у устройства Алисы есть умение on_off, и прибор на связи; On - он включён.
	Switch, On bool
}

// stateTone - вид метки состояния: run - работает, idle - на связи и не работает,
// off - нет связи, warn - требует внимания, err - ошибка.
func stateTone(online, working, waiting bool) string {
	switch {
	case waiting:
		return "warn"
	case !online:
		return "off"
	case working:
		return "run"
	}
	return "idle"
}

func (s *Server) status(id string, d config.Device) status {
	var st status
	dev, built := s.gw.Device(id)
	hc, isHC := s.gw.HomeConnect(id)
	working := false
	switch {
	case isHC:
		ast := hc.Appliance.State()
		st.Online, st.Text = ast.Online, summary(&ast, hc.Profile)
		working = hcdevice.Working(hc.Profile, &ast)
	case built:
		st.Online, st.Text = aliceSummary(dev, id)
	case s.gw.Waiting(id):
		st.Waiting, st.Text = true, waitingText(d.Source)
	}
	if built && st.Online {
		st.Switch, st.On = power(dev, id)
	}
	switch {
	case !st.Online && d.Source == config.SourceHomeConnectCloud && !s.gw.Cloud.API.LoggedIn():
		// Без входа прибор облака не появится на связи сам: причину надо назвать.
		st.Text = "нет входа в Home Connect"
	case st.Waiting:
	case !st.Online:
		st.Text = s.offlineText(id)
	case st.Text == "":
		st.Text = "на связи"
	}
	st.Tone = stateTone(st.Online, working || !isHC && st.On, st.Waiting)
	st.Problem, st.Notice = s.problem(id, d, &st)
	return st
}

// waitingText - чего ждёт прибор облака, которого ещё нет в Алисе.
func waitingText(source string) string {
	switch source {
	case config.SourceThinQ:
		return "ждёт профиля прибора LG"
	case config.SourceSmartThings:
		return "ждёт описания SmartThings"
	}
	return "ждёт данных облака Home Connect"
}

// offlineText - «нет связи» и сколько: «нет связи 5 мин», «нет связи с запуска шлюза».
func (s *Server) offlineText(id string) string {
	since, fromStart, ok := s.gw.Offline(id)
	switch {
	case !ok:
		return "нет связи"
	case fromStart:
		return "нет связи с запуска шлюза"
	}
	return "нет связи" + ago(time.Since(since))
}

// ago - сколько прошло, коротко: « 5 мин», « 3 ч», « 2 дн». Меньше минуты - пусто.
// Часы, а не время пропажи: часовой пояс контейнера может не совпадать с домашним.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return " " + strconv.Itoa(int(d/time.Minute)) + " мин"
	case d < 48*time.Hour:
		return " " + strconv.Itoa(int(d/time.Hour)) + " ч"
	}
	return " " + strconv.Itoa(int(d/(24*time.Hour))) + " дн"
}

// problemLabels - что требует внимания, в порядке полосы над комнатами.
var problemLabels = []choice{{Value: "offline", Label: "без связи"}, {Value: "waiting", Label: "ждут данных облака"},
	{Value: "tv", Label: "ждут разрешения телевизора"}, {Value: "pin", Label: "ждут подтверждения сертификата"},
	{Value: "battery", Label: "разряжены"}, {Value: "food", Label: "нужно пополнить"}}

// problem - что в приборе требует внимания: ключ из problemLabels и слова для метки прибора.
// Телевизору без разрешения и кофемашине без подтверждённого сертификата шлюз не может
// подключиться, поэтому они - раньше, чем «нет связи».
func (s *Server) problem(id string, d config.Device, st *status) (string, string) {
	if t, ok := s.gw.Television(id); ok {
		switch l := t.Link(); {
		case l.Prompt:
			return "tv", "ждёт разрешения на экране"
		case !l.Paired:
			return "tv", "ещё не разрешил подключение"
		}
	}
	if hc, ok := s.gw.HomeConnect(id); ok && d.Pin == "" {
		if ap, ok := hc.Appliance.(*delonghi.Appliance); ok && ap.Observed() != "" {
			return "pin", "подтвердите сертификат"
		}
	}
	switch {
	case st.Waiting:
		return "waiting", ""
	case !st.Online:
		return "offline", ""
	}
	dev, ok := s.gw.Device(id)
	if !ok {
		return "", ""
	}
	if level, ok := battery(dev, id); ok && level <= batteryLow {
		return "battery", "заряд " + formatValue(level) + " %"
	}
	if foodLow(dev) {
		return "food", "нужно пополнить"
	}
	return "", ""
}

// battery - заряд прибора в процентах, если прибор его сообщает.
func battery(dev alice.Device, id string) (float64, bool) {
	for _, st := range dev.States() {
		if st.ID != id {
			continue
		}
		for _, p := range st.Properties {
			if sv, ok := p.State.(alice.StateValue); ok && p.Type == alice.PropFloat && sv.Instance == "battery_level" {
				v, ok := sv.Value.(float64)
				return v, ok
			}
		}
	}
	return 0, false
}

// foodLow - датчик событий прибора сообщает, что что-то заканчивается: соль, вода, зёрна.
func foodLow(dev alice.Device) bool {
	for _, st := range dev.States() {
		for _, p := range st.Properties {
			if sv, ok := p.State.(alice.StateValue); ok && p.Type == alice.PropEvent && sv.Instance == "food_level" && sv.Value == "low" {
				return true
			}
		}
	}
	return false
}

// attention - полоса «Требует внимания»: число приборов по каждой причине словами, пусто -
// ничего не требует внимания.
func attention(rows []deviceRow) string {
	count := map[string]int{}
	for _, r := range rows {
		count[r.Problem]++
	}
	var parts []string
	for _, p := range problemLabels {
		if n := count[p.Value]; n > 0 {
			parts = append(parts, p.Label+": "+strconv.Itoa(n))
		}
	}
	return strings.Join(parts, " · ")
}

// problemList - причины для скрипта: «ключ:слова» через «|», в порядке полосы.
func problemList() string {
	parts := make([]string, len(problemLabels))
	for i, p := range problemLabels {
		parts[i] = p.Value + ":" + p.Label
	}
	return strings.Join(parts, "|")
}
