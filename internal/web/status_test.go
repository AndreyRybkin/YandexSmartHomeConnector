package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/delonghi/dlfake"
	"hcgateway/internal/profile"
)

// fakeDevice - устройство Алисы с заданными состояниями.
type fakeDevice struct{ states []alice.DeviceState }

func (d fakeDevice) IDs() []string                                     { return nil }
func (d fakeDevice) Info() []alice.DeviceInfo                          { return nil }
func (d fakeDevice) States() []alice.DeviceState                       { return d.states }
func (d fakeDevice) Act(context.Context, string, *alice.Action) string { return "" }
func (d fakeDevice) Run(context.Context, alice.Changed)                {}

// Заряд и «заканчивается» - свойства самого устройства.
func TestBatteryAndFood(t *testing.T) {
	dev := fakeDevice{states: []alice.DeviceState{
		{ID: "sensor", Properties: []alice.CapState{{Type: alice.PropFloat, State: alice.StateValue{Instance: "battery_level", Value: 12.0}},
			{Type: alice.PropEvent, State: alice.StateValue{Instance: "food_level", Value: "low"}}}},
	}}
	if v, ok := battery(dev, "sensor"); !ok || v != 12 {
		t.Fatalf("заряд: %v %v", v, ok)
	}
	if _, ok := battery(dev, "other"); ok {
		t.Fatal("заряд у другого устройства")
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

// Метка тревог кофемашины: первая словами и сколько ещё, сначала сбои. Уровни воды и зёрен сюда не входят.
func TestAlarmNotice(t *testing.T) {
	p, err := profile.Resolve(profile.DeLonghiType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	alarm := func(names ...string) map[string]any {
		v := map[string]any{"DeLonghi.Status.CurrMode": 7.0, "DeLonghi.Alarm.Filter": appliance.EventOff}
		for _, n := range names {
			v["DeLonghi.Alarm."+n] = appliance.EventPresent
		}
		return v
	}
	for _, c := range []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"TankLevel", "NoCoffee", "TankPresence"}, ""},
		{[]string{"Descale"}, "удалите накипь"},
		{[]string{"Descale", "TankLevel"}, "удалите накипь"},
		{[]string{"Descale", "BrokenGrinder", "Imbocco"}, "сбой кофемолки и ещё 2"},
		{[]string{"Imbocco"}, "тревога машины: Imbocco"},
		{[]string{"Новая", "Imbocco"}, "тревога машины: Imbocco и ещё 1"},
	} {
		if got := alarmNotice(p, alarm(c.names...)); got != c.want {
			t.Errorf("%v: %q, нужно %q", c.names, got, c.want)
		}
	}
	// Что значит событие в сводке: у тревоги - имя и слова вместо Present.
	for key, want := range map[string][2]string{
		"DeLonghi.Alarm.Waste=" + appliance.EventPresent:   {"Waste", "очистите контейнер гущи"},
		"DeLonghi.Alarm.Imbocco=" + appliance.EventPresent: {"Imbocco", ""},
		"DeLonghi.Alarm.TankLevel":                         {"TankLevel", "мало воды"},
		"run.state=END":                                    {"state=END", ""},
		"mode=0":                                           {"mode=0", ""},
		"BSH.Common.Event.ProgramFinished":                 {"ProgramFinished", ""},
	} {
		if label, note := eventLabel(key); label != want[0] || note != want[1] {
			t.Errorf("%s: %q, %q", key, label, note)
		}
	}
	// Кто берёт значение: раздел событий и что значит тревога.
	m := mappings(p)
	if got := m["DeLonghi.Alarm.Descale"]; got != "события: button double_click; удалите накипь" {
		t.Errorf("кто берёт Descale: %q", got)
	}
	if got := m["DeLonghi.Alarm.TankPresence"]; got != "события: water_level; бак снят" {
		t.Errorf("кто берёт TankPresence: %q", got)
	}
}

// Настоящая связь с машиной: тревога видна на плитке и в полосе «Требует внимания», а снятый бак вместе с
// ней - в той же метке. На английском - те же слова по каталогу.
func TestCoffeeAlarmOnTile(t *testing.T) {
	cloud := dlfake.NewCloud()
	defer cloud.Close()
	machine := dlfake.NewMachine(cloud)
	defer machine.Close()
	gigya, api, iot := cloud.URLs()
	f := newFixture(t, "delonghi:\n  gigya_url: "+gigya+"\n  api_url: "+api+"\n  iot_url: "+iot+"\n"+
		"devices:\n  coffee:\n    name: Кофе\n    source: delonghi\n    host: "+machine.Host()+"\n    serial: "+dlfake.Serial+"\n    pin: "+machine.Pin()+"\n")
	f.writeUser(testPassword)
	f.login()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		<-done
	}()
	go func() {
		defer close(done)
		f.gw.Run(ctx)
	}()
	f.get("/connections")
	f.post("/connections/delonghi/login", url.Values{"email": {dlfake.Email}, "password": {dlfake.Password}})
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("не дождался: %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("вход в машину", func() bool { return machine.Connected() == 1 })
	shows := func(want ...string) func() bool {
		return func() bool {
			_, body := f.get("/")
			for _, w := range want {
				if !strings.Contains(body, w) {
					return false
				}
			}
			return true
		}
	}
	status := func(alarms map[string]any) {
		machine.Status(map[string]any{"CurrMode": 7.0, "Status": "Idle", "BeverageDispensing": 0.0, "alarms": alarms})
	}
	status(map[string]any{"Descale": true, "TankLevel": false})
	wait("метка тревоги", shows(`<span class="chip warn">удалите накипь</span>`, "требуют обслуживания: 1"))
	if _, body := f.get("/devices/coffee"); !strings.Contains(body, `>Descale</code> <span class="muted">удалите накипь</span>`) {
		t.Fatal("в сводке нет подписи тревоги")
	}
	status(map[string]any{"Descale": true, "TankLevel": true})
	wait("тревога и нехватка воды", shows(`<span class="chip warn">нужно пополнить, удалите накипь</span>`))
	f.accept = "en"
	wait("метка на английском", shows(`<span class="chip warn">needs refilling, descale the machine</span>`, "need service: 1"))
	f.accept = ""
	status(map[string]any{"Descale": false, "TankLevel": true})
	wait("только нехватка воды", shows(`<span class="chip warn">нужно пополнить</span>`, "нужно пополнить: 1"))
	status(map[string]any{"Descale": false, "TankLevel": false})
	wait("метка пропала", func() bool {
		_, body := f.get("/")
		return !strings.Contains(body, `<span class="chip warn">`)
	})
}
