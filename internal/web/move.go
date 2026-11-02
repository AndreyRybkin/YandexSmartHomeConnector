package web

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/config"
	"hcgateway/internal/hclocal"
)

// Перевод прибора Home Connect между облаком и домашней сетью. Меняется только способ связи:
// идентификатор прибора, имя, комната, значок и сопоставления остаются, поэтому сценарии Алисы
// продолжают работать.

const (
	// moveTimeout - предел проверки связи перед переводом.
	moveTimeout = 15 * time.Second
	// moveListed - сколько имён показывать в итоге перевода.
	moveListed = 6
)

// moveView - страница перевода прибора.
type moveView struct {
	ID, Name, Title string
	// ToLocal - перевод на локальную связь, иначе в облако.
	ToLocal bool
	// Candidates - приборы, подходящие по haId; больше одного - выбор вручную.
	Candidates []choice
	Host       string
	// Found - адреса приборов Home Connect из последнего поиска в сети.
	Found []string
	// Twin - другой прибор шлюза с тем же haId: перевод сделал бы его дублем.
	Twin string
	// NoKeys - нет ключей этого прибора; NoLogin - нет входа в облако.
	NoKeys, NoLogin bool
	Err             string
}

func hcSource(source string) bool {
	return source == config.SourceHomeConnectCloud || source == config.SourceHomeConnectLocal
}

// baseHaID - haId без суффикса номера прибора в API приборов.
func baseHaID(haID string) string {
	return suffixRe.ReplaceAllString(haID, "")
}

// otherDevice - другой прибор шлюза с тем же haId до суффикса.
func otherDevice(cfg *config.Config, id, haID string) string {
	base := baseHaID(haID)
	for _, other := range slices.Sorted(maps.Keys(cfg.Devices)) {
		if d := cfg.Devices[other]; other != id && hcSource(d.Source) && baseHaID(d.HaID) == base {
			return other
		}
	}
	return ""
}

// moveForm собирает страницу по текущим данным: picked - выбранный прибор, host - адрес из формы.
func (s *Server) moveForm(cfg *config.Config, id string, d config.Device, picked, host string) moveView {
	v := moveView{ID: id, Name: d.Name, ToLocal: d.Source == config.SourceHomeConnectCloud, Host: host,
		Twin: otherDevice(cfg, id, d.HaID)}
	base := baseHaID(d.HaID)
	if v.ToLocal {
		v.Title = "Перевести «" + d.Name + "» на локальную связь"
		for _, info := range s.gw.LocalAppliances() {
			if baseHaID(info.HaID) == base {
				v.Candidates = append(v.Candidates, choice{Value: info.HaID, Label: info.Brand + " " + typeName(info.HaType) + ", " + info.HaID,
					Selected: info.HaID == picked})
			}
		}
		v.NoKeys = len(v.Candidates) == 0
		for _, f := range s.gw.Discovery.Status().Found {
			if f.Source == config.SourceHomeConnectLocal {
				v.Found = append(v.Found, f.Host)
			}
		}
		return v
	}
	v.Title = "Перевести «" + d.Name + "» в облако Home Connect"
	v.NoLogin = !s.gw.Cloud.API.LoggedIn()
	for _, h := range s.gw.Cloud.Account() {
		if baseHaID(h.HaID) == base {
			v.Candidates = append(v.Candidates, choice{Value: h.HaID, Label: strings.TrimSpace(h.Brand+" "+h.Name) + ", " + h.HaID,
				Selected: h.HaID == picked})
		}
	}
	return v
}

// blocked - перевод невозможен, и страница говорит почему.
func (v *moveView) blocked() bool {
	return v.Twin != "" || v.NoKeys || v.NoLogin
}

// movePage показывает форму перевода.
func (s *Server) movePage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	cfg, _ := s.file.Current()
	d, ok := cfg.Devices[id]
	if !ok || !hcSource(d.Source) {
		s.notFound(w, sess)
		return
	}
	s.render(w, http.StatusOK, "move", s.newPage(sess, "devices", "Перевод прибора", s.moveForm(cfg, id, d, "", "")))
}

// moveDevice проверяет связь по новому способу и только после этого меняет настройки: ошибка в
// адресе не отключит прибор в Алисе.
func (s *Server) moveDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	cfg, _ := s.file.Current()
	d, ok := cfg.Devices[id]
	if !ok || !hcSource(d.Source) {
		s.notFound(w, sess)
		return
	}
	f := r.PostForm
	picked, host := f.Get("ha_id"), strings.TrimSpace(f.Get("host"))
	v := s.moveForm(cfg, id, d, picked, host)
	refuse := func(text string) {
		v.Err = text
		s.render(w, http.StatusBadRequest, "move", s.newPage(sess, "devices", "Перевод прибора", v))
	}
	if v.blocked() {
		refuse("")
		return
	}
	if v.ToLocal && !config.ValidHost(host) {
		refuse("Нужен адрес прибора в домашней сети: имя или IP, порт по желанию.")
		return
	}
	if !s.moving.TryLock() {
		refuse("Другой перевод ещё идёт: повторите через несколько секунд.")
		return
	}
	defer s.moving.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), moveTimeout)
	defer cancel()
	if !v.ToLocal {
		// Список - свежий: прибор мог пропасть из аккаунта или уйти со связи.
		if err := s.gw.Cloud.RefreshAccount(ctx); err != nil {
			s.log.Warn("перевод в облако: список приборов не загружен", "device", id, "err", err)
			refuse("Не удалось получить список приборов аккаунта, подробности в журнале шлюза.")
			return
		}
		v = s.moveForm(cfg, id, d, picked, host)
		if len(v.Candidates) == 0 {
			refuse("В аккаунте Home Connect нет этого прибора.")
			return
		}
	}
	target := ""
	switch {
	case slices.ContainsFunc(v.Candidates, func(c choice) bool { return c.Value == picked }):
		target = picked
	case len(v.Candidates) == 1:
		target = v.Candidates[0].Value
	default:
		refuse("Выберите прибор.")
		return
	}
	if v.ToLocal {
		if err := s.gw.CheckLocal(ctx, target, host); err != nil {
			s.log.Warn("перевод на локальную связь: нет связи с прибором", "device", id, "host", host, "err", err)
			if errors.Is(err, hclocal.ErrWrongKey) {
				refuse("Прибор по этому адресу не принял ключ: возможно, это другой прибор или ключи устарели. Настройки не изменились.")
			} else {
				refuse("Нет связи с прибором по этому адресу: проверьте адрес и что прибор в сети. Настройки не изменились.")
			}
			return
		}
	} else if !s.accountOnline(target) {
		refuse("Облако сообщает, что прибор не на связи: повторите, когда он появится в сети. Настройки не изменились.")
		return
	}
	var before appliance.Description
	if hc, ok := s.gw.HomeConnect(id); ok {
		before = hc.Appliance.Describe()
	}
	err := s.change(f.Get("version"), "devices."+id, func(c *config.Config) error {
		cur, ok := c.Devices[id]
		if !ok || cur.Source != d.Source {
			return invalid("Прибор %s изменился, пока шла проверка: откройте страницу заново", id)
		}
		cur.HaID = target
		if v.ToLocal {
			cur.Source, cur.Host = config.SourceHomeConnectLocal, host
		} else {
			cur.Source, cur.Host = config.SourceHomeConnectCloud, ""
		}
		c.Devices[id] = cur
		return nil
	})
	if err != nil {
		refuse(s.explain("devices."+id, err))
		return
	}
	text := "Прибор переведён в облако Home Connect."
	if v.ToLocal {
		// Облачный объект прибора больше не нужен: иначе каждое подключение потока тратило бы на него запросы.
		s.gw.Cloud.Forget(d.HaID)
		text = "Прибор переведён на локальную связь."
	}
	s.log.Info("прибор переведён", "device", id, "to_local", v.ToLocal)
	if hc, ok := s.gw.HomeConnect(id); ok {
		text += " " + moveChanges(before, hc.Appliance.Describe())
	}
	s.done(w, r, sess, "/devices/"+id+"#move", text)
}

// accountOnline сообщает, на связи ли прибор аккаунта по свежему списку.
func (s *Server) accountOnline(haID string) bool {
	for _, h := range s.gw.Cloud.Account() {
		if h.HaID == haID {
			return h.Connected
		}
	}
	return false
}

// moveChanges - что изменилось у прибора после перевода, словами: у облака и прибора списки
// программ и параметров могут отличаться.
func moveChanges(before, after appliance.Description) string {
	if len(after.Programs) == 0 && len(after.Options) == 0 {
		return "Программы и параметры шлюз загрузит из облака, список в Алисе обновится сам."
	}
	var parts []string
	if before.Type != "" && before.Type != after.Type {
		parts = append(parts, "Тип прибора: "+typeName(after.Type)+" вместо прежнего "+typeName(before.Type)+".")
	}
	gone, added := listDiff(before.Programs, after.Programs)
	parts = append(parts, moveList("Пропали программы", gone, programShort), moveList("Появились программы", added, programShort))
	gone, added = listDiff(optionKeys(before), optionKeys(after))
	parts = append(parts, moveList("Пропали параметры", gone, short), moveList("Появились параметры", added, short))
	text := strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), " ")
	if text == "" {
		return "Программы и параметры те же."
	}
	return text
}

func optionKeys(d appliance.Description) []string {
	out := make([]string, 0, len(d.Options))
	for k := range d.Options {
		out = append(out, k)
	}
	return out
}

// listDiff - чего нет в after и чего не было в before, по порядку строк.
func listDiff(before, after []string) (gone, added []string) {
	for _, k := range before {
		if !slices.Contains(after, k) {
			gone = append(gone, k)
		}
	}
	for _, k := range after {
		if !slices.Contains(before, k) {
			added = append(added, k)
		}
	}
	slices.Sort(gone)
	slices.Sort(added)
	return gone, added
}

// moveList - «Пропали программы: A, B и ещё 3.»; пустой список - пустая строка.
func moveList(title string, keys []string, name func(string) string) string {
	if len(keys) == 0 {
		return ""
	}
	shown := keys[:min(len(keys), moveListed)]
	names := make([]string, len(shown))
	for i, k := range shown {
		names[i] = name(k)
	}
	text := title + ": " + strings.Join(names, ", ")
	if extra := len(keys) - len(shown); extra > 0 {
		text += " и ещё " + strconv.Itoa(extra)
	}
	return text + "."
}
