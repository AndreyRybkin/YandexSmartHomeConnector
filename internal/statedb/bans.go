package statedb

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"time"
)

// Кто или что закончило бан раньше срока.
const (
	// EndedWeb - кнопка «Снять» на странице «Входы».
	EndedWeb = "web"
	// EndedCommand - команда hcgateway unban.
	EndedCommand = "command"
	// EndedEvicted - адрес вытеснен из переполненной таблицы банов.
	EndedEvicted = "evicted"
)

// Ban - строка журнала банов.
type Ban struct {
	ID     int64
	Prefix netip.Prefix
	Start  time.Time
	Until  time.Time
	// Where - откуда шли попытки: веб-интерфейс или привязка.
	Where string
	// Ended и EndedBy - когда и кем бан снят раньше срока; пусто - не снимали.
	Ended   time.Time
	EndedBy string
}

// banOp - запись в журнал банов: начало бана или его конец раньше срока.
type banOp struct {
	start bool
	ban   Ban
}

// BanStarted записывает новый бан и просит записать его сразу.
func (d *DB) BanStarted(p netip.Prefix, start, until time.Time, where string) {
	if d == nil {
		return
	}
	b := Ban{Prefix: p, Start: start, Until: until, Where: where}
	d.mu.Lock()
	d.active[p.String()] = b
	d.queueBan(banOp{start: true, ban: b})
	d.mu.Unlock()
	d.wakeUp()
}

// BanEnded записывает, что бан снят раньше срока. until отличает этот бан от других банов адреса.
func (d *DB) BanEnded(p netip.Prefix, until time.Time, by string, at time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if a, ok := d.active[p.String()]; ok && a.Until.UnixMilli() == until.UnixMilli() {
		delete(d.active, p.String())
	}
	d.queueBan(banOp{ban: Ban{Prefix: p, Until: until, Ended: at, EndedBy: by}})
	d.mu.Unlock()
	d.wakeUp()
}

// queueBan ставит запись в очередь. Пока база недоступна, очередь не растёт дальше MaxBans:
// старые записи уступают новым. Только под mu.
func (d *DB) queueBan(op banOp) {
	d.banOps = append(d.banOps, op)
	if n := len(d.banOps) - MaxBans; n > 0 {
		d.banOps = d.banOps[n:]
	}
}

// ActiveBans - действующие баны для восстановления при запуске.
func (d *DB) ActiveBans(now time.Time) []Ban {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Ban
	for _, b := range d.active {
		if now.Before(b.Until) {
			out = append(out, b)
		}
	}
	return out
}

// LiftedByCommand - ещё не истёкшие баны, которые сняла команда unban. Она работает отдельным
// процессом, поэтому читать нужно из файла. Сбой чтения - пустой список: бан остаётся до срока.
func (d *DB) LiftedByCommand(now time.Time) []Ban {
	if d == nil {
		return nil
	}
	db := d.handle()
	if db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT prefix, until FROM bans WHERE ended_by = ? AND until > ?", EndedCommand, now.UnixMilli())
	if err != nil {
		d.noteRead(err)
		return nil
	}
	defer rows.Close()
	var out []Ban
	for rows.Next() {
		var prefix string
		var until int64
		if err := rows.Scan(&prefix, &until); err != nil {
			d.noteRead(err)
			return nil
		}
		if p, err := netip.ParsePrefix(prefix); err == nil {
			out = append(out, Ban{Prefix: p, Until: time.UnixMilli(until)})
		}
	}
	if err := rows.Err(); err != nil {
		d.noteRead(err)
		return nil
	}
	d.mu.Lock()
	for _, b := range out {
		if a, ok := d.active[b.Prefix.String()]; ok && a.Until.UnixMilli() == b.Until.UnixMilli() {
			delete(d.active, b.Prefix.String())
		}
	}
	d.mu.Unlock()
	return out
}

// BanHistory - журнал банов, новые первыми: не больше limit строк с номером меньше before.
// before <= 0 - с самой новой.
func (d *DB) BanHistory(before int64, limit int) ([]Ban, error) {
	if d == nil {
		return nil, nil
	}
	db := d.handle()
	if db == nil {
		return nil, errors.New("база состояния недоступна")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	q := "SELECT id, prefix, started_at, until, place, ended_at, ended_by FROM bans"
	args := []any{limit}
	if before > 0 {
		q += " WHERE id < ?"
		args = []any{before, limit}
	}
	rows, err := db.QueryContext(ctx, q+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		d.noteRead(err)
		return nil, err
	}
	defer rows.Close()
	out, err := scanBans(rows)
	if err != nil {
		d.noteRead(err)
		return nil, err
	}
	return out, nil
}

func scanBans(rows *sql.Rows) ([]Ban, error) {
	var out []Ban
	for rows.Next() {
		var b Ban
		var prefix string
		var start, until int64
		var ended sql.NullInt64
		var by sql.NullString
		if err := rows.Scan(&b.ID, &prefix, &start, &until, &b.Where, &ended, &by); err != nil {
			return nil, err
		}
		p, err := netip.ParsePrefix(prefix)
		if err != nil {
			continue
		}
		b.Prefix, b.Start, b.Until, b.Ended, b.EndedBy = p, time.UnixMilli(start), time.UnixMilli(until), fromMillis(ended), by.String
		out = append(out, b)
	}
	return out, rows.Err()
}

// activeBans читает действующие баны, самые долгие первыми, не больше MaxBans.
func activeBans(ctx context.Context, db *sql.DB, now time.Time) ([]Ban, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, prefix, started_at, until, place, ended_at, ended_by FROM bans
		WHERE ended_at IS NULL AND until > ? ORDER BY until DESC LIMIT ?`, now.UnixMilli(), MaxBans)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBans(rows)
}

// writeBans пишет очередь журнала банов в транзакции tx.
func writeBans(ctx context.Context, tx *sql.Tx, ops []banOp) error {
	for _, op := range ops {
		b := op.ban
		var err error
		if op.start {
			_, err = tx.ExecContext(ctx, "INSERT INTO bans (prefix, started_at, until, place) VALUES (?, ?, ?, ?)",
				b.Prefix.String(), b.Start.UnixMilli(), b.Until.UnixMilli(), b.Where)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE bans SET ended_at = ?, ended_by = ? WHERE prefix = ? AND until = ? AND ended_at IS NULL",
				b.Ended.UnixMilli(), b.EndedBy, b.Prefix.String(), b.Until.UnixMilli())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pruneBans удаляет строки старше BanKeep и всё, что сверх MaxBans.
func pruneBans(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM bans WHERE started_at < ?", now.Add(-BanKeep).UnixMilli()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM bans WHERE id <= (SELECT id FROM bans ORDER BY id DESC LIMIT 1 OFFSET ?)", MaxBans)
	return err
}

// Unban снимает действующие баны прямо в файле path: так работает команда unban, а работающий шлюз
// замечает это при следующей проверке бана. all - все баны, иначе только p. Возвращает, сколько
// снято. Файла нет - банов нет.
func Unban(path string, p netip.Prefix, all bool, now time.Time) (int64, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	db, err := openFile(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	q := "UPDATE bans SET ended_at = ?, ended_by = ? WHERE ended_at IS NULL AND until > ?"
	args := []any{now.UnixMilli(), EndedCommand, now.UnixMilli()}
	if !all {
		q += " AND prefix = ?"
		args = append(args, p.String())
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
