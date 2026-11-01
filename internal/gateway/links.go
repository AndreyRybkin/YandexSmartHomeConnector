package gateway

import (
	"context"
	"sync"
	"time"

	"hcgateway/internal/metrics"
)

// linkCheck - как часто шлюз проверяет связь с приборами: так он знает, с какого времени
// прибор не на связи, даже если страницу приборов никто не открывает.
const linkCheck = 5 * time.Second

// linkStartup - прибор, которого шлюз впервые проверил так скоро после запуска и с тех пор не
// видел на связи, не на связи с запуска шлюза.
const linkStartup = time.Minute

// links - с какого времени приборы не на связи. Всё - в памяти: после перезапуска шлюза
// отсчёт начинается заново.
type links struct {
	mu sync.Mutex
	// start - запуск шлюза, checked - последняя проверка.
	start, checked time.Time
	// first - первая проверка прибора; off - с какого времени он не на связи; seen - прибор
	// был на связи после первой проверки.
	first, off map[string]time.Time
	seen       map[string]bool
}

func newLinks(now time.Time) links {
	return links{start: now, first: map[string]time.Time{}, off: map[string]time.Time{}, seen: map[string]bool{}}
}

// update запоминает связь приборов на момент now. Прибор, которого нет в status, забывается.
func (l *links) update(now time.Time, status []metrics.Device) {
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
		}
	}
	for id := range l.first {
		if !present[id] {
			delete(l.first, id)
			delete(l.off, id)
			delete(l.seen, id)
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
	return since, !l.seen[id] && l.first[id].Sub(l.start) < linkStartup, true
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
			g.links.update(g.clock(), g.Status())
		}
	}
}

// Offline - с какого времени прибор id не на связи; ok - прибор не на связи. fromStart -
// прибор не был на связи с запуска шлюза: когда он пропал, шлюз не знает. Если связь давно
// не проверяли, Offline проверяет её сам.
func (g *Gateway) Offline(id string) (since time.Time, fromStart, ok bool) {
	if now := g.clock(); g.links.stale(now) {
		g.links.update(now, g.Status())
	}
	return g.links.offline(id)
}
