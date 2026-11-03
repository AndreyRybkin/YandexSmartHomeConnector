// Package statedb - база состояния шлюза в файле SQLite: баны, связь приборов, последние значения,
// история изменений и сохранённые сообщения брокера MQTT. Вызовы шлюза меняют только копию в памяти, а отдельная горутина пишет
// изменившееся на диск: сбой базы не задерживает вход и команды Алисы.
// Сломанную базу шлюз пересоздаёт сам и записывает в неё то, что лежит в памяти.
// Все методы можно вызывать у nil: тогда шлюз работает без базы.
package statedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"hcgateway/internal/metrics"
)

const (
	// FileName - файл базы в data_dir.
	FileName = "state.db"
	// brokenSuffix - окончание отложенного файла сломанной базы; такой файл один.
	brokenSuffix = ".broken"

	// flushEvery - как часто изменения уходят на диск. Баны пишутся сразу.
	flushEvery = 5 * time.Second
	// aliveEvery - как часто шлюз отмечает, что работает, и проверяет, что файл базы на месте.
	aliveEvery = time.Minute
	// pruneEvery - как часто удаляются устаревшие записи.
	pruneEvery = time.Hour
	// opTimeout и readTimeout - пределы записи на диск и чтения на пути запроса.
	opTimeout   = 5 * time.Second
	readTimeout = time.Second

	// MaxBans и BanKeep - сколько строк и сколько времени хранится журнал банов: без предела
	// подбор пароля с разных адресов заполнил бы диск.
	MaxBans = 10000
	BanKeep = 90 * 24 * time.Hour
	// MaxNames - сколько значений хранится на прибор. ValueKeep - сколько хранится значение,
	// которое прибор перестал сообщать.
	MaxNames  = 2000
	ValueKeep = 90 * 24 * time.Hour
	// MaxValueBytes - предел значения; длиннее обрезается. maxKeyBytes - предел имени; значение с
	// более длинным именем не хранится.
	MaxValueBytes = 1024
	maxKeyBytes   = 256
	// MaxChanges - сколько изменений хранится на прибор.
	MaxChanges = 50
)

// errBroken - база есть, но шлюз не может ей пользоваться: проверка нашла повреждение или схема
// новее кода, например после отката образа.
var errBroken = errors.New("база состояния повреждена или новее кода")

// migrations - схема базы: номер версии - число применённых строк, он лежит в user_version.
var migrations = []string{`
CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
CREATE TABLE devices (
	id TEXT PRIMARY KEY,
	first_at INTEGER NOT NULL,
	online INTEGER NOT NULL,
	off_at INTEGER,
	seen INTEGER NOT NULL
);
CREATE TABLE device_values (
	device_id TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	gone_at INTEGER,
	PRIMARY KEY (device_id, key)
);
CREATE TABLE device_changes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id TEXT NOT NULL,
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL
);
CREATE INDEX device_changes_device ON device_changes (device_id, id);
CREATE TABLE bans (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	prefix TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	until INTEGER NOT NULL,
	place TEXT NOT NULL,
	ended_at INTEGER,
	ended_by TEXT
);
CREATE INDEX bans_until ON bans (until);
`, `
CREATE TABLE mqtt_retained (
	topic TEXT PRIMARY KEY,
	payload BLOB NOT NULL,
	qos INTEGER NOT NULL,
	at INTEGER NOT NULL
);
`}

// DB - база состояния и её копия в памяти.
type DB struct {
	path string
	log  *slog.Logger
	now  func() time.Time
	wake chan struct{}

	// writeMu - запись на диск идёт по одной; lastAlive, lastCheck, lastPrune и file - только под ним.
	writeMu                         sync.Mutex
	lastAlive, lastCheck, lastPrune time.Time
	// file - файл, который открыт: по нему шлюз замечает, что файл удалили или заменили.
	file fs.FileInfo

	mu sync.Mutex
	// db - nil, пока базу не удалось открыть.
	db     *sql.DB
	closed bool
	// broken - чтение нашло повреждение: база пересоздаётся при следующей записи.
	broken bool
	// failing и lastErr - последняя запись не удалась; одна и та же ошибка пишется в журнал один раз.
	failing bool
	lastErr string
	// alive - отметка работы шлюза из базы при запуске.
	alive   time.Time
	devices map[string]*device
	// dropped - удалённые приборы, чьи записи ещё лежат в базе.
	dropped map[string]bool
	active  map[string]Ban
	banOps  []banOp
	// retained - сохранённые сообщения брокера MQTT; retainedGone - стёртые, но ещё лежащие в базе.
	retained     map[string]*retained
	retainedGone map[string]bool
}

// Open открывает базу path: при запуске шлюз читает из неё сохранённое. Ошибка не мешает шлюзу:
// он работает на памяти и повторяет попытку при каждой записи.
func Open(path string, log *slog.Logger) *DB {
	return open(path, log, time.Now)
}

func open(path string, log *slog.Logger, now func() time.Time) *DB {
	d := &DB{path: path, log: log, now: now, wake: make(chan struct{}, 1),
		devices: map[string]*device{}, dropped: map[string]bool{}, active: map[string]Ban{},
		retained: map[string]*retained{}, retainedGone: map[string]bool{}}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if err := d.connect(); err != nil {
		d.fail(err)
	}
	return d
}

// Run пишет изменения на диск, пока ctx не отменён. Последнюю запись делает Close.
func (d *DB) Run(ctx context.Context) {
	if d == nil {
		return
	}
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
		d.flush(false)
	}
}

// Flush пишет изменения на диск сейчас.
func (d *DB) Flush() {
	d.flush(false)
}

// Close записывает всё, что не записано, с отметкой работы шлюза и закрывает базу.
func (d *DB) Close() {
	if d == nil {
		return
	}
	d.flush(true)
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.Lock()
	db := d.db
	d.db, d.closed = nil, true
	d.mu.Unlock()
	if db != nil {
		db.Close()
	}
}

// Failing сообщает, что последняя запись на диск не удалась: состояние лежит только в памяти.
func (d *DB) Failing() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failing
}

// wakeUp просит записать изменения сейчас, не дожидаясь flushEvery.
func (d *DB) wakeUp() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// openFile открывает файл базы и приводит его схему к текущей. Файл создаётся с правами 0600:
// журнал и индекс SQLite получают те же права.
func openFile(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	dsn := url.URL{Scheme: "file", Path: abs,
		RawQuery: "_pragma=busy_timeout(3000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := prepare(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// prepare проверяет файл и применяет новые строки схемы.
func prepare(ctx context.Context, db *sql.DB) error {
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("%w: %s", errBroken, check)
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("%w: версия схемы %d", errBroken, version)
	}
	for ; version < len(migrations); version++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// isBroken отличает сломанную базу от сбоя записи. Заполненный диск и том только для чтения базу
// не ломают: если бы шлюз пересоздавал её при них, он стирал бы исправный файл.
func isBroken(err error) bool {
	if errors.Is(err, errBroken) {
		return true
	}
	var e *sqlite.Error
	if errors.As(err, &e) {
		switch e.Code() & 0xff {
		case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
			return true
		}
	}
	return false
}

// setAside откладывает файл сломанной базы вместе с её журналом, заменяя прежний отложенный файл.
func (d *DB) setAside() error {
	os.Remove(d.path + brokenSuffix + "-wal")
	if err := os.Rename(d.path, d.path+brokenSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(d.path+"-wal", d.path+brokenSuffix+"-wal"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	os.Remove(d.path + "-shm")
	return nil
}

// connect открывает базу и читает сохранённое; сломанная база пересоздаётся. Только под writeMu.
func (d *DB) connect() error {
	db, err := openFile(d.path)
	if isBroken(err) {
		if db, err = d.replace(err); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	saved, err := load(db, d.now())
	if isBroken(err) {
		db.Close()
		if db, err = d.replace(err); err != nil {
			return err
		}
		saved = &loaded{devices: map[string]*device{}}
	} else if err != nil {
		db.Close()
		return err
	}
	d.mu.Lock()
	d.db = db
	d.merge(saved)
	d.mu.Unlock()
	d.remember()
	return nil
}

// replace откладывает сломанный файл и создаёт новую базу.
func (d *DB) replace(cause error) (*sql.DB, error) {
	d.log.Error("база состояния сломана, шлюз создаёт новую", "path", d.path, "err", cause)
	if err := d.setAside(); err != nil {
		return nil, err
	}
	metrics.StoreRecreated()
	return openFile(d.path)
}

// remember запоминает открытый файл.
func (d *DB) remember() {
	d.file = nil
	if info, err := os.Stat(d.path); err == nil {
		d.file = info
	}
}

// fileGone сообщает, что открытый файл удалили или заменили: запись шла бы в никуда.
func (d *DB) fileGone() bool {
	info, err := os.Stat(d.path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return err == nil && d.file != nil && !os.SameFile(info, d.file)
}

// recreate заменяет открытую базу новой и помечает всё из памяти к записи. aside - файл сломан и
// откладывается; удалённый файл откладывать незачем. Только под writeMu.
func (d *DB) recreate(cause error, aside bool) error {
	d.mu.Lock()
	old := d.db
	d.db = nil
	d.mu.Unlock()
	if old != nil {
		old.Close()
	}
	var db *sql.DB
	var err error
	if aside {
		db, err = d.replace(cause)
	} else {
		d.log.Error("файл базы состояния пропал, шлюз создаёт новый", "path", d.path)
		metrics.StoreRecreated()
		db, err = openFile(d.path)
	}
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.db, d.broken = db, false
	d.markAll()
	d.mu.Unlock()
	d.remember()
	return nil
}

// flush пишет изменения на диск. final - последняя запись: с отметкой работы шлюза.
func (d *DB) flush(final bool) {
	if d == nil {
		return
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.Lock()
	closed, connected, broken := d.closed, d.db != nil, d.broken
	d.mu.Unlock()
	if closed {
		return
	}
	now := d.now()
	if !connected {
		if err := d.connect(); err != nil {
			d.fail(err)
			return
		}
	}
	if connected && now.Sub(d.lastCheck) >= aliveEvery {
		d.lastCheck = now
		if d.fileGone() {
			if err := d.recreate(nil, false); err != nil {
				d.fail(err)
				return
			}
		}
	}
	if broken {
		if err := d.recreate(errBroken, true); err != nil {
			d.fail(err)
			return
		}
	}
	alive := final || now.Sub(d.lastAlive) >= aliveEvery
	prune := now.Sub(d.lastPrune) >= pruneEvery
	d.mu.Lock()
	db := d.db
	b := d.take(now, alive, prune)
	d.mu.Unlock()
	if b.empty() {
		return
	}
	err := b.write(db)
	if err != nil {
		d.mu.Lock()
		d.restore(b)
		d.mu.Unlock()
		d.fail(err)
		if isBroken(err) {
			if err := d.recreate(err, true); err != nil {
				d.fail(err)
				return
			}
			d.wakeUp()
		}
		return
	}
	if alive {
		d.lastAlive = now
	}
	if prune {
		d.lastPrune = now
	}
	d.ok()
}

// fail запоминает сбой записи.
func (d *DB) fail(err error) {
	metrics.StoreError()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing = true
	if text := err.Error(); text != d.lastErr {
		d.lastErr = text
		d.log.Error("база состояния: запись не удалась, состояние лежит в памяти", "path", d.path, "err", err)
	}
}

// ok запоминает, что запись снова идёт.
func (d *DB) ok() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failing {
		d.log.Info("база состояния: запись возобновилась", "path", d.path)
	}
	d.failing, d.lastErr = false, ""
}

// noteRead запоминает ошибку чтения: сломанную базу пересоздаст следующая запись.
func (d *DB) noteRead(err error) {
	if !isBroken(err) {
		return
	}
	d.mu.Lock()
	d.broken = true
	d.mu.Unlock()
	d.wakeUp()
}

// handle - открытая база или nil.
func (d *DB) handle() *sql.DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.db
}

// millis и fromMillis - время в базе: миллисекунды Unix, пустое время - NULL.
func millis(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromMillis(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

// encode - значение прибора в JSON, не длиннее MaxValueBytes. Длинное значение становится
// обрезанной строкой: база нужна только для показа.
func encode(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if len(data) <= MaxValueBytes {
		return string(data), nil
	}
	text, ok := v.(string)
	if !ok {
		text = string(data)
	}
	for n := MaxValueBytes; ; n /= 2 {
		if data, err = json.Marshal(clip(text, n)); err != nil {
			return "", err
		}
		if len(data) <= MaxValueBytes {
			return string(data), nil
		}
	}
}

// clip обрезает строку до n байт, не разрывая символ.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}
