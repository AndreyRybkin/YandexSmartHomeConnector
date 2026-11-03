package statedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"time"
)

// GapKind - вид изменения «шлюз не работал».
const GapKind = "gateway"

// Link - связь прибора.
type Link struct {
	// First - первая проверка прибора.
	First time.Time
	// Online - прибор был на связи при последней проверке.
	Online bool
	// Off - с какого времени прибор не на связи; у прибора на связи - пусто.
	Off time.Time
	// Seen - прибор хоть раз был на связи.
	Seen bool
}

// Value - последнее значение имени.
type Value struct {
	Key string
	// Value - значение прибора: bool, число, строка или составное значение.
	Value any
	// Gone - когда прибор перестал его сообщать; пусто - сообщал при последней проверке.
	Gone time.Time
}

// Change - изменение прибора.
type Change struct {
	Time             time.Time
	Kind, Key, Value string
}

// value - значение в памяти: raw - JSON, dirty - ещё не записано.
type value struct {
	raw   string
	gone  time.Time
	dirty bool
}

// device - всё сохранённое о приборе.
type device struct {
	link               Link
	hasLink, linkDirty bool
	values             map[string]*value
	// removed - значения, удалённые из памяти, но ещё лежащие в базе.
	removed map[string]bool
	// full - о пределе значений уже написано в журнал.
	full bool
	// changes - последние изменения, старые первыми. unsaved - сколько последних ещё не записано;
	// rewrite - строки прибора в базе заменяются целиком.
	changes []Change
	unsaved int
	rewrite bool
}

func newDevice() *device {
	return &device{values: map[string]*value{}, removed: map[string]bool{}}
}

// entry - прибор id в памяти; нет - создаётся. Только под mu.
func (d *DB) entry(id string) *device {
	dev := d.devices[id]
	if dev == nil {
		dev = newDevice()
		d.devices[id] = dev
	}
	return dev
}

// Alive - когда шлюз в прошлый раз отметил, что работает; пусто - отметки нет.
func (d *DB) Alive() time.Time {
	if d == nil {
		return time.Time{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.alive
}

// Links - сохранённая связь приборов.
func (d *DB) Links() map[string]Link {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]Link{}
	for id, dev := range d.devices {
		if dev.hasLink {
			out[id] = dev.link
		}
	}
	return out
}

// SetLink запоминает связь прибора.
func (d *DB) SetLink(id string, l Link) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dev := d.entry(id)
	if dev.hasLink && sameLink(dev.link, l) {
		return
	}
	dev.link, dev.hasLink, dev.linkDirty = l, true, true
}

func sameLink(a, b Link) bool {
	return a.First.Equal(b.First) && a.Online == b.Online && a.Off.Equal(b.Off) && a.Seen == b.Seen
}

// SetValues запоминает значения прибора на связи. Новое значение заменяет только своё имя: имя,
// которого больше нет в values, остаётся с временем now. Замена целиком не подходит: выключенный
// телевизор на связи и сообщает только питание.
func (d *DB) SetValues(id string, values map[string]any, now time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dev := d.entry(id)
	for k, v := range values {
		if len(k) > maxKeyBytes {
			continue
		}
		raw, err := encode(v)
		if err != nil {
			continue
		}
		e := dev.values[k]
		if e == nil {
			if len(dev.values) >= MaxNames && !dev.evict() {
				if !dev.full {
					dev.full = true
					d.log.Warn("база состояния: у прибора слишком много значений, новые не хранятся", "device", id, "max", MaxNames)
				}
				continue
			}
			dev.values[k] = &value{raw: raw, dirty: true}
			delete(dev.removed, k)
			continue
		}
		if e.raw != raw || !e.gone.IsZero() {
			e.raw, e.gone, e.dirty = raw, time.Time{}, true
		}
	}
	for k, e := range dev.values {
		if _, ok := values[k]; !ok && e.gone.IsZero() {
			e.gone, e.dirty = now, true
		}
	}
}

// evict освобождает место: удаляет самое давнее значение, которое прибор больше не сообщает.
func (dev *device) evict() bool {
	var oldest string
	var at time.Time
	for k, e := range dev.values {
		if !e.gone.IsZero() && (at.IsZero() || e.gone.Before(at)) {
			oldest, at = k, e.gone
		}
	}
	if at.IsZero() {
		return false
	}
	delete(dev.values, oldest)
	dev.removed[oldest] = true
	return true
}

// Values - последние значения прибора по имени.
func (d *DB) Values(id string) []Value {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dev := d.devices[id]
	if dev == nil {
		return nil
	}
	out := make([]Value, 0, len(dev.values))
	for _, k := range slices.Sorted(maps.Keys(dev.values)) {
		e := dev.values[k]
		var v any
		if json.Unmarshal([]byte(e.raw), &v) != nil {
			continue
		}
		out = append(out, Value{Key: k, Value: v, Gone: e.gone})
	}
	return out
}

// AddChanges дописывает изменения прибора; остаются последние MaxChanges.
func (d *DB) AddChanges(id string, changes []Change) {
	if d == nil || len(changes) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dev := d.entry(id)
	for _, c := range changes {
		c.Key, c.Value = clip(c.Key, MaxValueBytes), clip(c.Value, MaxValueBytes)
		dev.changes = append(dev.changes, c)
	}
	dev.unsaved += len(changes)
	dev.trim()
}

func (dev *device) trim() {
	if n := len(dev.changes) - MaxChanges; n > 0 {
		dev.changes = slices.Clone(dev.changes[n:])
	}
	dev.unsaved = min(dev.unsaved, len(dev.changes))
}

// Changes - последние изменения прибора, старые первыми.
func (d *DB) Changes(id string) []Change {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if dev := d.devices[id]; dev != nil {
		return slices.Clone(dev.changes)
	}
	return nil
}

// MarkGap добавляет отметку простоя шлюза приборам, у которых есть история. Отметки подряд
// схлопываются в последнюю: иначе частые перезапуски вытеснили бы настоящие изменения.
func (d *DB) MarkGap(c Change) {
	if d == nil {
		return
	}
	c.Kind = GapKind
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, dev := range d.devices {
		switch n := len(dev.changes); {
		case n == 0:
		case dev.changes[n-1].Kind == GapKind:
			dev.changes[n-1] = c
			dev.rewrite = true
		default:
			dev.changes = append(dev.changes, c)
			dev.unsaved++
			dev.trim()
		}
	}
}

// Retain забывает приборы, которых нет в настройках.
func (d *DB) Retain(keep func(id string) bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.devices {
		if !keep(id) {
			delete(d.devices, id)
			d.dropped[id] = true
		}
	}
}

// markAll помечает всё из памяти к записи: база новая и пустая. Только под mu.
func (d *DB) markAll() {
	d.dropped = map[string]bool{}
	for _, dev := range d.devices {
		dev.linkDirty = dev.hasLink
		dev.removed = map[string]bool{}
		for _, e := range dev.values {
			e.dirty = true
		}
		dev.unsaved, dev.rewrite = 0, len(dev.changes) > 0
	}
	d.banOps = nil
	for _, b := range d.active {
		d.banOps = append(d.banOps, banOp{start: true, ban: b})
	}
}

// batch - одна запись на диск.
type batch struct {
	now     time.Time
	dropped []string
	links   []linkRow
	values  []valueRow
	removed []valueRow
	changes []changeRows
	bans    []banOp
	alive   bool
	prune   bool
}

type linkRow struct {
	id   string
	link Link
}

type valueRow struct {
	id, key, raw string
	gone         time.Time
}

type changeRows struct {
	id      string
	rows    []Change
	rewrite bool
}

func (b *batch) empty() bool {
	return len(b.dropped) == 0 && len(b.links) == 0 && len(b.values) == 0 && len(b.removed) == 0 &&
		len(b.changes) == 0 && len(b.bans) == 0 && !b.alive && !b.prune
}

// take забирает изменения в запись. Только под mu.
func (d *DB) take(now time.Time, alive, prune bool) *batch {
	b := &batch{now: now, alive: alive, prune: prune, bans: d.banOps}
	d.banOps = nil
	for id := range d.dropped {
		b.dropped = append(b.dropped, id)
	}
	d.dropped = map[string]bool{}
	for k, ban := range d.active {
		if !now.Before(ban.Until) {
			delete(d.active, k)
		}
	}
	for id, dev := range d.devices {
		if prune {
			for k, e := range dev.values {
				if !e.gone.IsZero() && now.Sub(e.gone) >= ValueKeep {
					delete(dev.values, k)
					dev.removed[k] = true
				}
			}
		}
		if dev.linkDirty {
			b.links = append(b.links, linkRow{id, dev.link})
			dev.linkDirty = false
		}
		for k, e := range dev.values {
			if e.dirty {
				b.values = append(b.values, valueRow{id: id, key: k, raw: e.raw, gone: e.gone})
				e.dirty = false
			}
		}
		for k := range dev.removed {
			b.removed = append(b.removed, valueRow{id: id, key: k})
		}
		dev.removed = map[string]bool{}
		switch {
		case dev.rewrite:
			b.changes = append(b.changes, changeRows{id, slices.Clone(dev.changes), true})
			dev.rewrite, dev.unsaved = false, 0
		case dev.unsaved > 0:
			b.changes = append(b.changes, changeRows{id, slices.Clone(dev.changes[len(dev.changes)-dev.unsaved:]), false})
			dev.unsaved = 0
		}
	}
	return b
}

// restore возвращает несохранённое в очередь после сбоя записи. Только под mu.
func (d *DB) restore(b *batch) {
	d.banOps = append(b.bans, d.banOps...)
	if n := len(d.banOps) - MaxBans; n > 0 {
		d.banOps = d.banOps[n:]
	}
	for _, id := range b.dropped {
		if d.devices[id] == nil {
			d.dropped[id] = true
		}
	}
	for _, r := range b.links {
		if dev := d.devices[r.id]; dev != nil {
			dev.linkDirty = true
		}
	}
	for _, r := range b.values {
		if dev := d.devices[r.id]; dev != nil {
			if e := dev.values[r.key]; e != nil {
				e.dirty = true
			}
		}
	}
	for _, r := range b.removed {
		if dev := d.devices[r.id]; dev != nil && dev.values[r.key] == nil {
			dev.removed[r.key] = true
		}
	}
	for _, c := range b.changes {
		if dev := d.devices[c.id]; dev != nil {
			if c.rewrite {
				dev.rewrite = true
			}
			dev.unsaved += len(c.rows)
			dev.trim()
		}
	}
}

// write записывает изменения одной транзакцией.
func (b *batch) write(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range b.dropped {
		for _, q := range []string{"DELETE FROM devices WHERE id = ?", "DELETE FROM device_values WHERE device_id = ?",
			"DELETE FROM device_changes WHERE device_id = ?"} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
	}
	for _, r := range b.links {
		l := r.link
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices (id, first_at, online, off_at, seen) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET first_at = excluded.first_at, online = excluded.online, off_at = excluded.off_at, seen = excluded.seen`,
			r.id, l.First.UnixMilli(), l.Online, millis(l.Off), l.Seen); err != nil {
			return err
		}
	}
	for _, r := range b.values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_values (device_id, key, value, gone_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (device_id, key) DO UPDATE SET value = excluded.value, gone_at = excluded.gone_at`,
			r.id, r.key, r.raw, millis(r.gone)); err != nil {
			return err
		}
	}
	for _, r := range b.removed {
		if _, err := tx.ExecContext(ctx, "DELETE FROM device_values WHERE device_id = ? AND key = ?", r.id, r.key); err != nil {
			return err
		}
	}
	for _, c := range b.changes {
		if c.rewrite {
			if _, err := tx.ExecContext(ctx, "DELETE FROM device_changes WHERE device_id = ?", c.id); err != nil {
				return err
			}
		}
		for _, r := range c.rows {
			if _, err := tx.ExecContext(ctx, "INSERT INTO device_changes (device_id, at, kind, key, value) VALUES (?, ?, ?, ?, ?)",
				c.id, r.Time.UnixMilli(), r.Kind, r.Key, r.Value); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_changes WHERE device_id = ?1 AND id <=
			(SELECT id FROM device_changes WHERE device_id = ?1 ORDER BY id DESC LIMIT 1 OFFSET ?2)`, c.id, MaxChanges); err != nil {
			return err
		}
	}
	if err := writeBans(ctx, tx, b.bans); err != nil {
		return err
	}
	if b.alive {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('alive_at', ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, b.now.UnixMilli()); err != nil {
			return err
		}
	}
	if b.prune {
		if err := pruneBans(ctx, tx, b.now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// loaded - что лежит в базе.
type loaded struct {
	alive   time.Time
	devices map[string]*device
	active  []Ban
}

// load читает сохранённое при открытии базы.
func load(db *sql.DB, now time.Time) (*loaded, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l := &loaded{devices: map[string]*device{}}
	entry := func(id string) *device {
		if l.devices[id] == nil {
			l.devices[id] = newDevice()
		}
		return l.devices[id]
	}
	var alive sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'alive_at'").Scan(&alive); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	l.alive = fromMillis(alive)

	rows, err := db.QueryContext(ctx, "SELECT id, first_at, online, off_at, seen FROM devices")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var first int64
		var off sql.NullInt64
		var link Link
		if err := rows.Scan(&id, &first, &link.Online, &off, &link.Seen); err != nil {
			return nil, err
		}
		link.First, link.Off = time.UnixMilli(first), fromMillis(off)
		dev := entry(id)
		dev.link, dev.hasLink = link, true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if rows, err = db.QueryContext(ctx, "SELECT device_id, key, value, gone_at FROM device_values"); err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, raw string
		var gone sql.NullInt64
		if err := rows.Scan(&id, &key, &raw, &gone); err != nil {
			return nil, err
		}
		entry(id).values[key] = &value{raw: raw, gone: fromMillis(gone)}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if rows, err = db.QueryContext(ctx, "SELECT device_id, at, kind, key, value FROM device_changes ORDER BY id"); err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at int64
		var c Change
		if err := rows.Scan(&id, &at, &c.Kind, &c.Key, &c.Value); err != nil {
			return nil, err
		}
		c.Time = time.UnixMilli(at)
		dev := entry(id)
		dev.changes = append(dev.changes, c)
		dev.trim()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if l.active, err = activeBans(ctx, db, now); err != nil {
		return nil, err
	}
	return l, nil
}

// merge добавляет прочитанное в память. Приборы и баны, которые шлюз уже знает, новее базы:
// база открылась не с первого раза. Только под mu.
func (d *DB) merge(l *loaded) {
	d.alive = l.alive
	for id, dev := range l.devices {
		if d.devices[id] == nil {
			d.devices[id] = dev
		}
	}
	for _, b := range l.active {
		if _, ok := d.active[b.Prefix.String()]; !ok {
			d.active[b.Prefix.String()] = b
		}
	}
}
