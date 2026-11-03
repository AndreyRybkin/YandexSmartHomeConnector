package web

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"hcgateway/internal/config"
	"hcgateway/internal/mqttbroker"
)

// Страница /mqtt: брокер встроен в шлюз, и без неё не понять, почему прибор не подключается и что
// он присылает. Поток /mqtt/stream обновляет её без перезагрузки.

// mqttView - страница брокера.
type mqttView struct {
	Listen, Username, Password string
	// Open - порт открыт; Pending - пароль задан, порт откроется после перезапуска; Err - почему порт
	// не открылся.
	Open, Pending bool
	Err           string
	Filter        string
	// Stream - адрес потока с текущим фильтром.
	Stream string
	Live   mqttSnapshot
	Send   mqttMessage
}

// mqttMessage - сообщение из формы отправки.
type mqttMessage struct {
	Topic, Payload, QoS string
	Retain              bool
}

// mqttSnapshot - то, что меняется: его отдаёт поток SSE.
type mqttSnapshot struct {
	Clients  []mqttClientRow  `json:"clients"`
	Journal  []mqttJournalRow `json:"journal"`
	Values   []mqttValueRow   `json:"values"`
	Failures []mqttFailRow    `json:"failures"`
	Saved    int              `json:"saved"`
	Unsaved  int              `json:"unsaved"`
}

type mqttClientRow struct {
	ID      string `json:"id"`
	Remote  string `json:"remote"`
	User    string `json:"user"`
	Version string `json:"version"`
	Since   string `json:"since"`
}

type mqttJournalRow struct {
	Time    string `json:"time"`
	Dir     string `json:"dir"`
	Client  string `json:"client"`
	Topic   string `json:"topic"`
	Payload string `json:"payload"`
	Flags   string `json:"flags"`
}

type mqttValueRow struct {
	Topic   string `json:"topic"`
	Payload string `json:"payload"`
	Client  string `json:"client"`
	Time    string `json:"time"`
	Flags   string `json:"flags"`
}

type mqttFailRow struct {
	Time   string `json:"time"`
	Remote string `json:"remote"`
	User   string `json:"user"`
}

// protocolNames - версии MQTT по номеру в CONNECT.
var protocolNames = map[byte]string{3: "3.1", 4: "3.1.1", 5: "5.0"}

const mqttTime = "02.01 15:04:05"

// mqttStatus - состояние брокера; брокера нет - порт закрыт.
func (s *Server) mqttStatus(filter string) mqttbroker.Status {
	if s.broker == nil {
		return mqttbroker.Status{}
	}
	return s.broker.Status(filter)
}

func mqttSnap(st mqttbroker.Status) mqttSnapshot {
	sn := mqttSnapshot{Saved: st.Saved, Unsaved: st.Unsaved}
	for _, c := range st.Clients {
		sn.Clients = append(sn.Clients, mqttClientRow{c.ID, c.Remote, c.User, protocolNames[c.Version], c.Since.Local().Format(mqttTime)})
	}
	for _, e := range st.Journal {
		dir := "от клиента"
		if e.Dir == mqttbroker.Out {
			dir = "от шлюза"
		}
		var flags []string
		if e.Retain {
			flags = append(flags, "сохраняется")
		}
		if e.PayloadCut {
			flags = append(flags, "обрезано")
		}
		sn.Journal = append(sn.Journal, mqttJournalRow{e.Time.Local().Format(mqttTime), dir, e.Client, e.Topic, e.Payload, strings.Join(flags, ", ")})
	}
	for _, v := range st.Values {
		flags := ""
		switch {
		case v.Saved:
			flags = "из базы: с запуска шлюза не приходило"
		case v.Retain:
			flags = "сохраняется"
		}
		sn.Values = append(sn.Values, mqttValueRow{v.Topic, v.Payload, v.Client, v.Time.Local().Format(mqttTime), flags})
	}
	for _, f := range st.Failures {
		sn.Failures = append(sn.Failures, mqttFailRow{f.Time.Local().Format(mqttTime), f.Remote, f.User})
	}
	return sn
}

// mqttFilter - фильтр топиков из адреса: начало топика, не длиннее предела.
func mqttFilter(r *http.Request) string {
	f := strings.TrimSpace(r.URL.Query().Get("topic"))
	if len(f) > mqttbroker.MaxTopicBytes {
		return ""
	}
	return f
}

func (s *Server) mqttPage(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, _ := s.file.Current()
	filter := mqttFilter(r)
	st := s.mqttStatus(filter)
	v := mqttView{Listen: cfg.MQTT.Listen, Username: cfg.MQTT.Username, Password: cfg.MQTT.Password, Open: st.Open, Err: st.Err,
		Pending: cfg.MQTT.Password != "" && !st.Open && st.Err == "", Filter: filter, Stream: "/mqtt/stream", Live: mqttSnap(st), Send: mqttMessage{QoS: "0"}}
	if filter != "" {
		v.Stream += "?topic=" + url.QueryEscape(filter)
	}
	s.render(w, http.StatusOK, "mqtt", s.newPage(sess, "mqtt", "MQTT", v))
}

func (s *Server) mqttStream(w http.ResponseWriter, r *http.Request, _ *session) {
	filter := mqttFilter(r)
	s.sse(w, r, "mqtt", func() any { return mqttSnap(s.mqttStatus(filter)) })
}

// parseMQTTMessage проверяет сообщение из формы до обращения к брокеру.
func parseMQTTMessage(f url.Values) (m mqttMessage, qos byte, errText string) {
	m = mqttMessage{Topic: strings.TrimSpace(f.Get("topic")), Payload: f.Get("payload"), QoS: f.Get("qos"), Retain: f.Get("retain") != ""}
	switch m.QoS {
	case "", "0":
		m.QoS = "0"
	case "1":
		qos = 1
	case "2":
		qos = 2
	default:
		return m, 0, "QoS - это 0, 1 или 2."
	}
	switch {
	case m.Topic == "" || len(m.Topic) > mqttbroker.MaxTopicBytes || !utf8.ValidString(m.Topic):
		return m, 0, "Топик - от 1 до 256 байт в UTF-8."
	case strings.ContainsAny(m.Topic, "+#\x00") || strings.HasPrefix(m.Topic, "$"):
		return m, 0, "В топике для отправки не бывает +, # и $ в начале."
	case len(m.Payload) > mqttbroker.MaxPayloadBytes:
		return m, 0, "Значение - не больше 16 КБ."
	}
	return m, qos, ""
}

// mqttPublishPage спрашивает подтверждение: команда уйдёт настоящему прибору, например включит чайник.
func (s *Server) mqttPublishPage(w http.ResponseWriter, r *http.Request, sess *session) {
	m, _, errText := parseMQTTMessage(r.URL.Query())
	if errText != "" {
		s.failed(w, r, sess, "/mqtt#send", errText)
		return
	}
	text := "Значение: «" + m.Payload + "». QoS " + m.QoS + "."
	if m.Payload == "" {
		text = "Значение пустое."
	}
	if m.Retain {
		text += " Брокер сохранит сообщение и отдаст его новым подписчикам."
	}
	text += " Прибор выполнит команду сразу."
	fields := []kv{{"topic", m.Topic}, {"payload", m.Payload}, {"qos", m.QoS}}
	if m.Retain {
		fields = append(fields, kv{"retain", "on"})
	}
	s.render(w, http.StatusOK, "confirm", s.newPage(sess, "mqtt", "MQTT", confirmView{Title: "Отправить сообщение в " + m.Topic + "?", Text: text,
		Action: "/mqtt/publish", Button: "Отправить", Back: "/mqtt#send", Fields: fields}))
}

// mqttPublish отправляет сообщение от имени шлюза.
func (s *Server) mqttPublish(w http.ResponseWriter, r *http.Request, sess *session) {
	m, qos, errText := parseMQTTMessage(r.PostForm)
	if errText != "" {
		s.failed(w, r, sess, "/mqtt#send", errText)
		return
	}
	if s.broker == nil {
		s.failed(w, r, sess, "/mqtt#send", "Брокер не запущен.")
		return
	}
	if err := s.broker.Publish(m.Topic, []byte(m.Payload), m.Retain, qos); err != nil {
		s.log.Warn("сообщение MQTT не отправлено", "topic", m.Topic, "err", err)
		s.failed(w, r, sess, "/mqtt#send", "Сообщение не отправлено, подробности в журнале шлюза.")
		return
	}
	s.log.Info("сообщение MQTT отправлено из интерфейса", "topic", m.Topic)
	s.done(w, r, sess, "/mqtt#send", "Сообщение отправлено.")
}

// mqttPasswordPage спрашивает подтверждение: со сменой пароля клиенты перестанут входить.
func (s *Server) mqttPasswordPage(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, _ := s.file.Current()
	text := "Шлюз создаст случайный пароль и покажет его на странице. Порт брокера откроется после перезапуска шлюза."
	if cfg.MQTT.Password != "" {
		text = "Новый пароль действует сразу: подключённые клиенты останутся на связи, но при следующем подключении не смогут войти, пока вы не впишете им новый пароль."
	}
	s.render(w, http.StatusOK, "confirm", s.newPage(sess, "mqtt", "MQTT", confirmView{Title: "Создать новый пароль брокера?", Text: text,
		Action: "/mqtt/password", Button: "Создать пароль", Back: "/mqtt"}))
}

// mqttPassword создаёт пароль брокера.
func (s *Server) mqttPassword(w http.ResponseWriter, r *http.Request, sess *session) {
	password, err := randomToken()
	if err != nil {
		s.log.Error("пароль брокера не создан", "err", err)
		s.failed(w, r, sess, "/mqtt", "Пароль не создан, подробности в журнале шлюза.")
		return
	}
	err = s.change(r.PostForm.Get("version"), "mqtt.password", func(c *config.Config) error {
		c.MQTT.Password = password
		return nil
	})
	if err != nil {
		s.failed(w, r, sess, "/mqtt", s.explain("mqtt", err))
		return
	}
	s.log.Info("пароль брокера MQTT изменён")
	s.done(w, r, sess, "/mqtt", "Пароль создан.")
}
