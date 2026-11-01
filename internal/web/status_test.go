package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/alice"
)

// fakeDevice - устройство Алисы с заданными состояниями.
type fakeDevice struct{ states []alice.DeviceState }

func (d fakeDevice) IDs() []string                                     { return nil }
func (d fakeDevice) Info() []alice.DeviceInfo                          { return nil }
func (d fakeDevice) States() []alice.DeviceState                       { return d.states }
func (d fakeDevice) Act(context.Context, string, *alice.Action) string { return "" }
func (d fakeDevice) Run(context.Context, alice.Changed)                {}

// Заряд - свойство самого устройства, «заканчивается» - событие его датчика.
func TestBatteryAndFood(t *testing.T) {
	dev := fakeDevice{states: []alice.DeviceState{
		{ID: "sensor", Properties: []alice.CapState{{Type: alice.PropFloat, State: alice.StateValue{Instance: "battery_level", Value: 12.0}}}},
		{ID: "sensor.events", Properties: []alice.CapState{{Type: alice.PropEvent, State: alice.StateValue{Instance: "food_level", Value: "low"}}}},
	}}
	if v, ok := battery(dev, "sensor"); !ok || v != 12 {
		t.Fatalf("заряд: %v %v", v, ok)
	}
	if _, ok := battery(dev, "sensor.events"); ok {
		t.Fatal("заряд у датчика событий")
	}
	if !foodLow(dev) {
		t.Fatal("нет «заканчивается»")
	}
	if foodLow(fakeDevice{states: []alice.DeviceState{{ID: "x", Properties: []alice.CapState{{Type: alice.PropEvent,
		State: alice.StateValue{Instance: "food_level", Value: "normal"}}}}}}) {
		t.Fatal("«заканчивается» при normal")
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Second: "", 5 * time.Minute: " 5 мин", 3 * time.Hour: " 3 ч",
		47 * time.Hour: " 47 ч", 72 * time.Hour: " 3 дн"} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, нужно %q", d, got, want)
		}
	}
	rows := []deviceRow{{Problem: "battery"}, {Problem: "offline"}, {}, {Problem: "offline"}}
	if got := attention(rows); got != "без связи: 2 · разряжены: 1" {
		t.Fatalf("полоса: %q", got)
	}
}

const attentionConfig = `devices:
  oven:
    name: Духовка
    room: Кухня
    source: demo
    demo: Oven
  strip:
    name: Лента
    room: Кухня
    source: wled
    host: 127.0.0.1:1
  tv:
    name: Телевизор
    room: Гостиная
    source: webos
    host: 127.0.0.1
`

// Полоса «Требует внимания» считает приборы по причинам, а ссылка из неё оставляет в списке
// только их. Без связи - серая плитка и сколько прибор без связи.
func TestAttention(t *testing.T) {
	f := newFixture(t, attentionConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/")
	for _, want := range []string{`<a href="/?problems=1">Требует внимания · <span data-counts>без связи: 1 · ждут разрешения телевизора: 1</span></a>`,
		`<article class="tile off" id="d-strip">`, `<span class="chip off">нет связи с запуска шлюза</span>`,
		`<span class="chip warn">ещё не разрешил подключение</span>`, `<article class="tile idle" id="d-oven">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("в списке нет %s", want)
		}
	}
	_, body = f.get("/?problems=1")
	for _, want := range []string{`data-filter`, `<a href="/">Показать все</a>`, `<article class="tile idle" id="d-oven" hidden>`,
		`<article class="tile off" id="d-strip">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("в отфильтрованном списке нет %s", want)
		}
	}
	// Без проблем полоса есть, но скрыта: её показывает скрипт, когда проблема появится.
	g := newFixture(t, demoConfig)
	g.writeUser(testPassword)
	g.login()
	if _, body = g.get("/"); !strings.Contains(body, `id="attention" data-problems="offline:без связи|`) || !strings.Contains(body, `hidden><svg class="icon" aria-hidden="true"><use href="#i-alert"/>`) {
		t.Fatal("полоса без проблем не скрыта")
	}
}
