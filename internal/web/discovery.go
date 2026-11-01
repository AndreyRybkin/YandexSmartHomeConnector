package web

import (
	"errors"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"hcgateway/internal/config"
	"hcgateway/internal/discovery"
)

// Поиск приборов в домашней сети на странице «Подключения».

// discoveryRefresh - как часто страница обновляется сама, пока идёт поиск, в секундах.
const discoveryRefresh = 2

// discoveryView - поиск на странице «Подключения». Network - сеть для поля: последнего
// поиска, из настроек или по адресам приборов; Searching - сеть идущего или прошлого поиска.
type discoveryView struct {
	Network, Searching string
	Running, Partial   bool
	Checked, Total     int
	// Searched - поиск был, и его находки ещё свежие.
	Searched bool
	Rows     []foundRow
}

// foundRow - находка и что с ней делать. Device - прибор настроек с этим адресом; Moved -
// прибор настроек, которого поиск узнал по MAC или серийному номеру, но по другому адресу
// OldHost.
type foundRow struct {
	Index                     int
	Kind, Name, Host, MAC     string
	Device, DeviceName        string
	Moved, MovedName, OldHost string
}

func (s *Server) discoveryView(cfg *config.Config) discoveryView {
	st := s.gw.Discovery.Status()
	v := discoveryView{Network: st.Network, Searching: st.Network, Running: st.Running, Partial: st.Partial,
		Checked: st.Checked, Total: st.Total, Searched: st.Running || st.Fresh}
	if v.Network == "" {
		v.Network = cfg.Discovery.Network
	}
	if v.Network == "" {
		v.Network = defaultNetwork(cfg)
	}
	for i, f := range st.Found {
		r := foundRow{Index: i, Kind: discovery.Label(f.Source), Name: strings.TrimSpace(f.Name + " " + f.Model), Host: f.Host, MAC: f.MAC}
		r.Device, r.Moved = s.match(cfg, f)
		r.DeviceName = cfg.Devices[r.Device].Name
		if r.Moved != "" {
			r.MovedName, r.OldHost = cfg.Devices[r.Moved].Name, cfg.Devices[r.Moved].Host
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

// defaultNetwork - сеть поиска по адресам приборов: сеть /24, в которой больше всего
// приборов. Приборов с частным IPv4 нет - пусто.
func defaultNetwork(cfg *config.Config) string {
	count := map[netip.Prefix]int{}
	for _, d := range cfg.Devices {
		ip, err := netip.ParseAddr(bareHost(d.Host))
		if err != nil || !ip.Is4() || !ip.IsPrivate() {
			continue
		}
		p, _ := ip.Prefix(24)
		count[p]++
	}
	var best netip.Prefix
	for _, p := range slices.SortedFunc(maps.Keys(count), func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) }) {
		if !best.IsValid() || count[p] > count[best] {
			best = p
		}
	}
	if !best.IsValid() {
		return ""
	}
	return best.String()
}

// bareHost - адрес прибора без порта и скобок.
func bareHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

// match - прибор настроек для находки f: same - с тем же адресом; moved - узнанный по MAC
// или серийному номеру, но с другим адресом. Телевизор LG и прибор Home Connect поиск по MAC
// не узнаёт: они его не сообщают.
func (s *Server) match(cfg *config.Config, f discovery.Found) (same, moved string) {
	ids := slices.Sorted(maps.Keys(cfg.Devices))
	for _, id := range ids {
		if d := cfg.Devices[id]; d.Source == f.Source && bareHost(d.Host) == f.Host {
			return id, ""
		}
	}
	for _, id := range ids {
		d := cfg.Devices[id]
		if d.Source != f.Source {
			continue
		}
		mac := s.knownMAC(id, d)
		if f.MAC != "" && strings.EqualFold(mac, f.MAC) || f.Serial != "" && d.Serial == f.Serial {
			return "", id
		}
	}
	return "", ""
}

// knownMAC - MAC прибора настроек, который знает шлюз: ленту - из её сообщений, телевизор -
// из настроек или того, что он сообщил сам.
func (s *Server) knownMAC(id string, d config.Device) string {
	switch d.Source {
	case config.SourceWLED:
		if strip, ok := s.gw.Strip(id); ok {
			return strip.MAC()
		}
	case config.SourceSamsungTV, config.SourceWebOS:
		if d.MAC != "" {
			return d.MAC
		}
		if t, ok := s.gw.Television(id); ok {
			return t.Link().MAC
		}
	}
	return ""
}

// discover начинает поиск в сети из формы и запоминает сеть в настройках.
func (s *Server) discover(w http.ResponseWriter, r *http.Request, sess *session) {
	const to = "/connections#discovery"
	raw := r.PostForm.Get("network")
	if len(raw) > 64 {
		s.failed(w, r, sess, to, "Поиск не начат: неверная сеть.")
		return
	}
	p, err := config.ParseNetwork(raw)
	if err != nil {
		s.failed(w, r, sess, to, "Поиск не начат: "+err.Error()+".")
		return
	}
	if cfg, _ := s.file.Current(); cfg.Discovery.Network != p.String() {
		err := s.change(r.PostForm.Get("version"), "discovery", func(c *config.Config) error {
			c.Discovery.Network = p.String()
			return nil
		})
		if err != nil {
			s.failed(w, r, sess, to, s.explain("discovery", err))
			return
		}
	}
	switch err := s.gw.Discovery.Start(p, s.gw.CoffeeAddresses); {
	case errors.Is(err, discovery.ErrBusy):
		s.failed(w, r, sess, to, "Поиск уже идёт.")
		return
	case errors.Is(err, discovery.ErrTooSoon):
		s.failed(w, r, sess, to, "Прошлый поиск закончился меньше минуты назад: подождите и нажмите «Найти» снова.")
		return
	case err != nil:
		s.failed(w, r, sess, to, "Поиск не начат: "+err.Error()+".")
		return
	}
	s.log.Info("поиск в домашней сети начат", "network", p.String())
	s.done(w, r, sess, to, "Поиск начат: шлюз проверит адреса сети "+p.String()+".")
}

// foundAt - находка поиска по номеру из адреса страницы.
func (s *Server) foundAt(raw string) (discovery.Found, bool) {
	i, err := strconv.Atoi(raw)
	st := s.gw.Discovery.Status()
	if err != nil || i < 0 || i >= len(st.Found) || st.Running {
		return discovery.Found{}, false
	}
	return st.Found[i], true
}

// foundNames - имя нового прибора, если находка своего не сообщила.
var foundNames = map[string]string{config.SourceWLED: "Лента", config.SourceSamsungTV: "Телевизор",
	config.SourceWebOS: "Телевизор", config.SourceHomeConnectLocal: "Прибор Home Connect", config.SourceDeLonghi: "Кофемашина"}

// foundForm - форма добавления прибора по находке поиска.
func foundForm(f discovery.Found) config.Device {
	d := config.Device{Source: f.Source, Host: f.Host, Name: f.Name, Serial: f.Serial}
	if d.Name == "" {
		d.Name = foundNames[f.Source]
	}
	if f.Source == config.SourceSamsungTV || f.Source == config.SourceWebOS {
		d.MAC = f.MAC
	}
	return d
}

// moveFound меняет адрес прибора настроек, которого поиск нашёл по другому адресу.
func (s *Server) moveFound(w http.ResponseWriter, r *http.Request, sess *session) {
	const to = "/connections#discovery"
	f, ok := s.foundAt(r.PostForm.Get("found"))
	cfg, _ := s.file.Current()
	if !ok {
		s.failed(w, r, sess, to, "Находки устарели: найдите приборы заново.")
		return
	}
	_, id := s.match(cfg, f)
	if id == "" {
		s.failed(w, r, sess, to, "Прибор с другим адресом не найден: адрес уже обновлён или прибор удалён.")
		return
	}
	err := s.change(r.PostForm.Get("version"), "devices."+id, func(c *config.Config) error {
		d := c.Devices[id]
		host := f.Host
		if _, port, err := net.SplitHostPort(d.Host); err == nil {
			host = net.JoinHostPort(f.Host, port)
		}
		d.Host = host
		c.Devices[id] = d
		return nil
	})
	if err != nil {
		s.failed(w, r, sess, to, s.explain("devices."+id, err))
		return
	}
	s.log.Info("адрес прибора обновлён по поиску", "device", id, "host", f.Host)
	s.done(w, r, sess, to, "Адрес прибора «"+cfg.Devices[id].Name+"» обновлён: "+f.Host+".")
}
