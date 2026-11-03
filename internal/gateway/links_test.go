package gateway

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"hcgateway/internal/config"
	"hcgateway/internal/metrics"
	"hcgateway/internal/statedb"
)

// С какого времени прибор не на связи: с первой проверки после запуска, с потери связи, а
// прибор на связи и удалённый прибор - не без связи.
func TestLinks(t *testing.T) {
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	l := newLinks(start)
	removed := func(string) bool { return false }
	l.update(start.Add(5*time.Second), []metrics.Device{{ID: "oven", Online: true}, {ID: "strip"}}, removed)
	if since, fromStart, ok := l.offline("strip"); !ok || !fromStart || !since.Equal(start.Add(5*time.Second)) {
		t.Fatalf("лента без связи с запуска: %v %v %v", since, fromStart, ok)
	}
	if _, _, ok := l.offline("oven"); ok {
		t.Fatal("духовка на связи")
	}
	lost := start.Add(10 * time.Minute)
	l.update(lost, []metrics.Device{{ID: "oven"}, {ID: "strip"}}, removed)
	l.update(lost.Add(time.Minute), []metrics.Device{{ID: "oven"}, {ID: "strip"}, {ID: "tv"}}, removed)
	if since, fromStart, ok := l.offline("oven"); !ok || fromStart || !since.Equal(lost) {
		t.Fatalf("духовка без связи с %v: %v %v %v", lost, since, fromStart, ok)
	}
	if since, _, _ := l.offline("strip"); !since.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("время ленты сдвинулось: %v", since)
	}
	// Прибор добавлен позже: время - с его первой проверки, не с запуска.
	if since, fromStart, ok := l.offline("tv"); !ok || fromStart || !since.Equal(lost.Add(time.Minute)) {
		t.Fatalf("телевизор: %v %v %v", since, fromStart, ok)
	}
	l.update(lost.Add(2*time.Minute), []metrics.Device{{ID: "oven", Online: true}, {ID: "tv"}}, removed)
	if _, _, ok := l.offline("oven"); ok {
		t.Fatal("духовка вернулась, а время без связи осталось")
	}
	if _, _, ok := l.offline("strip"); ok || len(l.first) != 2 {
		t.Fatal("удалённая лента не забыта")
	}
}

// Шлюз сам проверяет связь, если её давно не проверяли.
func TestOffline(t *testing.T) {
	g := newGateway(t)
	cfg := baseConfig()
	cfg.Devices["strip"] = config.Device{Name: "Лента", Source: config.SourceWLED, Host: "127.0.0.1:1"}
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if _, fromStart, ok := g.Offline("strip"); !ok || !fromStart {
		t.Fatalf("лента: %v %v", fromStart, ok)
	}
	if _, _, ok := g.Offline("oven"); ok {
		t.Fatal("демо-духовка на связи")
	}
}

// Связь из базы: прибор без связи продолжает отсчёт, бывший на связи пропал с отметки работы шлюза,
// а прибор, которого нет в проверке, но есть в настройках, своё время не теряет.
func TestLinksRestore(t *testing.T) {
	alive := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	longAgo := alive.Add(-72 * time.Hour)
	start := alive.Add(time.Hour)
	l := newLinks(start)
	l.restore(map[string]statedb.Link{
		"lost":    {First: longAgo, Off: longAgo.Add(time.Hour), Seen: true},
		"never":   {First: longAgo, Off: longAgo},
		"was":     {First: longAgo, Online: true, Seen: true},
		"back":    {First: longAgo, Online: true, Seen: true},
		"waiting": {First: longAgo, Off: longAgo, Seen: true},
		"removed": {First: longAgo, Off: longAgo, Seen: true},
	}, alive)
	configured := func(id string) bool { return id != "removed" }
	l.update(start.Add(5*time.Second), []metrics.Device{{ID: "lost"}, {ID: "never"}, {ID: "was"}, {ID: "back", Online: true}, {ID: "fresh"}}, configured)
	for id, want := range map[string]time.Time{"lost": longAgo.Add(time.Hour), "never": longAgo, "was": alive, "waiting": longAgo} {
		if since, fromStart, ok := l.offline(id); !ok || fromStart || !since.Equal(want) {
			t.Fatalf("%s: %v %v %v", id, since, fromStart, ok)
		}
	}
	if _, _, ok := l.offline("back"); ok {
		t.Fatal("вернувшийся прибор без связи")
	}
	if _, fromStart, ok := l.offline("fresh"); !ok || !fromStart {
		t.Fatal("прибор без записи в базе - с запуска шлюза")
	}
	if _, ok := l.saved()["removed"]; ok {
		t.Fatal("удалённый прибор не забыт")
	}
	if s := l.saved()["waiting"]; !s.First.Equal(longAgo) || s.Online {
		t.Fatalf("ждущий прибор: %+v", s)
	}
}

// Перезапуск шлюза: время без связи и значения остаются, в истории виден простой, история
// переживает пересборку прибора, а удалённый прибор исчезает из базы.
func TestRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), statedb.FileName)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := baseConfig()
	cfg.Devices["strip"] = config.Device{Name: "Лента", Source: config.SourceWLED, Host: "127.0.0.1:1"}
	cfg.Devices["gone"] = config.Device{Name: "Лента 2", Source: config.SourceWLED, Host: "127.0.0.1:1"}

	state := statedb.Open(path, log)
	g := newGateway(t)
	g.Attach(state)
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	checked := time.Now().Add(-3 * time.Hour)
	g.checkLinks(checked)
	if v := g.LastValues("oven"); len(v) == 0 {
		t.Fatal("значения прибора на связи не сохранены")
	}
	state.AddChanges("oven", []statedb.Change{{Time: checked, Kind: "value", Key: "door", Value: "open"}})
	state.Close()

	state = statedb.Open(path, log)
	defer state.Close()
	g = newGateway(t)
	g.Attach(state)
	delete(cfg.Devices, "gone")
	if err := g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if since, fromStart, ok := g.links.offline("strip"); !ok || fromStart || since.UnixMilli() != checked.UnixMilli() {
		t.Fatalf("лента: %v %v %v", since, fromStart, ok)
	}
	if v := g.LastValues("oven"); len(v) == 0 {
		t.Fatal("значения не пережили перезапуск")
	}
	oven, ok := g.HomeConnect("oven")
	if !ok {
		t.Fatal("нет духовки")
	}
	history := oven.History()
	if len(history) != 2 || history[0].Kind != statedb.GapKind || history[1].Key != "door" {
		t.Fatalf("история: %+v", history)
	}
	// Правка имени собирает прибор заново, а история остаётся.
	next := *cfg
	next.Devices = map[string]config.Device{}
	for id, d := range cfg.Devices {
		next.Devices[id] = d
	}
	d := next.Devices["oven"]
	d.Name = "Новое имя"
	next.Devices["oven"] = d
	if err := g.Apply(&next); err != nil {
		t.Fatal(err)
	}
	renamed, _ := g.HomeConnect("oven")
	if renamed == oven || len(renamed.History()) != 2 {
		t.Fatalf("после правки: %+v", renamed.History())
	}
	g.checkLinks(time.Now())
	if g.LastValues("gone") != nil {
		t.Fatal("удалённый прибор остался в базе")
	}
}
