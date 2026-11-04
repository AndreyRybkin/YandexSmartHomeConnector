package polaris

import (
	"context"
	"errors"
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
	// Уровень воды: вес воды в проценты от полного чайника, за границами - граница.
	level := func(weight float64) float64 {
		t.Helper()
		for i := range p.Properties {
			if pr := &p.Properties[i]; pr.Instance == "water_level" && pr.Status == "weight" {
				return pr.Value(weight)
			}
		}
		t.Fatalf("нет уровня воды: %+v", p.Properties)
		return 0
	}
	for weight, want := range map[float64]float64{0: 0, 850: 50, 1630: 95.88, 1700: 100, 1800: 100, -10: 0} {
		if got := level(weight); got != want {
			t.Errorf("вес %v: уровень %v, нужно %v", weight, got, want)
		}
	}
	d := &hcdevice.Device{ID: "kettle", Name: "Чайник", Profile: p, Appliance: a, Manufacturer: "Polaris"}
	info := d.Info()
	if len(info) != 1 || info[0].Type != alice.TypePrefix+"cooking.kettle" {
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

	// Уровень воды у чайника двумя свойствами: число - проценты, событие - мало или нормально.
	water := func(kind string) any {
		for _, ds := range d.States() {
			for _, p := range ds.Properties {
				if sv, ok := p.State.(alice.StateValue); ok && p.Type == kind && sv.Instance == "water_level" {
					return sv.Value
				}
			}
		}
		return nil
	}
	if got := water(alice.PropFloat); got != 64.71 {
		t.Fatalf("уровень воды в процентах: %v", got)
	}
	if got := water(alice.PropEvent); got != "normal" {
		t.Fatalf("уровень воды: %v", got)
	}
	f.state("error/code", "01")
	waitFor(t, "мало воды", func() bool { return water(alice.PropEvent) == "low" })
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

// Цель 0 в простое не уходит Алисе; шаг без цели считается от температуры воды.
func TestKettleNoTarget(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	f.connect()
	waitFor(t, "значения", func() bool { return a.State().Values[KeySensor] == 31.0 })
	if v, ok := a.State().Values[KeyTemperature]; ok {
		t.Fatalf("цель в простое: %v", v)
	}
	p, err := profile.Resolve(profile.PolarisKettleType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &hcdevice.Device{ID: "kettle", Profile: p, Appliance: a}
	for _, ds := range d.States() {
		for _, c := range ds.Capabilities {
			if sv, ok := c.State.(alice.StateValue); ok && sv.Instance == "temperature" {
				t.Fatalf("цель в состоянии Алисы: %v", sv.Value)
			}
		}
	}
	step := action(alice.CapRange, "temperature", 10.0)
	step.State.Relative = true
	if code := d.Act(context.Background(), "kettle", step); code != "" {
		t.Fatalf("шаг: %s", code)
	}
	f.expect("temperature=41", "mode=3")
	step.State.Value = 90.0
	if code := d.Act(context.Background(), "kettle", step); code != alice.CodeInvalidValue {
		t.Fatalf("шаг за предел: %q", code)
	}
	f.state(KeyTemperature, "80")
	waitFor(t, "цель", func() bool { return a.State().Values[KeyTemperature] == 80.0 })
}

// Конец работы - только если чайник выключился сам: не снят с базы и не выключен командой шлюза.
func TestKettleFinished(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	a.finishAfter = 50 * time.Millisecond
	finished := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, func(events []string) {
		if slices.Contains(events, EventBoilFinished) {
			finished <- struct{}{}
		}
	})
	waitFor(t, "подписка", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.notify != nil
	})
	const c = "client"
	expect := func(what string, want bool) {
		t.Helper()
		select {
		case <-finished:
			if !want {
				t.Fatalf("%s: конец работы", what)
			}
		case <-time.After(200 * time.Millisecond):
			if want {
				t.Fatalf("%s: нет конца работы", what)
			}
		}
	}
	a.message(c, KeyMode, "0")
	expect("первое значение после запуска", false)
	a.message(c, KeyMode, "1")
	a.message(c, KeyMode, "0")
	expect("чайник выключился сам", true)
	// Снят с базы: порядок сообщений не важен.
	a.message(c, KeyMode, "1")
	a.message(c, KeyEmptyBase, "true")
	a.message(c, KeyMode, "0")
	expect("база, затем режим", false)
	a.message(c, KeyEmptyBase, "false")
	a.message(c, KeyMode, "1")
	a.message(c, KeyMode, "0")
	a.message(c, KeyEmptyBase, "true")
	expect("режим, затем база", false)
	a.message(c, KeyEmptyBase, "false")
	// Снова включён за время ожидания.
	a.message(c, KeyMode, "1")
	a.message(c, KeyMode, "0")
	a.message(c, KeyMode, "1")
	expect("снова работает", false)
	// Выключен командой шлюза.
	if err := a.Set(context.Background(), KeyMode, "0"); err != nil {
		t.Fatal(err)
	}
	a.message(c, KeyMode, "0")
	expect("команда шлюза", false)
}

// У прибора другого типа числа разбираются сами, строки с ведущим нулём остаются строками.
func TestOtherTypeValues(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	a.message("c", KeyDevType, "999")
	a.message("c", "sensor/humidity", "45.5")
	a.message("c", "error/code", "00")
	a.message("c", KeyMode, "0")
	st := a.State()
	if st.Values[KeyMode] != "0" {
		t.Fatalf("режим: %#v", st.Values[KeyMode])
	}
	if st.Values["sensor/humidity"] != 45.5 || st.Values["error/code"] != "00" {
		t.Fatalf("значения: %v", st.Values)
	}
}

// Снятый с базы чайник и чайник без воды не греют, а Алиса называет причину. Выключить можно всегда.
func TestKettleRefusesHeat(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	a.message("c", KeyDevType, "294")
	a.message("c", KeyFirmware, "0.8")
	d := a.Describe()
	if d.Model != "PWK-1775CGLD" || d.Firmware != "0.8" {
		t.Fatalf("модель %q, прошивка %q", d.Model, d.Firmware)
	}
	ctx := context.Background()
	a.message("c", KeyEmptyBase, "true")
	for key, value := range map[string]any{KeyMode: "1", KeyTemperature: 80.0, KeyTea: "green"} {
		if err := a.Set(ctx, key, value); !errors.Is(err, appliance.ErrHumanNeeded) {
			t.Fatalf("%s на снятом чайнике: %v", key, err)
		}
	}
	if err := a.Set(ctx, KeyMode, "0"); errors.Is(err, appliance.ErrHumanNeeded) {
		t.Fatalf("выключение: %v", err)
	}
	a.message("c", KeyEmptyBase, "false")
	a.message("c", KeyErrorCode, "01")
	if err := a.Set(ctx, KeyMode, "1"); !errors.Is(err, appliance.ErrNotEnoughWater) {
		t.Fatalf("мало воды: %v", err)
	}
}

// Звук и режим работы чайника: mute обратен sound, режим запускает работу, у выключенного режима нет.
func TestKettleMuteAndProgram(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	f.connect()
	waitFor(t, "значения", func() bool { return a.State().Values["sound"] == true && a.State().Values[KeySensor] == 31.0 })
	p, err := profile.Resolve(profile.PolarisKettleType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &hcdevice.Device{ID: "kettle", Profile: p, Appliance: a}
	state := func(instance string) any {
		for _, ds := range d.States() {
			for _, c := range ds.Capabilities {
				if sv, ok := c.State.(alice.StateValue); ok && sv.Instance == instance {
					return sv.Value
				}
			}
		}
		return nil
	}
	if state("mute") != false || state("program") != nil {
		t.Fatalf("в простое: mute %v, program %v", state("mute"), state("program"))
	}
	if code := d.Act(context.Background(), "kettle", action(alice.CapToggle, "mute", true)); code != "" {
		t.Fatal(code)
	}
	f.expect("sound=false")
	if code := d.Act(context.Background(), "kettle", action(alice.CapMode, "program", "smart")); code != "" {
		t.Fatal(code)
	}
	f.expect("mode=5")
	f.state(KeyMode, "5")
	waitFor(t, "режим", func() bool { return state("program") == "smart" })
}

// Удержание тепла: значение считается из режима и есть у выключенного чайника, команда меняет режим,
// конец работы с удержанием - тоже конец работы.
func TestKettleKeepWarm(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	a.finishAfter = 10 * time.Millisecond
	events := make(chan []string, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, func(e []string) {
		if e != nil {
			events <- e
		}
	})
	f.connect()
	waitFor(t, "значения", func() bool { return a.State().Values["sound"] == true && a.State().Values[KeyMode] == "0" })
	p, err := profile.Resolve(profile.PolarisKettleType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &hcdevice.Device{ID: "kettle", Profile: p, Appliance: a}
	state := func() any {
		for _, ds := range d.States() {
			for _, c := range ds.Capabilities {
				if sv, ok := c.State.(alice.StateValue); ok && sv.Instance == "keep_warm" {
					return sv.Value
				}
			}
		}
		return nil
	}
	if o := a.Describe().Options[KeyKeepWarm]; !o.Setting || !slices.Contains(a.Describe().Reports, KeyKeepWarm) {
		t.Fatalf("настройка %+v, значения %v", o, a.Describe().Reports)
	}
	if state() != false {
		t.Fatalf("выключенный чайник: %v", state())
	}
	act := func(on bool) {
		t.Helper()
		if code := d.Act(ctx, "kettle", action(alice.CapToggle, "keep_warm", on)); code != "" {
			t.Fatal(code)
		}
	}
	mode := func(m string) {
		t.Helper()
		f.state(KeyMode, m)
		waitFor(t, "режим "+m, func() bool { return a.State().Values[KeyMode] == m })
	}
	// Выключенный чайник получает кипячение с удержанием, а снять нечего.
	act(false)
	act(true)
	f.expect("mode=2")
	mode("2")
	if state() != true {
		t.Fatalf("режим 2: %v", state())
	}
	act(true)
	act(false)
	f.expect("mode=1")
	mode("1")
	if state() != false {
		t.Fatalf("режим 1: %v", state())
	}
	act(true)
	f.expect("mode=2")
	mode("3")
	act(true)
	f.expect("mode=4")
	mode("4")
	if state() != true {
		t.Fatalf("режим 4: %v", state())
	}
	act(false)
	f.expect("mode=3")
	// Режимы 5 и 6 удержания не знают: включить нельзя, снимать нечего.
	mode("5")
	if err := a.Set(ctx, KeyKeepWarm, true); !errors.Is(err, appliance.ErrWrongMode) {
		t.Fatalf("режим 5: %v", err)
	}
	act(false)
	if err := a.Set(ctx, KeyKeepWarm, "да"); !errors.Is(err, appliance.ErrInvalidValue) {
		t.Fatalf("не флаг: %v", err)
	}
	// Конец работы с удержанием: режим 2 переходит в 0.
	mode("2")
	mode("0")
	select {
	case e := <-events:
		if !slices.Contains(e, EventBoilFinished) {
			t.Fatalf("события: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("нет конца работы")
	}
	f.expect()
}

// Снятый с базы чайник и чайник без воды удержание не включают, а снять его можно всегда.
func TestKettleKeepWarmRefused(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	f.connect()
	waitFor(t, "значения", func() bool { return a.State().Values["sound"] == true && a.State().Values[KeyMode] == "0" })
	ctx := context.Background()
	f.state(KeyEmptyBase, "true")
	waitFor(t, "снят с базы", func() bool { return a.State().Values[KeyEmptyBase] == true })
	if err := a.Set(ctx, KeyKeepWarm, true); !errors.Is(err, appliance.ErrHumanNeeded) {
		t.Fatalf("снят с базы: %v", err)
	}
	f.state(KeyEmptyBase, "false")
	f.state(KeyErrorCode, "01")
	waitFor(t, "мало воды", func() bool { return a.State().Values[KeyWaterAlarm] == appliance.EventPresent })
	if err := a.Set(ctx, KeyKeepWarm, true); !errors.Is(err, appliance.ErrNotEnoughWater) {
		t.Fatalf("мало воды: %v", err)
	}
	f.state(KeyMode, "2")
	waitFor(t, "режим 2", func() bool { return a.State().Values[KeyMode] == "2" })
	if err := a.Set(ctx, KeyKeepWarm, false); err != nil {
		t.Fatalf("снятие удержания: %v", err)
	}
	f.expect("mode=1")
}

// Удержание тепла только у чайника: у другого прибора функции нет и команда её не отправляет.
func TestKeepWarmOnlyKettle(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	a.message("c", KeyDevType, "1000")
	a.message("c", KeyMode, "2")
	d := a.Describe()
	if _, ok := d.Options[KeyKeepWarm]; ok || slices.Contains(d.Reports, KeyKeepWarm) {
		t.Fatalf("описание: %+v", d)
	}
	if _, ok := a.State().Values[KeyKeepWarm]; ok {
		t.Fatalf("значения: %v", a.State().Values)
	}
	if err := a.Set(context.Background(), KeyKeepWarm, true); !errors.Is(err, appliance.ErrNotSupported) {
		t.Fatalf("команда: %v", err)
	}
}

// Пустой чайник: код 2 даёт уровень empty и отказ в нагреве, код 0 после доливки снимает оба.
func TestKettleEmpty(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	a.message("c", KeyDevType, "294")
	p, err := profile.Resolve(profile.PolarisKettleType, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &hcdevice.Device{ID: "kettle", Profile: p, Appliance: a}
	level := func() any {
		for _, ds := range d.States() {
			for _, c := range ds.Properties {
				if sv, ok := c.State.(alice.StateValue); ok && c.Type == alice.PropEvent && sv.Instance == "water_level" {
					return sv.Value
				}
			}
		}
		return nil
	}
	for _, code := range []string{"2", "02"} {
		a.message("c", KeyErrorCode, code)
		if level() != "empty" {
			t.Fatalf("код %s: %v", code, level())
		}
		if err := a.Set(context.Background(), KeyMode, "1"); !errors.Is(err, appliance.ErrNotEnoughWater) {
			t.Fatalf("код %s, нагрев: %v", code, err)
		}
	}
	a.message("c", KeyErrorCode, "0")
	if level() != "normal" {
		t.Fatalf("после доливки: %v", level())
	}
	if err := a.Set(context.Background(), KeyMode, "1"); errors.Is(err, appliance.ErrNotEnoughWater) {
		t.Fatalf("нагрев после доливки: %v", err)
	}
}

// Разогрев без цели не запускается; чай сбрасывается при режиме без цели.
func TestKettleHeatNeedsTarget(t *testing.T) {
	f := newFixture(t, t.TempDir(), nil)
	a := f.hub.Appliance(testID)
	a.message("c", KeyDevType, "294")
	a.message("c", KeyTemperature, "0")
	for _, mode := range []string{"3", "4"} {
		if err := a.Set(context.Background(), KeyMode, mode); !errors.Is(err, appliance.ErrWrongMode) {
			t.Fatalf("режим %s без цели: %v", mode, err)
		}
	}
	a.message("c", KeyTemperature, "80")
	a.mu.Lock()
	a.values[KeyTea] = "green"
	a.mu.Unlock()
	a.message("c", KeyMode, "3")
	if a.State().Values[KeyTea] != "green" {
		t.Fatal("чай сброшен при разогреве")
	}
	a.message("c", KeyMode, "1")
	if _, ok := a.State().Values[KeyTea]; ok {
		t.Fatal("чай остался при кипячении")
	}
}
