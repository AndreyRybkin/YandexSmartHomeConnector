package statedb

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"time"
)

const (
	// MaxRetained и MaxRetainedBytes - сколько сохранённых сообщений брокера хранит база и какого
	// размера: без предела клиент с паролем заполнил бы диск.
	MaxRetained      = 1000
	MaxRetainedBytes = 16 << 10
)

// Retained - сохранённое сообщение брокера MQTT.
type Retained struct {
	Topic   string
	Payload []byte
	QoS     byte
	Time    time.Time
}

// retained - сообщение в памяти; dirty - ещё не записано.
type retained struct {
	payload []byte
	qos     byte
	at      time.Time
	dirty   bool
}

// Retained - сохранённые сообщения по топикам.
func (d *DB) Retained() []Retained {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Retained, 0, len(d.retained))
	for _, t := range slices.Sorted(maps.Keys(d.retained)) {
		r := d.retained[t]
		out = append(out, Retained{Topic: t, Payload: slices.Clone(r.payload), QoS: r.qos, Time: r.at})
	}
	return out
}

// RetainedCount - сколько сохранённых сообщений в базе.
func (d *DB) RetainedCount() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.retained)
}

// SetRetained запоминает сообщение. false - сообщение за пределами и не сохранено; прежнее значение
// топика при этом стирается, иначе после перезапуска оно вернулось бы как новое.
func (d *DB) SetRetained(topic string, payload []byte, qos byte, now time.Time) bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, known := d.retained[topic]
	if len(topic) > maxKeyBytes || len(payload) > MaxRetainedBytes || (!known && len(d.retained) >= MaxRetained) {
		if known {
			delete(d.retained, topic)
			d.retainedGone[topic] = true
		}
		return false
	}
	d.retained[topic] = &retained{payload: slices.Clone(payload), qos: qos, at: now, dirty: true}
	delete(d.retainedGone, topic)
	return true
}

// DeleteRetained стирает сообщение топика.
func (d *DB) DeleteRetained(topic string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.retained[topic]; ok {
		delete(d.retained, topic)
		d.retainedGone[topic] = true
	}
}

// retainedRow - сообщение к записи.
type retainedRow struct {
	topic   string
	payload []byte
	qos     byte
	at      time.Time
}

// takeRetained забирает изменения сообщений в запись. Только под mu.
func (d *DB) takeRetained(b *batch) {
	for t, r := range d.retained {
		if r.dirty {
			b.retained = append(b.retained, retainedRow{t, r.payload, r.qos, r.at})
			r.dirty = false
		}
	}
	for t := range d.retainedGone {
		b.retainedGone = append(b.retainedGone, t)
	}
	d.retainedGone = map[string]bool{}
}

// restoreRetained возвращает несохранённое после сбоя записи. Только под mu.
func (d *DB) restoreRetained(b *batch) {
	for _, r := range b.retained {
		if e := d.retained[r.topic]; e != nil {
			e.dirty = true
		}
	}
	for _, t := range b.retainedGone {
		if d.retained[t] == nil {
			d.retainedGone[t] = true
		}
	}
}

// writeRetained пишет сообщения в транзакции tx.
func writeRetained(ctx context.Context, tx *sql.Tx, b *batch) error {
	for _, t := range b.retainedGone {
		if _, err := tx.ExecContext(ctx, "DELETE FROM mqtt_retained WHERE topic = ?", t); err != nil {
			return err
		}
	}
	for _, r := range b.retained {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mqtt_retained (topic, payload, qos, at) VALUES (?, ?, ?, ?)
			ON CONFLICT (topic) DO UPDATE SET payload = excluded.payload, qos = excluded.qos, at = excluded.at`,
			r.topic, r.payload, r.qos, r.at.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// loadRetained читает сохранённые сообщения при открытии базы.
func loadRetained(ctx context.Context, db *sql.DB) (map[string]*retained, error) {
	rows, err := db.QueryContext(ctx, "SELECT topic, payload, qos, at FROM mqtt_retained ORDER BY at DESC LIMIT ?", MaxRetained)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*retained{}
	for rows.Next() {
		var topic string
		var r retained
		var at int64
		if err := rows.Scan(&topic, &r.payload, &r.qos, &at); err != nil {
			return nil, err
		}
		r.at = time.UnixMilli(at)
		out[topic] = &r
	}
	return out, rows.Err()
}
