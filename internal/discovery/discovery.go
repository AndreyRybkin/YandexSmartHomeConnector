// Package discovery ищет приборы в домашней сети по кнопке «Найти». Шлюз спрашивает каждый
// адрес сети напрямую, без широковещательных пакетов: так поиск работает и из обычной сети
// Docker, откуда широковещательные пакеты в домашнюю сеть не выходят.
package discovery

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"hcgateway/internal/config"
	"hcgateway/internal/hclocal"
	"hcgateway/internal/samsungtv"
	"hcgateway/internal/tlspsk"
	"hcgateway/internal/webos"
	"hcgateway/internal/wled"
)

const (
	// connectTimeout - сколько ждать ответа порта; probeTimeout - ответа прибора на проверку;
	// tlsTimeout - рукопожатия TLS: кофемашина на ESP32 отвечает полторы секунды.
	connectTimeout = 400 * time.Millisecond
	probeTimeout   = 2 * time.Second
	tlsTimeout     = 4 * time.Second
	// searchTimeout - самый долгий поиск; workers - сколько адресов проверяются сразу.
	searchTimeout = time.Minute
	workers       = 64
	// keep - сколько находки хранятся после поиска; pause - пауза между поисками.
	keep  = 10 * time.Minute
	pause = time.Minute
	// awsCertificate - имя в сертификате кофемашины De'Longhi на платформе AWS IoT.
	awsCertificate = "AWS IoT Certificate"
)

// ports - порты, по которым поиск узнаёт приборы: лента WLED - 80, прибор Home Connect и
// кофемашина - 443, телевизор Samsung - 8001, телевизор LG - 3001 и 3000.
var ports = []string{"80", "443", "8001", "3001", "3000"}

// Found - прибор, который нашёл поиск. Source - источник в настройках шлюза, Host - IP,
// Name и Model - имя и модель, если прибор их сообщил. MAC и Serial - по ним шлюз узнаёт
// прибор настроек после смены адреса.
type Found struct {
	Source, Host, Name, Model, MAC, Serial string
}

// Status - поиск для страницы: идёт ли, в какой сети, сколько адресов проверено, находки.
// Partial - поиск остановился по сроку и проверил не все адреса; Fresh - поиск закончился
// меньше 10 минут назад, и находки ещё верны.
type Status struct {
	Running        bool
	Network        string
	Checked, Total int
	Partial, Fresh bool
	Found          []Found
	Finished       time.Time
}

var (
	// ErrBusy - поиск уже идёт.
	ErrBusy = errors.New("поиск уже идёт")
	// ErrTooSoon - прошлый поиск закончился меньше минуты назад.
	ErrTooSoon = errors.New("прошлый поиск закончился меньше минуты назад")
)

// Probe - проверка одного адреса: какие приборы на нём отвечают. Тесты её подменяют.
type Probe func(ctx context.Context, ip netip.Addr) []Found

// Search - поиск в домашней сети: один за раз и не чаще раза в минуту.
type Search struct {
	probe Probe
	now   func() time.Time

	mu       sync.Mutex
	st       Status
	finished time.Time
}

// New - поиск, который проверяет адреса probe; nil - настоящими проверками (Check).
func New(probe Probe) *Search {
	if probe == nil {
		probe = Check
	}
	return &Search{probe: probe, now: time.Now}
}

// Start начинает поиск в сети p в фоне. cloud - приборы, адреса которых сообщило облако,
// по адресу: поиск дополняет ими то, что нашёл сам на этих адресах. Вызывается в фоне.
func (s *Search) Start(p netip.Prefix, cloud func(ctx context.Context) map[netip.Addr]Found) error {
	p = p.Masked()
	if !p.Addr().Is4() || p.Bits() < config.MinNetworkBits {
		return errors.New("сеть поиска больше 1024 адресов")
	}
	addrs := hosts(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.st.Running:
		return ErrBusy
	case !s.finished.IsZero() && s.now().Sub(s.finished) < pause:
		return ErrTooSoon
	}
	s.st = Status{Running: true, Network: p.String(), Total: len(addrs)}
	go s.run(addrs, cloud)
	return nil
}

func (s *Search) run(addrs []netip.Addr, cloud func(ctx context.Context) map[netip.Addr]Found) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()
	known := map[netip.Addr]Found{}
	if cloud != nil {
		known = cloud(ctx)
	}
	next := make(chan netip.Addr)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range next {
				found := s.probe(ctx, ip)
				for i, f := range found {
					if k, ok := known[ip]; ok && k.Source == f.Source {
						found[i] = merge(f, k)
					}
				}
				s.mu.Lock()
				s.st.Checked++
				s.st.Found = append(s.st.Found, found...)
				s.mu.Unlock()
			}
		}()
	}
	partial := false
send:
	for _, ip := range addrs {
		select {
		case next <- ip:
		case <-ctx.Done():
			partial = true
			break send
		}
	}
	close(next)
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	slices.SortFunc(s.st.Found, func(a, b Found) int {
		return netip.MustParseAddr(a.Host).Compare(netip.MustParseAddr(b.Host))
	})
	s.st.Running, s.st.Partial = false, partial || ctx.Err() != nil
	s.finished = s.now()
	s.st.Finished = s.finished
}

// merge дополняет найденное f тем, что о приборе сообщило облако.
func merge(f, k Found) Found {
	if f.Name == "" {
		f.Name = k.Name
	}
	if f.Model == "" {
		f.Model = k.Model
	}
	if f.Serial == "" {
		f.Serial = k.Serial
	}
	return f
}

// Status - поиск сейчас. Находки старше keep поиск не отдаёт: адреса могли смениться.
func (s *Search) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Found = slices.Clone(st.Found)
	st.Fresh = !st.Running && !s.finished.IsZero() && s.now().Sub(s.finished) <= keep
	if !st.Running && !st.Fresh {
		st.Found = nil
	}
	return st
}

// hosts - адреса узлов сети p: без адреса сети и широковещательного, если сеть больше /31.
func hosts(p netip.Prefix) []netip.Addr {
	var out []netip.Addr
	for ip := p.Addr(); p.Contains(ip); ip = ip.Next() {
		out = append(out, ip)
	}
	if p.Bits() < 31 && len(out) > 2 {
		out = out[1 : len(out)-1]
	}
	return out
}

// client - HTTP без перенаправлений и без прокси: поиск спрашивает только сам адрес.
var client = &http.Client{Timeout: probeTimeout, Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Check - настоящие проверки адреса ip: сначала открытые порты, затем у каждого открытого
// порта - проверка прибора. Проверки только спрашивают: прибору не уходят ни ключи, ни
// вход, телевизор не спрашивает разрешение на экране.
func Check(ctx context.Context, ip netip.Addr) []Found {
	open := openPorts(ctx, ip)
	host := ip.String()
	var out []Found
	if open["80"] {
		if in, err := wled.Probe(ctx, client, host); err == nil {
			out = append(out, Found{Source: config.SourceWLED, Host: host, Name: in.Name, Model: in.Arch, MAC: in.MAC})
		}
	}
	if open["8001"] {
		if in, err := samsungtv.Probe(ctx, client, host); err == nil {
			out = append(out, Found{Source: config.SourceSamsungTV, Host: host, Name: in.Name, Model: in.Model, MAC: in.MAC})
		}
	}
	if open["3001"] || open["3000"] {
		if model, err := webos.Probe(ctx, host); err == nil {
			out = append(out, Found{Source: config.SourceWebOS, Host: host, Model: model})
		}
	}
	if open["443"] {
		switch {
		case pskServer(ctx, host):
			out = append(out, Found{Source: config.SourceHomeConnectLocal, Host: host})
		case awsServer(ctx, host):
			out = append(out, Found{Source: config.SourceDeLonghi, Host: host})
		}
	}
	return out
}

// openPorts - какие порты из ports отвечают на соединение. Порты проверяются сразу все.
func openPorts(ctx context.Context, ip netip.Addr) map[string]bool {
	var mu sync.Mutex
	open := map[string]bool{}
	var wg sync.WaitGroup
	d := net.Dialer{Timeout: connectTimeout}
	for _, port := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
			if err != nil {
				return
			}
			c.Close()
			mu.Lock()
			open[port] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	return open
}

// pskServer - на порту 443 сервер TLS-PSK, как у прибора Home Connect: он выбирает набор с
// PSK и не принимает случайный ключ.
func pskServer(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, tlsTimeout)
	defer cancel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	c, err := tlspsk.Dial(ctx, "tcp4", net.JoinHostPort(host, "443"), hclocal.ProbeIdentity, key)
	if err == nil {
		c.Close()
		return false
	}
	return tlspsk.WrongKey(err)
}

// awsServer - на порту 443 сервер TLS с сертификатом AWS IoT, как у кофемашины De'Longhi.
// Сертификат не проверяется: поиску нужно только его имя.
func awsServer(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, tlsTimeout)
	defer cancel()
	// Срок соединения - только ctx: срок NetDialer действовал бы и на всё рукопожатие.
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	c, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(host, "443"))
	if err != nil {
		return false
	}
	defer c.Close()
	certs := c.(*tls.Conn).ConnectionState().PeerCertificates
	return len(certs) > 0 && certs[0].Subject.CommonName == awsCertificate
}

// Label - что за прибор найден, словами, для страницы.
func Label(source string) string {
	switch source {
	case config.SourceWLED:
		return "лента WLED"
	case config.SourceSamsungTV:
		return "телевизор Samsung"
	case config.SourceWebOS:
		return "телевизор LG webOS"
	case config.SourceHomeConnectLocal:
		return "прибор Home Connect"
	case config.SourceDeLonghi:
		return "кофемашина De'Longhi"
	}
	return source
}
