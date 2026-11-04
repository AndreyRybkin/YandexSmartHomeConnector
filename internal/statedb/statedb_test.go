package statedb

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/metrics"
)

var (
	t0   = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	addr = netip.MustParsePrefix("203.0.113.7/32")
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	d := open(path, discard(), func() time.Time { return t0 })
	t.Cleanup(d.Close)
	return d
}

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), FileName)
}

// Всё сохранённое переживает закрытие базы, а файлы базы закрыты для других пользователей.
func TestReopen(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	link := Link{First: t0, Off: t0.Add(time.Hour), Seen: true}
	d.SetLink("oven", link)
	d.SetValues("oven", map[string]any{"door": "open", "temp": 180.5, "on": true}, t0)
	d.AddChanges("oven", []Change{{Time: t0, Kind: "value", Key: "door", Value: "open"}})
	d.BanStarted(addr, t0, t0.Add(time.Hour), "привязка")
	d.flush(false)
	if d.Failing() {
		t.Fatal("запись не удалась")
	}
	for _, name := range []string{path, path + "-wal"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("права %s: %v", name, info.Mode().Perm())
		}
	}
	d.Close()

	r := openAt(t, path)
	if got := r.Links()["oven"]; !sameLink(got, link) {
		t.Fatalf("связь: %+v", got)
	}
	values := r.Values("oven")
	if len(values) != 3 || values[0].Key != "door" || values[0].Value != "open" || values[1].Value != true || values[2].Value != 180.5 {
		t.Fatalf("значения: %+v", values)
	}
	if c := r.Changes("oven"); len(c) != 1 || c[0].Key != "door" || !c[0].Time.Equal(t0) {
		t.Fatalf("изменения: %+v", c)
	}
	if !r.Alive().Equal(t0) {
		t.Fatalf("отметка работы: %v", r.Alive())
	}
	bans := r.ActiveBans(t0)
	if len(bans) != 1 || bans[0].Prefix != addr || bans[0].Where != "привязка" {
		t.Fatalf("баны: %+v", bans)
	}
	if len(r.ActiveBans(t0.Add(time.Hour))) != 0 {
		t.Fatal("истёкший бан считается действующим")
	}
}

// Имя, которого нет в новом состоянии, остаётся со временем пропажи, а вернувшись, теряет его.
func TestValuesMerge(t *testing.T) {
	d := openAt(t, tempPath(t))
	d.SetValues("tv", map[string]any{"power": "on", "volume": 20.0}, t0)
	off := t0.Add(time.Minute)
	d.SetValues("tv", map[string]any{"power": "off"}, off)
	d.SetValues("tv", map[string]any{"power": "off"}, off.Add(time.Minute))
	v := d.Values("tv")
	if len(v) != 2 || v[0].Value != "off" || !v[0].Gone.IsZero() || v[1].Value != 20.0 || !v[1].Gone.Equal(off) {
		t.Fatalf("значения: %+v", v)
	}
	d.SetValues("tv", map[string]any{"power": "on", "volume": 20.0}, off)
	if v = d.Values("tv"); !v[1].Gone.IsZero() {
		t.Fatalf("громкость вернулась, а время пропажи осталось: %+v", v)
	}
}

// Записываются только изменившиеся значения.
func TestOnlyChangedWritten(t *testing.T) {
	d := openAt(t, tempPath(t))
	d.SetValues("oven", map[string]any{"a": 1.0, "b": 2.0}, t0)
	d.flush(false)
	d.SetValues("oven", map[string]any{"a": 1.0, "b": 3.0}, t0)
	d.mu.Lock()
	b := d.take(t0, false, false)
	d.mu.Unlock()
	if len(b.values) != 1 || b.values[0].key != "b" {
		t.Fatalf("к записи: %+v", b.values)
	}
	if !d.empty() {
		t.Fatal("после записи осталось несохранённое")
	}
}

func (d *DB) empty() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.take(t0, false, false).empty()
}

// Пределы: длинное значение обрезается и остаётся JSON, длинное имя не хранится, лишние имена
// вытесняют самые старые из пропавших, а если таких нет - не хранятся.
func TestValueLimits(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	long := strings.Repeat("я", 2*MaxValueBytes)
	d.SetValues("a", map[string]any{"text": long, "list": []any{long}, strings.Repeat("k", maxKeyBytes+1): 1.0}, t0)
	d.flush(false)
	d.Close()
	r := openAt(t, path)
	v := r.Values("a")
	if len(v) != 2 {
		t.Fatalf("значения: %d", len(v))
	}
	for _, x := range v {
		text, ok := x.Value.(string)
		if !ok || text == "" || len(text) > MaxValueBytes {
			t.Fatalf("%s: %T длиной %d", x.Key, x.Value, len(text))
		}
	}

	names := map[string]any{}
	for i := range MaxNames {
		names[fmt.Sprint("n", i)] = 1.0
	}
	r.SetValues("b", names, t0)
	names["extra"] = 1.0
	r.SetValues("b", names, t0)
	if len(r.Values("b")) != MaxNames {
		t.Fatalf("имён: %d", len(r.Values("b")))
	}
	delete(names, "extra")
	delete(names, "n0")
	r.SetValues("b", names, t0)
	names["extra"] = 1.0
	r.SetValues("b", names, t0.Add(time.Minute))
	got := r.Values("b")
	if len(got) != MaxNames || got[len(got)-1].Key == "n0" {
		t.Fatalf("пропавшее имя не уступило место: %d", len(got))
	}
	for _, x := range got {
		if x.Key == "n0" {
			t.Fatal("n0 не вытеснено")
		}
	}
}

// В базе и в памяти - последние MaxChanges изменений; удалённый прибор забывается в базе.
func TestChangesAndRetain(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	for i := range MaxChanges + 5 {
		d.AddChanges("oven", []Change{{Time: t0, Kind: "value", Key: fmt.Sprint(i)}})
		d.flush(false)
	}
	d.AddChanges("gone", []Change{{Time: t0, Kind: "value", Key: "x"}})
	d.SetLink("gone", Link{First: t0})
	d.SetValues("gone", map[string]any{"a": 1.0}, t0)
	d.flush(false)
	d.Retain(func(id string) bool { return id != "gone" })
	d.Close()
	r := openAt(t, path)
	c := r.Changes("oven")
	if len(c) != MaxChanges || c[0].Key != "5" || c[len(c)-1].Key != fmt.Sprint(MaxChanges+4) {
		t.Fatalf("изменения: %d, %+v", len(c), c[0])
	}
	if r.Changes("gone") != nil || r.Values("gone") != nil || len(r.Links()) != 0 {
		t.Fatal("удалённый прибор остался в базе")
	}
}

// Отметки простоя подряд схлопываются, а прибору без истории не добавляются.
func TestMarkGap(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	d.AddChanges("oven", []Change{{Time: t0, Kind: "value", Key: "door"}})
	d.SetLink("strip", Link{First: t0})
	d.MarkGap(Change{Time: t0, Value: "не работал 1 мин"})
	d.flush(false)
	d.MarkGap(Change{Time: t0, Value: "не работал 2 мин"})
	d.flush(false)
	d.Close()
	r := openAt(t, path)
	c := r.Changes("oven")
	if len(c) != 2 || c[1].Kind != GapKind || c[1].Value != "не работал 2 мин" {
		t.Fatalf("история: %+v", c)
	}
	if len(r.Changes("strip")) != 0 {
		t.Fatal("отметка у прибора без истории")
	}
}

// Файл, испорченный до запуска, откладывается, а база начинается с нуля.
func TestBrokenAtStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{"мусор", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("мусор", 2000)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"порченые страницы", func(t *testing.T, path string) {
			d := Open(path, discard())
			for i := range 2000 {
				d.AddChanges(fmt.Sprint("d", i), []Change{{Time: t0, Kind: "value", Key: strings.Repeat("k", 200)}})
			}
			d.Close()
			corrupt(t, path)
		}},
		{"схема новее кода", func(t *testing.T, path string) {
			d := Open(path, discard())
			if _, err := d.db.Exec("PRAGMA user_version = 999"); err != nil {
				t.Fatal(err)
			}
			d.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tempPath(t)
			tc.make(t, path)
			before := metrics.Value("hcgateway_store_recreated_total")
			d := openAt(t, path)
			if d.Failing() {
				t.Fatal("новая база не открылась")
			}
			if _, err := os.Stat(path + brokenSuffix); err != nil {
				t.Fatal("сломанный файл не отложен:", err)
			}
			if metrics.Value("hcgateway_store_recreated_total") != before+1 {
				t.Fatal("пересоздание не посчитано")
			}
			if len(d.Links()) != 0 || len(d.Changes("d1")) != 0 {
				t.Fatal("новая база не пуста")
			}
			d.SetLink("oven", Link{First: t0})
			d.flush(false)
			if d.Failing() {
				t.Fatal("запись в новую базу не удалась")
			}
		})
	}
}

// corrupt портит страницы файла за заголовком.
func corrupt(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte(strings.Repeat("\xff", 64*1024)), 4096); err != nil {
		t.Fatal(err)
	}
}

// Файл, испорченный на работающем шлюзе: следующая запись это находит, а в новой базе - всё из памяти.
func TestBrokenWhileRunning(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	d.SetLink("oven", Link{First: t0, Online: true, Seen: true})
	d.SetValues("oven", map[string]any{"door": "open"}, t0)
	d.AddChanges("oven", []Change{{Time: t0, Kind: "value", Key: "door"}})
	d.BanStarted(addr, t0, t0.Add(time.Hour), "привязка")
	for i := range 2000 {
		d.AddChanges(fmt.Sprint("d", i), []Change{{Time: t0, Kind: "value", Key: strings.Repeat("k", 200)}})
	}
	d.flush(false)
	if _, err := d.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec("PRAGMA cache_size = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec("PRAGMA shrink_memory"); err != nil {
		t.Fatal(err)
	}
	corrupt(t, path)
	d.AddChanges("oven", []Change{{Time: t0, Kind: "value", Key: "after"}})
	d.flush(false)
	if _, err := os.Stat(path + brokenSuffix); err != nil {
		t.Fatal("сломанный файл не отложен:", err)
	}
	d.flush(false)
	if d.Failing() {
		t.Fatal("запись в новую базу не удалась")
	}
	d.Close()
	r := openAt(t, path)
	if len(r.Links()) != 1 || len(r.Values("oven")) != 1 || len(r.Changes("oven")) != 2 || len(r.ActiveBans(t0)) != 1 {
		t.Fatalf("в новой базе не всё из памяти: %v %v %v %v", r.Links(), r.Values("oven"), r.Changes("oven"), r.ActiveBans(t0))
	}
}

// Удалённый файл шлюз замечает раз в минуту и создаёт заново из памяти.
func TestFileRemoved(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	d.SetLink("oven", Link{First: t0})
	d.flush(false)
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		os.Remove(name)
	}
	now := t0.Add(time.Minute)
	d.now = func() time.Time { return now }
	d.flush(false)
	if d.Failing() {
		t.Fatal("новая база не записана")
	}
	if _, err := os.Stat(path + brokenSuffix); err == nil {
		t.Fatal("откладывать нечего")
	}
	d.Close()
	if r := openAt(t, path); len(r.Links()) != 1 {
		t.Fatalf("связь не записана заново: %v", r.Links())
	}
}

// Заполненный диск базу не пересоздаёт: файл на месте, запись возобновляется, как только есть место.
func TestDiskFull(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	d.SetLink("oven", Link{First: t0})
	d.flush(false)
	var pages int
	if err := d.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		d.AddChanges(fmt.Sprint("d", i), []Change{{Time: t0, Kind: "value", Key: strings.Repeat("k", 500)}})
	}
	before := metrics.Value("hcgateway_store_errors_total")
	d.flush(false)
	if !d.Failing() || metrics.Value("hcgateway_store_errors_total") <= before {
		t.Fatal("сбой записи не замечен")
	}
	if _, err := os.Stat(path + brokenSuffix); err == nil {
		t.Fatal("заполненный диск пересоздал базу")
	}
	if _, err := d.db.Exec("PRAGMA max_page_count = 1000000"); err != nil {
		t.Fatal(err)
	}
	d.flush(false)
	if d.Failing() {
		t.Fatal("запись не возобновилась")
	}
	d.Close()
	r := openAt(t, path)
	if len(r.Links()) != 1 || len(r.Changes("d499")) != 1 {
		t.Fatal("после сбоя записано не всё")
	}
}

// Том только для чтения: шлюз работает на памяти и не трогает файл.
func TestReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root пишет в файл без прав записи")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	d := Open(path, discard())
	d.SetLink("oven", Link{First: t0})
	d.Close()
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	r := Open(path, discard())
	defer r.Close()
	r.SetLink("oven", Link{First: t0.Add(time.Hour)})
	r.flush(false)
	if !r.Failing() {
		t.Fatal("сбой не замечен")
	}
	if _, err := os.Stat(path + brokenSuffix); err == nil {
		t.Fatal("база только для чтения пересоздана")
	}
	if got := r.Links()["oven"]; !got.First.Equal(t0.Add(time.Hour)) {
		t.Fatalf("память: %+v", got)
	}
}

// Журнал банов: итоги, постраничное чтение и пределы.
func TestBanJournal(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	other := netip.MustParsePrefix("203.0.113.8/32")
	d.BanStarted(addr, t0, t0.Add(time.Hour), "привязка")
	d.BanStarted(other, t0.Add(time.Minute), t0.Add(time.Hour+time.Minute), "веб-интерфейс")
	d.BanEnded(other, t0.Add(time.Hour+time.Minute), EndedWeb, t0.Add(2*time.Minute))
	d.flush(false)
	rows, err := d.BanHistory(0, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("журнал: %v %v", rows, err)
	}
	if rows[0].Prefix != other || rows[0].EndedBy != EndedWeb || !rows[0].Ended.Equal(t0.Add(2*time.Minute)) || rows[1].EndedBy != "" {
		t.Fatalf("строки: %+v", rows)
	}
	if older, err := d.BanHistory(rows[0].ID, 100); err != nil || len(older) != 1 || older[0].Prefix != addr {
		t.Fatalf("старые: %v %v", older, err)
	}
	if len(d.ActiveBans(t0)) != 1 {
		t.Fatal("снятый бан считается действующим")
	}

	// Предел по числу строк и по сроку.
	if _, err := d.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO bans (prefix, started_at, until, place) SELECT '198.51.100.1/32', ?, ?, 'привязка' FROM n`,
		MaxBans+10, t0.UnixMilli(), t0.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	d.lastPrune = time.Time{}
	d.flush(false)
	var count int
	if err := d.db.QueryRow("SELECT count(*) FROM bans").Scan(&count); err != nil || count != MaxBans {
		t.Fatalf("строк: %d %v", count, err)
	}
	now := t0.Add(BanKeep + time.Hour)
	d.now = func() time.Time { return now }
	d.lastPrune = time.Time{}
	d.SetLink("oven", Link{First: t0})
	d.flush(false)
	if err := d.db.QueryRow("SELECT count(*) FROM bans").Scan(&count); err != nil || count != 0 {
		t.Fatalf("старше срока: %d %v", count, err)
	}
}

// Команда снимает бан в файле, пока шлюз работает: шлюз видит это в журнале.
func TestUnban(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	other := netip.MustParsePrefix("203.0.113.8/32")
	until := t0.Add(time.Hour)
	d.BanStarted(addr, t0, until, "привязка")
	d.BanStarted(other, t0, until, "привязка")
	d.flush(false)
	if n, err := Unban(path, addr, false, t0.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("один бан: %d %v", n, err)
	}
	lifted := d.LiftedByCommand(t0.Add(2 * time.Minute))
	if len(lifted) != 1 || lifted[0].Prefix != addr || lifted[0].Until.UnixMilli() != until.UnixMilli() {
		t.Fatalf("снятые: %+v", lifted)
	}
	if n, err := Unban(path, netip.Prefix{}, true, t0.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("все баны: %d %v", n, err)
	}
	if bans := d.ActiveBans(t0); len(bans) != 1 {
		t.Fatalf("после чтения журнала: %+v", bans)
	}
	if n, err := Unban(filepath.Join(t.TempDir(), FileName), addr, false, t0); err != nil || n != 0 {
		t.Fatalf("файла нет: %d %v", n, err)
	}
}

// Путь с пробелом и знаками, которые значат своё в адресе файла.
func TestPathWithSpecialChars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home connect #1?x", FileName)
	d := openAt(t, path)
	d.SetLink("oven", Link{First: t0})
	d.flush(false)
	if d.Failing() {
		t.Fatal("запись не удалась")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// Без базы методы ничего не делают: так шлюз работает в тестах других пакетов.
func TestNil(t *testing.T) {
	var d *DB
	d.SetLink("a", Link{})
	d.SetValues("a", nil, t0)
	d.AddChanges("a", []Change{{}})
	d.MarkGap(Change{})
	d.AddAction("a", Action{}, nil)
	d.Retain(func(string) bool { return false })
	d.BanStarted(addr, t0, t0, "")
	d.BanEnded(addr, t0, EndedWeb, t0)
	d.flush(false)
	d.Run(context.Background())
	d.Close()
	if d.Failing() || d.Links() != nil || d.Values("a") != nil || d.Changes("a") != nil || d.Actions("") != nil || d.ActiveBans(t0) != nil || d.LiftedByCommand(t0) != nil {
		t.Fatal("у nil есть данные")
	}
	if rows, err := d.BanHistory(0, 1); rows != nil || err != nil {
		t.Fatal(rows, err)
	}
}

// Журнал действий переживает перезапуск, хранит 50 строк на прибор и уходит вместе с прибором.
func TestActions(t *testing.T) {
	path := tempPath(t)
	d := openAt(t, path)
	d.AddAction("oven", Action{Time: t0, Initiator: "alice", Type: "on_off", Instance: "on"}, true)
	d.AddAction("oven", Action{Time: t0.Add(time.Second), Initiator: "gateway", Type: "range", Instance: "volume", Relative: true, Code: "DOOR_OPEN"}, 5.0)
	d.AddAction("tv", Action{Time: t0.Add(2 * time.Second), Initiator: "gateway", Type: "on_off", Instance: "on"}, false)
	d.flush(false)
	d.Close()

	r := openAt(t, path)
	all := r.Actions("")
	if len(all) != 3 || all[0].Device != "tv" || all[2].Device != "oven" || all[2].Value != "true" {
		t.Fatalf("все действия: %+v", all)
	}
	oven := r.Actions("oven")
	if len(oven) != 2 || oven[0].Initiator != "gateway" || oven[0].Code != "DOOR_OPEN" || !oven[0].Relative || oven[0].Value != "5" || !oven[0].Time.Equal(t0.Add(time.Second)) {
		t.Fatalf("действия прибора: %+v", oven)
	}

	// 51-е действие вытесняет самое старое и в памяти, и в файле.
	for i := range MaxActions {
		r.AddAction("oven", Action{Time: t0.Add(time.Duration(i+10) * time.Second), Initiator: "alice", Type: "on_off"}, true)
		if i%20 == 0 {
			r.flush(false)
		}
	}
	r.flush(false)
	check := func(db *DB) {
		t.Helper()
		got := db.Actions("oven")
		if len(got) != MaxActions || !got[0].Time.Equal(t0.Add((MaxActions+9)*time.Second)) || !got[MaxActions-1].Time.Equal(t0.Add(10*time.Second)) {
			t.Fatalf("после вытеснения %d строк: %+v", len(got), got)
		}
	}
	check(r)
	var rows int
	if err := r.handle().QueryRow("SELECT count(*) FROM device_actions WHERE device_id = 'oven'").Scan(&rows); err != nil || rows != MaxActions {
		t.Fatalf("в файле %d строк: %v", rows, err)
	}
	r.Close()
	r2 := openAt(t, path)
	check(r2)

	// Прибора больше нет в настройках: его действия удаляются из памяти и файла.
	r2.Retain(func(id string) bool { return id != "oven" })
	if got := r2.Actions("oven"); len(got) != 0 {
		t.Fatalf("действия удалённого прибора: %+v", got)
	}
	r2.flush(false)
	r2.Close()
	if got := openAt(t, path).Actions(""); len(got) != 1 || got[0].Device != "tv" {
		t.Fatalf("после удаления прибора: %+v", got)
	}
}

// Длинное значение команды обрезается, а не раздувает базу.
func TestActionValueLimit(t *testing.T) {
	d := openAt(t, tempPath(t))
	d.AddAction("oven", Action{Time: t0, Type: strings.Repeat("t", 10000)}, strings.Repeat("v", 10000))
	got := d.Actions("oven")
	if len(got) != 1 || len(got[0].Value) > MaxValueBytes || len(got[0].Type) > maxKeyBytes {
		t.Fatalf("пределы: %d, %d", len(got[0].Value), len(got[0].Type))
	}
}
