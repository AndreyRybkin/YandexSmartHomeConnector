package statedb

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// MaxActions - сколько действий хранится на прибор.
const MaxActions = 50

// Action - действие с прибором: команда, которую шлюз получил от инициатора.
type Action struct {
	// Device - прибор; заполняется при чтении.
	Device string
	Time   time.Time
	// Initiator - откуда команда. Type и Instance - умение Алисы.
	Initiator, Type, Instance string
	// Value - значение команды в JSON. Relative - значение прибавляется к текущему.
	Value    string
	Relative bool
	// Code - код ошибки Яндекса; пусто - команда выполнена.
	Code string
}

// AddAction дописывает действие прибора; остаются последние MaxActions.
func (d *DB) AddAction(id string, a Action, value any) {
	if d == nil {
		return
	}
	raw, err := encode(value)
	if err != nil {
		raw = ""
	}
	a.Device, a.Value = "", raw
	a.Initiator, a.Type = clip(a.Initiator, maxKeyBytes), clip(a.Type, maxKeyBytes)
	a.Instance, a.Code = clip(a.Instance, maxKeyBytes), clip(a.Code, maxKeyBytes)
	d.mu.Lock()
	defer d.mu.Unlock()
	dev := d.entry(id)
	dev.actions = append(dev.actions, a)
	dev.actionsUnsaved++
	dev.trimActions()
}

func (dev *device) trimActions() {
	if n := len(dev.actions) - MaxActions; n > 0 {
		dev.actions = slices.Clone(dev.actions[n:])
	}
	dev.actionsUnsaved = min(dev.actionsUnsaved, len(dev.actions))
}

// Actions - действия прибора id, а при пустом id - всех приборов, новые первыми.
func (d *DB) Actions(id string) []Action {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Action
	add := func(id string, dev *device) {
		for _, a := range dev.actions {
			a.Device = id
			out = append(out, a)
		}
	}
	if id != "" {
		if dev := d.devices[id]; dev != nil {
			add(id, dev)
		}
	} else {
		for id, dev := range d.devices {
			add(id, dev)
		}
	}
	slices.SortStableFunc(out, func(a, b Action) int { return b.Time.Compare(a.Time) })
	return out
}

// actionRows - действия прибора для записи.
type actionRows struct {
	id      string
	rows    []Action
	rewrite bool
}

// takeActions забирает незаписанные действия прибора в запись. Только под mu.
func (dev *device) takeActions(id string, b *batch) {
	switch {
	case dev.actionsRewrite:
		b.actions = append(b.actions, actionRows{id, slices.Clone(dev.actions), true})
		dev.actionsRewrite, dev.actionsUnsaved = false, 0
	case dev.actionsUnsaved > 0:
		b.actions = append(b.actions, actionRows{id, slices.Clone(dev.actions[len(dev.actions)-dev.actionsUnsaved:]), false})
		dev.actionsUnsaved = 0
	}
}

// restoreActions возвращает действия в очередь после сбоя записи. Только под mu.
func (d *DB) restoreActions(b *batch) {
	for _, c := range b.actions {
		if dev := d.devices[c.id]; dev != nil {
			if c.rewrite {
				dev.actionsRewrite = true
			}
			dev.actionsUnsaved += len(c.rows)
			dev.trimActions()
		}
	}
}

func writeActions(ctx context.Context, tx *sql.Tx, b *batch) error {
	for _, c := range b.actions {
		if c.rewrite {
			if _, err := tx.ExecContext(ctx, "DELETE FROM device_actions WHERE device_id = ?", c.id); err != nil {
				return err
			}
		}
		for _, r := range c.rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO device_actions (device_id, at, initiator, type, instance, value, relative, code)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.id, r.Time.UnixMilli(), r.Initiator, r.Type, r.Instance, r.Value, r.Relative, r.Code); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_actions WHERE device_id = ?1 AND id <=
			(SELECT id FROM device_actions WHERE device_id = ?1 ORDER BY id DESC LIMIT 1 OFFSET ?2)`, c.id, MaxActions); err != nil {
			return err
		}
	}
	return nil
}

func loadActions(ctx context.Context, db *sql.DB, entry func(id string) *device) error {
	rows, err := db.QueryContext(ctx, "SELECT device_id, at, initiator, type, instance, value, relative, code FROM device_actions ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at int64
		var a Action
		if err := rows.Scan(&id, &at, &a.Initiator, &a.Type, &a.Instance, &a.Value, &a.Relative, &a.Code); err != nil {
			return err
		}
		a.Time = time.UnixMilli(at)
		dev := entry(id)
		dev.actions = append(dev.actions, a)
		dev.trimActions()
	}
	return rows.Err()
}
