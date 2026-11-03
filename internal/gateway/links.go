package gateway

import (
	"context"
	"maps"
	"sync"
	"time"

	"hcgateway/internal/hcdevice"
	"hcgateway/internal/metrics"
	"hcgateway/internal/statedb"
	"hcgateway/internal/wled"
)

// linkCheck - как часто шлюз проверяет связь с приборами: так он знает, с какого времени
// прибор не на связи, даже если страницу приборов никто не открывает.
const linkCheck = 5 * time.Second

// linkStartup - прибор, которого шлюз впервые проверил так скоро после запуска и с тех пор не
// видел на связи, не на связи с запуска шлюза.
const linkStartup = time.Minute

// links - с какого времени приборы не на связи. Отсчёт переживает перезапуск шлюза, если шлюз
// подключён к базе состояния.
type links struct {
	mu sync.Mutex
	// start - запуск шлюза, checked - последняя проверка.
	start, checked time.Time
	// first - первая проверка прибора; off - с какого времени он не на связи; seen - прибор
	// был на связи после первой проверки.
	first, off map[string]time.Time
	seen       map[string]bool
	// wasOn - прибор был на связи при остановке шлюза; alive - последняя отметка работы шлюза. Если
	// прибора нет на первой проверке, он пропал с этой отметки: точнее шлюз не знает.
	wasOn map[string]bool
	alive time.Time
}

func newLinks(now time.Time) links {
	return links{start: now, first: map[string]time.Time{}, off: map[string]time.Time{}, seen: map[string]bool{},
		wasOn: map[string]bool{}}
}

// restore задаёт связь приборов из базы состояния.
func (l *links) restore(saved map[string]statedb.Link, alive time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.alive = alive
	for id, s := range saved {
		l.first[id] = s.First
		if s.Seen {
			l.seen[id] = true
		}
		if s.Online {
			l.wasOn[id] = true
		} else {
			l.off[id] = s.Off
		}
	}
}

// saved - связь приборов для базы состояния.
func (l *links) saved() map[string]statedb.Link {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]statedb.Link, len(l.first))
	for id, first := range l.first {
		off := l.off[id]
		out[id] = statedb.Link{First: first, Online: off.IsZero(), Off: off, Seen: l.seen[id]}
	}
	return out
}

// update запоминает связь приборов на момент now. Прибор, которого нет в status и в настройках,
// забывается: прибор облака, который ждёт данных, в status не попадает, но своё время не теряет.
func (l *links) update(now time.Time, status []metrics.Device, configured func(id string) bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checked = now
	present := make(map[string]bool, len(status))
	for _, d := range status {
		present[d.ID] = true
		if _, ok := l.first[d.ID]; !ok {
			l.first[d.ID] = now
		}
		switch {
		case d.Online:
			l.seen[d.ID] = true
			delete(l.off, d.ID)
		case l.off[d.ID].IsZero():
			l.off[d.ID] = now
			if l.wasOn[d.ID] && !l.alive.IsZero() {
				l.off[d.ID] = l.alive
			}
		}
		delete(l.wasOn, d.ID)
	}
	for id := range l.first {
		if !present[id] && !configured(id) {
			delete(l.first, id)
			delete(l.off, id)
			delete(l.seen, id)
			delete(l.wasOn, id)
		}
	}
}

// stale - связь не проверяли дольше linkCheck.
func (l *links) stale(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return now.Sub(l.checked) >= linkCheck
}

// offline - с какого времени прибор id не на связи (Gateway.Offline).
func (l *links) offline(id string) (since time.Time, fromStart, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if since, ok = l.off[id]; !ok {
		return time.Time{}, false, false
	}
	// Первая проверка раньше запуска - прибор из базы: шлюз знает, с какого времени нет связи.
	sinceStart := l.first[id].Sub(l.start)
	return since, !l.seen[id] && sinceStart >= 0 && sinceStart < linkStartup, true
}

// clock - текущее время; тесты подменяют часы.
func (g *Gateway) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// watchLinks проверяет связь с приборами раз в linkCheck, пока ctx не отменён.
func (g *Gateway) watchLinks(ctx context.Context) {
	t := time.NewTicker(linkCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.checkLinks(g.clock())
		}
	}
}

// checkLinks проверяет связь приборов и передаёт базе состояния связь и значения.
func (g *Gateway) checkLinks(now time.Time) {
	g.links.update(now, g.Status(), g.configured)
	if g.state == nil {
		return
	}
	for id, l := range g.links.saved() {
		g.state.SetLink(id, l)
	}
	g.mu.Lock()
	list := make(map[string]*built, len(g.devices))
	maps.Copy(list, g.devices)
	g.mu.Unlock()
	// Приборы опрашиваются без блокировки, как в Status.
	for id, b := range list {
		if values, ok := liveValues(b); ok && len(values) > 0 {
			g.state.SetValues(id, values, now)
		}
	}
	if g.cfg.Load() != nil {
		g.state.Retain(g.configured)
	}
}

// configured сообщает, что прибор есть в применённых настройках. До первого Apply забывать нечего.
func (g *Gateway) configured(id string) bool {
	cfg := g.cfg.Load()
	if cfg == nil {
		return true
	}
	_, ok := cfg.Devices[id]
	return ok
}

// liveValues - значения прибора на связи. Прибор без связи ничего не сообщает: значения в базе
// остаются последними полученными.
func liveValues(b *built) (map[string]any, bool) {
	switch dev := b.dev.(type) {
	case *hcdevice.Device:
		st := dev.Appliance.State()
		if !st.Online {
			return nil, false
		}
		values := st.Values
		if values == nil {
			values = map[string]any{}
		}
		if st.ActiveProgram != "" {
			values[ValueProgram] = st.ActiveProgram
		}
		if st.SelectedProgram != "" {
			values[ValueSelected] = st.SelectedProgram
		}
		return values, true
	case *wled.Device:
		return dev.Values()
	}
	return nil, false
}

// LastValues - последние полученные значения прибора. Они нужны только для показа: Алиса и команды
// работают с состоянием источника.
func (g *Gateway) LastValues(id string) []statedb.Value {
	return g.state.Values(id)
}

// StateFailing сообщает, что база состояния не принимает запись.
func (g *Gateway) StateFailing() bool {
	return g.state.Failing()
}

// Offline - с какого времени прибор id не на связи; ok - прибор не на связи. fromStart -
// прибор не был на связи с запуска шлюза: когда он пропал, шлюз не знает. Если связь давно
// не проверяли, Offline проверяет её сам.
func (g *Gateway) Offline(id string) (since time.Time, fromStart, ok bool) {
	if now := g.clock(); g.links.stale(now) {
		g.links.update(now, g.Status(), g.configured)
	}
	return g.links.offline(id)
}
