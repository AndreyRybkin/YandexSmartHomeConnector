package auth

import (
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	// MaxFailures - после стольких неудачных входов с адреса за FailWindow адрес банится на BanTime.
	MaxFailures = 4
	FailWindow  = time.Hour
	BanTime     = time.Hour
	// maxEntries - предел адресов со счётчиком или баном; при переполнении удаляется самый старый.
	maxEntries = 10000
)

// Ban - адрес в бане.
type Ban struct {
	// Prefix - адрес IPv4 или сеть IPv6 /64.
	Prefix netip.Prefix
	Until  time.Time
}

type entry struct {
	// fails - неудачные попытки за FailWindow, не больше MaxFailures.
	fails   []time.Time
	until   time.Time
	updated time.Time
}

// Bans - счётчики неудачных входов и баны. Лежат в памяти: перезапуск снимает все баны.
type Bans struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[netip.Prefix]*entry
}

// NewBans создаёт пустую таблицу банов.
func NewBans() *Bans {
	return &Bans{now: time.Now, entries: map[netip.Prefix]*entry{}}
}

// key - что банится: адрес IPv4 целиком, у IPv6 - сеть /64: у одного абонента в ней миллиарды адресов.
func key(a netip.Addr) netip.Prefix {
	a = a.Unmap().WithZone("")
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p
	}
	return netip.PrefixFrom(a, a.BitLen())
}

// Banned сообщает, забанен ли адрес.
func (b *Bans) Banned(a netip.Addr) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[key(a)]
	return e != nil && b.now().Before(e.until)
}

// Fail считает неудачную попытку. Если попытка последняя допустимая, адрес банится:
// тогда Fail возвращает забаненный адрес или сеть и true.
func (b *Bans) Fail(a netip.Addr) (netip.Prefix, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	k := key(a)
	e := b.entries[k]
	if e == nil {
		if len(b.entries) >= maxEntries {
			b.evictLocked(now)
		}
		e = &entry{}
		b.entries[k] = e
	}
	e.updated = now
	e.fails = slices.DeleteFunc(e.fails, func(t time.Time) bool { return now.Sub(t) >= FailWindow })
	e.fails = append(e.fails, now)
	if len(e.fails) < MaxFailures {
		return k, false
	}
	e.fails = nil
	e.until = now.Add(BanTime)
	return k, true
}

// Reset обнуляет счётчик адреса после успешного входа.
func (b *Bans) Reset(a netip.Addr) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := key(a)
	if e := b.entries[k]; e != nil && !b.now().Before(e.until) {
		delete(b.entries, k)
	}
}

// List - действующие баны, первым - тот, что кончится раньше.
func (b *Bans) List() []Ban {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var out []Ban
	for k, e := range b.entries {
		if now.Before(e.until) {
			out = append(out, Ban{Prefix: k, Until: e.until})
		}
	}
	slices.SortFunc(out, func(x, y Ban) int { return x.Until.Compare(y.Until) })
	return out
}

// Active - сколько адресов в бане сейчас.
func (b *Bans) Active() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	n := 0
	for _, e := range b.entries {
		if now.Before(e.until) {
			n++
		}
	}
	return n
}

// Lift снимает бан и обнуляет счётчик. false - такого бана нет.
func (b *Bans) Lift(p netip.Prefix) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[p]
	if e == nil || !b.now().Before(e.until) {
		return false
	}
	delete(b.entries, p)
	return true
}

// evictLocked освобождает место: удаляет устаревшие записи, а если их нет - самую старую.
func (b *Bans) evictLocked(now time.Time) {
	var oldest netip.Prefix
	var oldestAt time.Time
	for k, e := range b.entries {
		live := now.Before(e.until) || slices.ContainsFunc(e.fails, func(t time.Time) bool { return now.Sub(t) < FailWindow })
		if !live {
			delete(b.entries, k)
			continue
		}
		if oldestAt.IsZero() || e.updated.Before(oldestAt) {
			oldest, oldestAt = k, e.updated
		}
	}
	if len(b.entries) >= maxEntries {
		delete(b.entries, oldest)
	}
}
