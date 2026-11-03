package auth

import (
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"hcgateway/internal/statedb"
)

const (
	// MaxFailures - после стольких неудачных входов с адреса за FailWindow адрес банится на BanTime.
	MaxFailures = 4
	FailWindow  = time.Hour
	BanTime     = time.Hour
	// maxEntries - предел адресов со счётчиком или баном; при переполнении удаляется самый старый.
	maxEntries = 10000
	// refreshEvery - не чаще чем через сколько таблица сверяется с баном, снятым командой unban.
	refreshEvery = time.Second
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

// Bans - счётчики неудачных входов и баны. Баны лежат ещё и в базе состояния и переживают
// перезапуск. Счётчики - только в памяти: иначе шлюз писал бы на диск при каждой попытке.
type Bans struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[netip.Prefix]*entry
	// db - журнал банов; nil - только память.
	db *statedb.DB
	// refreshed - последняя сверка с базой.
	refreshed time.Time
}

// NewBans создаёт пустую таблицу банов.
func NewBans() *Bans {
	return &Bans{now: time.Now, entries: map[netip.Prefix]*entry{}}
}

// Attach подключает журнал банов и восстанавливает действующие баны.
func (b *Bans) Attach(db *statedb.DB) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.db = db
	now := b.now()
	for _, s := range db.ActiveBans(now) {
		b.entries[s.Prefix] = &entry{until: s.Until, updated: now}
	}
}

// BanTarget разбирает, какой бан снимать: адрес или сеть. Адрес IPv6 значит его сеть /64.
func BanTarget(s string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		return key(a), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q: нужен адрес или сеть", s)
	}
	return p.Masked(), nil
}

// refresh снимает в памяти баны, которые сняла команда unban: она работает отдельным процессом.
func (b *Bans) refresh() {
	b.mu.Lock()
	now := b.now()
	db := b.db
	if db == nil || now.Sub(b.refreshed) < refreshEvery {
		b.mu.Unlock()
		return
	}
	b.refreshed = now
	b.mu.Unlock()
	lifted := db.LiftedByCommand(now)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range lifted {
		// Сравнение сроков: адрес могли забанить снова уже после команды.
		if e := b.entries[l.Prefix]; e != nil && e.until.UnixMilli() == l.Until.UnixMilli() {
			delete(b.entries, l.Prefix)
		}
	}
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

// Banned сообщает, забанен ли адрес. Перед ответом «да» сверяется с базой: команда unban действует
// на следующую же попытку входа.
func (b *Bans) Banned(a netip.Addr) bool {
	if !b.bannedNow(a) {
		return false
	}
	b.refresh()
	return b.bannedNow(a)
}

func (b *Bans) bannedNow(a netip.Addr) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[key(a)]
	return e != nil && b.now().Before(e.until)
}

// Fail считает неудачную попытку. Если попытка последняя допустимая, адрес банится:
// тогда Fail возвращает забаненный адрес или сеть и true. where - откуда попытка, для журнала банов.
func (b *Bans) Fail(a netip.Addr, where string) (netip.Prefix, bool) {
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
	b.db.BanStarted(k, now, e.until, where)
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
	b.refresh()
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
	b.refresh()
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
	now := b.now()
	if e == nil || !now.Before(e.until) {
		return false
	}
	delete(b.entries, p)
	b.db.BanEnded(p, e.until, statedb.EndedWeb, now)
	return true
}

// History - журнал банов, новые первыми: не больше limit строк с номером меньше before; before <= 0 -
// с самой новой. Без журнала - пусто.
func (b *Bans) History(before int64, limit int) ([]statedb.Ban, error) {
	b.mu.Lock()
	db := b.db
	b.mu.Unlock()
	return db.BanHistory(before, limit)
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
		if e := b.entries[oldest]; now.Before(e.until) {
			b.db.BanEnded(oldest, e.until, statedb.EndedEvicted, now)
		}
		delete(b.entries, oldest)
	}
}
