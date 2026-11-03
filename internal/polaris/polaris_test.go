package polaris

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/mqttbroker"
	"hcgateway/internal/profile"
	"hcgateway/internal/statedb"
)

const (
	testID       = "aabbccddeeff"
	testPassword = "0123456789abcdef"
)

// kettleState - что чайник публикует при подключении: значения сняты с PWK-1775CGLD тип 294.
var kettleState = map[string]string{"acl_enable": "false", "backlight": "true", "child_lock": "false", "devtype": "294",
	"diag/mcu_err": "0", "diag/rssi": "-47", "error/code": "00", "error/connection": "false", "error/empty_base": "false",
	"firmware": "0.8", "mac": testID, "mcu_firmware": "2.0.6", "mode": "0", "program_data/1": "00", "program_data/2": "05",
	"program_data/3": "00020000", "sensor/temperature": "31.00", "smart_button": "d", "sound": "true", "temperature": "0",
	"timezone": "180", "weight": "1100"}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("не дождались: %s", what)
		}
	}
}

// fixture - брокер, приборы Polaris и поддельный чайник на настоящем клиенте MQTT.
type fixture struct {
	t      *testing.T
	hub    *Hub
	broker *mqttbroker.Broker
	addr   string
	kettle paho.Client

	mu       sync.Mutex
	commands []string
}

func newFixture(t *testing.T, dir string, state *statedb.DB) *fixture {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	f := &fixture{t: t, hub: New(dir, discard()), addr: addr}
	f.broker = mqttbroker.New(mqttbroker.Config{Listen: addr, Credentials: func() (string, string) { return "hcgateway", testPassword }}, state, discard())
	f.hub.Attach(f.broker)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.broker.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "брокер", func() bool { return f.broker.Status("").Open })
	return f
}

// connect подключает поддельный чайник: он слушает control и публикует kettleState.
func (f *fixture) connect() {
	f.t.Helper()
	o := paho.NewClientOptions().AddBroker("tcp://" + f.addr).SetClientID(testID).SetUsername("hcgateway").SetPassword(testPassword).SetAutoReconnect(false)
	f.kettle = paho.NewClient(o)
	if tok := f.kettle.Connect(); tok.Wait() && tok.Error() != nil {
		f.t.Fatal(tok.Error())
	}
	f.t.Cleanup(func() { f.kettle.Disconnect(10) })
	if tok := f.kettle.Subscribe("polaris/"+testID+"/control/#", 0, func(_ paho.Client, m paho.Message) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.commands = append(f.commands, strings.TrimPrefix(m.Topic(), "polaris/"+testID+"/control/")+"="+string(m.Payload()))
	}); tok.Wait() && tok.Error() != nil {
		f.t.Fatal(tok.Error())
	}
	for k, v := range kettleState {
		f.state(k, v)
	}
}

func (f *fixture) state(key, value string) {
	f.t.Helper()
	if tok := f.kettle.Publish("polaris/"+testID+"/state/"+key, 1, true, value); tok.Wait() && tok.Error() != nil {
		f.t.Fatal(tok.Error())
	}
}

// expect ждёт команды чайнику в этом порядке и забывает их.
func (f *fixture) expect(want ...string) {
	f.t.Helper()
	waitFor(f.t, strings.Join(want, ", "), func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.commands, want)
	})
	f.mu.Lock()
	f.commands = nil
	f.mu.Unlock()
}

func action(capability, instance string, value any) *alice.Action {
	return &alice.Action{Type: capability, State: alice.ActionState{Instance: instance, Value: value}}
}

// Чайник как устройство Алисы: тип, умения и команды уходят в нужные топики, а значения приходят.
func TestKettleInAlice(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan []string, 16)
	go a.Run(ctx, func(e []string) {
		select {
		case events <- e:
		default:
		}
	})
	if a.State().Online || a.Describe().Type != "" {
		t.Fatal("чайник на связи до подключения")
	}
	f.connect()
	waitFor(t, "значения", func() bool { st := a.State(); return st.Online && st.Values["sensor/temperature"] == 31.0 })
	if a.Describe().Type != profile.PolarisKettleType {
		t.Fatalf("тип: %q", a.Describe().Type)
	}
	waitFor(t, "все значения", func() bool {
		return a.State().Values["weight"] == 1100.0 && a.State().Values["program_data/2"] == "05" && a.State().Values["backlight"] == true
	})
	if st := a.State(); st.Values[KeyMode] != "0" || st.Values[KeyWaterAlarm] != appliance.EventOff {
		t.Fatalf("значения: %v", st.Values)
	}

	p, err := profile.Resolve(profile.PolarisKettleType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &hcdevice.Device{ID: "kettle", Name: "Чайник", Profile: p, Appliance: a, Manufacturer: "Polaris"}
	info := d.Info()
	if len(info) != 2 || info[0].Type != alice.TypePrefix+"cooking.kettle" {
		t.Fatalf("устройства: %+v", info)
	}
	caps := map[string]bool{}
	for _, c := range info[0].Capabilities {
		caps[c.Type] = true
	}
	for _, want := range []string{alice.CapOnOff, alice.CapMode, alice.CapRange, alice.CapToggle} {
		if !caps[want] {
			t.Fatalf("нет умения %s: %+v", want, info[0].Capabilities)
		}
	}

	act := func(a *alice.Action) {
		t.Helper()
		if code := d.Act(context.Background(), "kettle", a); code != "" {
			t.Fatalf("команда %+v: %s", a, code)
		}
	}
	act(action(alice.CapOnOff, "on", true))
	f.expect("mode=1")
	f.state("mode", "1")
	waitFor(t, "режим", func() bool { return a.State().Values[KeyMode] == "1" })
	act(action(alice.CapOnOff, "on", false))
	f.expect("mode=0")
	f.state("mode", "0")
	waitFor(t, "выключение", func() bool { return a.State().Values[KeyMode] == "0" })
	act(action(alice.CapRange, "temperature", 80.0))
	f.expect("temperature=80", "mode=3")
	act(action(alice.CapMode, "tea_mode", "white_tea"))
	f.expect("temperature=65", "mode=3")
	if a.State().Values[KeyTea] != "white" {
		t.Fatalf("чай: %v", a.State().Values[KeyTea])
	}
	act(action(alice.CapToggle, "backlight", false))
	f.expect("backlight=false")
	if code := d.Act(context.Background(), "kettle", action(alice.CapRange, "temperature", 150.0)); code == "" {
		t.Fatal("температура 150 принята")
	}

	// Мало воды и снятие с подставки.
	water := func() any {
		for _, ds := range d.States() {
			for _, p := range ds.Properties {
				if sv, ok := p.State.(alice.StateValue); ok && sv.Instance == "water_level" {
					return sv.Value
				}
			}
		}
		return nil
	}
	if got := water(); got != "normal" {
		t.Fatalf("уровень воды: %v", got)
	}
	f.state("error/code", "01")
	waitFor(t, "мало воды", func() bool { return water() == "low" })
	f.state("error/empty_base", "true")
	waitFor(t, "подставка", func() bool { return a.State().Values["error/empty_base"] == true })

	// Клиент отключился: прибор без связи, команд нет.
	f.kettle.Disconnect(10)
	waitFor(t, "потеря связи", func() bool { return !a.State().Online })
	if code := d.Act(context.Background(), "kettle", action(alice.CapOnOff, "on", true)); code != alice.CodeUnreachable {
		t.Fatalf("без связи: %q", code)
	}
}

// Разбор топиков: новые и старые прошивки, чужие топики и опасные идентификаторы.
func TestParseTopic(t *testing.T) {
	for topic, want := range map[string][2]string{
		"polaris/aabb/state/mode":               {"aabb", "mode"},
		"polaris/aabb/state/sensor/temperature": {"aabb", "sensor/temperature"},
		"polaris/2/aabb/state/mode":             {"2/aabb", "mode"},
	} {
		id, key, ok := parseTopic(topic)
		if !ok || id != want[0] || key != want[1] {
			t.Fatalf("%s: %q %q %v", topic, id, key, ok)
		}
	}
	for _, topic := range []string{"polaris/aabb/control/mode", "wled/x/state/mode", "polaris/../state/mode", "polaris/a b/state/x", "polaris/aabb/state"} {
		if _, _, ok := parseTopic(topic); ok {
			t.Fatalf("%s разобран", topic)
		}
	}
}

// После перезапуска чайник остаётся в Алисе по типу из файла, но без связи: сохранённые сообщения брокера
// не считаются текущим состоянием. Связь появляется только с его сообщением.
func TestRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := t.TempDir() + "/" + statedb.FileName
	state := statedb.Open(statePath, discard())
	f := newFixture(t, dir, state)
	a := f.hub.Appliance(testID)
	f.connect()
	waitFor(t, "тип", func() bool { return a.Describe().Type == profile.PolarisKettleType })
	waitFor(t, "сохранение", func() bool { return state.RetainedCount() == len(kettleState) })
	f.state("mode", "1")
	waitFor(t, "кипячение", func() bool { return a.State().Values[KeyMode] == "1" })
	f.kettle.Disconnect(10)
	state.Close()

	state = statedb.Open(statePath, discard())
	defer state.Close()
	f = newFixture(t, dir, state)
	a = f.hub.Appliance(testID)
	if st := a.State(); st.Online || len(st.Values) != 0 || a.Describe().Type != profile.PolarisKettleType {
		t.Fatalf("после перезапуска: %+v тип %q", st, a.Describe().Type)
	}
	if !slices.Contains(a.Describe().Reports, "backlight") {
		t.Fatalf("значения из файла: %v", a.Describe().Reports)
	}
	if err := a.Set(context.Background(), KeyMode, "1"); err == nil {
		t.Fatal("команда без связи прошла")
	}
	f.connect()
	waitFor(t, "связь", func() bool { return a.State().Online })
}

// Найденные приборы: чайник и незнакомый тип; поток приборов не бесконечен.
func TestFound(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	f.connect()
	waitFor(t, "находка", func() bool {
		l := f.hub.Found()
		return len(l) == 1 && l[0].ID == testID && l[0].DevType == "294" && l[0].Client == testID
	})
	if !Kettle("294") || Kettle("999") {
		t.Fatal("чайник определён неверно")
	}
	for i := range maxFound + 5 {
		f.hub.message(mqttbroker.Message{Client: "x", Topic: fmt.Sprintf("polaris/dev%d/state/devtype", i), Payload: []byte("999")})
	}
	if n := len(f.hub.Found()); n != maxFound {
		t.Fatalf("находок: %d", n)
	}
	unknown := f.hub.Appliance("dev1")
	f.hub.message(mqttbroker.Message{Client: "x", Topic: "polaris/dev1/state/devtype", Payload: []byte("999")})
	waitFor(t, "тип", func() bool { return unknown.Describe().Type == "Polaris999" })
}
