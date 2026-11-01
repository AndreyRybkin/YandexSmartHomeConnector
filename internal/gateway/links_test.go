package gateway

import (
	"testing"
	"time"

	"hcgateway/internal/config"
	"hcgateway/internal/metrics"
)

// С какого времени прибор не на связи: с первой проверки после запуска, с потери связи, а
// прибор на связи и удалённый прибор - не без связи.
func TestLinks(t *testing.T) {
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	l := newLinks(start)
	l.update(start.Add(5*time.Second), []metrics.Device{{ID: "oven", Online: true}, {ID: "strip"}})
	if since, fromStart, ok := l.offline("strip"); !ok || !fromStart || !since.Equal(start.Add(5*time.Second)) {
		t.Fatalf("лента без связи с запуска: %v %v %v", since, fromStart, ok)
	}
	if _, _, ok := l.offline("oven"); ok {
		t.Fatal("духовка на связи")
	}
	lost := start.Add(10 * time.Minute)
	l.update(lost, []metrics.Device{{ID: "oven"}, {ID: "strip"}})
	l.update(lost.Add(time.Minute), []metrics.Device{{ID: "oven"}, {ID: "strip"}, {ID: "tv"}})
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
	l.update(lost.Add(2*time.Minute), []metrics.Device{{ID: "oven", Online: true}, {ID: "tv"}})
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
