package hclocal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"hcgateway/internal/appliance"
	"hcgateway/internal/tlspsk"
)

// Образцы повторяют структуру файлов описания, номера и значения выдуманы.
const testMapping = `<?xml version="1.0" encoding="UTF-8"?>
<featureMappingFile>
  <featureDescription>
    <feature refUID="0100">BSH.Common.Root.SelectedProgram</feature>
    <feature refUID="0101">BSH.Common.Root.ActiveProgram</feature>
    <feature refUID="0201">BSH.Common.Status.DoorState</feature>
    <feature refUID="0202">BSH.Common.Status.OperationState</feature>
    <feature refUID="0203">BSH.Common.Status.RemoteControlStartAllowed</feature>
    <feature refUID="0301">BSH.Common.Setting.PowerState</feature>
    <feature refUID="0401">BSH.Common.Event.ProgramFinished</feature>
    <feature refUID="0501">BSH.Common.Command.AbortProgram</feature>
    <feature refUID="0502">BSH.Common.Command.PauseProgram</feature>
    <feature refUID="0601">BSH.Common.Option.StartInRelative</feature>
    <feature refUID="2001">Dishcare.Dishwasher.Program.Eco50</feature>
    <feature refUID="2002">Dishcare.Dishwasher.Program.Quick45</feature>
  </featureDescription>
  <enumDescriptionList>
    <enumDescription refENID="0001" enumKey="BSH.Common.EnumType.DoorState">
      <enumMember refValue="0">Open</enumMember>
      <enumMember refValue="1">Closed</enumMember>
    </enumDescription>
    <enumDescription refENID="0002" enumKey="BSH.Common.EnumType.OperationState">
      <enumMember refValue="0">Inactive</enumMember>
      <enumMember refValue="1">Ready</enumMember>
      <enumMember refValue="3">Run</enumMember>
      <enumMember refValue="6">Finished</enumMember>
    </enumDescription>
    <enumDescription refENID="0003" enumKey="BSH.Common.EnumType.PowerState">
      <enumMember refValue="1">Off</enumMember>
      <enumMember refValue="2">On</enumMember>
    </enumDescription>
    <enumDescription refENID="0004" enumKey="BSH.Common.EnumType.EventPresentState">
      <enumMember refValue="0">Off</enumMember>
      <enumMember refValue="1">Present</enumMember>
      <enumMember refValue="2">Confirmed</enumMember>
    </enumDescription>
  </enumDescriptionList>
</featureMappingFile>`

const testDescription = `<?xml version="1.0" encoding="UTF-8"?>
<device>
  <statusList uid="0010">
    <status uid="0201" refCID="03" refDID="80" access="read" available="true" enumerationType="0001"/>
    <status uid="0202" refCID="03" refDID="80" access="read" available="true" enumerationType="0002"/>
    <status uid="0203" refCID="01" refDID="00" access="read" available="true"/>
  </statusList>
  <settingList uid="0011">
    <setting uid="0301" refCID="03" refDID="80" access="readWrite" available="true" enumerationType="0003"/>
  </settingList>
  <eventList uid="0012">
    <event uid="0401" refCID="03" refDID="80" enumerationType="0004"/>
  </eventList>
  <commandList uid="0013">
    <command uid="0501" refCID="01" refDID="00" access="writeOnly"/>
    <command uid="0502" refCID="01" refDID="00" access="writeOnly"/>
  </commandList>
  <optionList uid="0014">
    <option uid="0601" refCID="10" refDID="82" access="readWrite" min="0" max="86340" stepSize="60"/>
  </optionList>
  <programGroup uid="0015">
    <program uid="2001"><option refUID="0601"/></program>
    <program uid="2002"/>
  </programGroup>
  <selectedProgram uid="0100"/>
  <activeProgram uid="0101"/>
</device>`

const (
	uidSelected = 0x100
	uidActive   = 0x101
	uidDoor     = 0x201
	uidOp       = 0x202
	uidRemote   = 0x203
	uidPower    = 0x301
	uidFinished = 0x401
	uidAbort    = 0x501
	uidStartIn  = 0x601
	uidEco      = 0x2001
)

const testHaID = "SIEMENS-SN658X06TE-000000000001"

var (
	testPSK = bytes.Repeat([]byte{0x42, 0x17}, 16)
	testIV  = bytes.Repeat([]byte{0x24}, 16)
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeFiles кладёт в каталог прибора то, что сохраняет fetch-keys.
func writeFiles(t *testing.T, mode string, psk []byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), testHaID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	kf := keyFile{Info: Info{HaID: testHaID, Brand: "Siemens", HaType: "Dishwasher", Mode: mode},
		Key: base64.RawURLEncoding.EncodeToString(psk)}
	if mode == ModeAES {
		kf.IV = base64.RawURLEncoding.EncodeToString(testIV)
	}
	data, _ := json.Marshal(kf)
	for name, content := range map[string][]byte{keyFileName: data, mappingFile: []byte(testMapping),
		descriptionFile: []byte(testDescription)} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fake - поддельный прибор: WebSocket /homeconnect по TLS-PSK или с шифрованием AES.
type fake struct {
	t    *testing.T
	mode string
	addr string

	mu       sync.Mutex
	ws       *websocket.Conn
	codec    *aesCodec
	values   map[int]any
	received []*message
	origins  []string
	connects int
	// reject - адрес, запросы к которому прибор отклоняет кодом 400. silent - прибор не отвечает на ping.
	reject string
	silent bool
	// tlsErrs - ошибки рукопожатия TLS-PSK.
	tlsErrs []error
}

func newFake(t *testing.T, mode string) *fake {
	t.Helper()
	f := &fake{t: t, mode: mode, values: map[int]any{uidDoor: 1, uidOp: 1, uidRemote: 1, uidPower: 2,
		uidSelected: uidEco, uidActive: 0}}
	mux := http.NewServeMux()
	mux.HandleFunc("/homeconnect", f.serve)
	if mode == ModeAES || mode == ModePlain {
		hs := httptest.NewServer(mux)
		t.Cleanup(hs.Close)
		f.addr = strings.TrimPrefix(hs.URL, "http://")
		return f
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(&pskListener{Listener: ln, f: f})
	t.Cleanup(func() { srv.Close() })
	f.addr = ln.Addr().String()
	return f
}

// pskListener выполняет рукопожатие TLS-PSK прибора до того, как соединение получит HTTP-сервер.
type pskListener struct {
	net.Listener
	f *fake
}

func (l *pskListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		tc := tlspsk.Server(c, tlspsk.ServerConfig{Identity: identity, PSK: testPSK})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = tc.Handshake(ctx)
		cancel()
		if err == nil {
			return tc, nil
		}
		c.Close()
		l.f.mu.Lock()
		l.f.tlsErrs = append(l.f.tlsErrs, err)
		l.f.mu.Unlock()
	}
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	var codec *aesCodec
	if f.mode == ModeAES {
		codec, _ = newAESCodec(testPSK, testIV, true)
	}
	f.mu.Lock()
	f.ws, f.codec = ws, codec
	f.connects++
	f.origins = append(f.origins, r.Header.Get("Origin"))
	if f.silent {
		ws.SetPingHandler(func(string) error { return nil })
	}
	f.mu.Unlock()
	f.send(ws, map[string]any{"sID": 7, "msgID": 100, "resource": "/ei/initialValues", "version": 2, "action": "POST",
		"data": []any{map[string]any{"edMsgID": 500}}})
	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if codec != nil {
			if typ != websocket.BinaryMessage {
				f.t.Error("в режиме AES пришло текстовое сообщение")
				return
			}
			if data, err = codec.open(data); err != nil {
				f.t.Errorf("сообщение шлюза не расшифровано: %v", err)
				return
			}
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			f.t.Errorf("сообщение шлюза не разобрано: %v", err)
			return
		}
		f.handle(ws, &m)
	}
}

func (f *fake) send(ws *websocket.Conn, m map[string]any) {
	data, _ := json.Marshal(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if ws != f.ws {
		return
	}
	typ := websocket.TextMessage
	if f.codec != nil {
		data, typ = f.codec.seal(data), websocket.BinaryMessage
	}
	ws.WriteMessage(typ, data)
}

func (f *fake) respond(ws *websocket.Conn, m *message, data ...any) {
	out := map[string]any{"sID": m.SID, "msgID": m.MsgID, "resource": m.Resource, "version": m.Version, "action": "RESPONSE"}
	if data != nil {
		out["data"] = data
	}
	f.send(ws, out)
}

// notify сообщает шлюзу изменения значений.
func (f *fake) notify(values map[int]any) {
	f.mu.Lock()
	ws := f.ws
	var data []any
	for uid, v := range values {
		f.values[uid] = v
		data = append(data, map[string]any{"uid": uid, "value": v})
	}
	f.mu.Unlock()
	f.send(ws, map[string]any{"sID": 7, "msgID": 900, "resource": "/ro/values", "version": 1, "action": "NOTIFY", "data": data})
}

func (f *fake) handle(ws *websocket.Conn, m *message) {
	f.mu.Lock()
	f.received = append(f.received, m)
	reject := f.reject == m.Resource
	f.mu.Unlock()
	if reject {
		f.send(ws, map[string]any{"sID": m.SID, "msgID": m.MsgID, "resource": m.Resource, "version": m.Version,
			"action": "RESPONSE", "code": 400})
		return
	}
	switch m.Action + " " + m.Resource {
	case "GET /ci/services":
		f.respond(ws, m, map[string]any{"service": "ci", "version": 3}, map[string]any{"service": "ei", "version": 2},
			map[string]any{"service": "iz", "version": 1}, map[string]any{"service": "ni", "version": 1},
			map[string]any{"service": "ro", "version": 2})
	case "GET /iz/info", "GET /ni/info":
		f.respond(ws, m, map[string]any{})
	case "GET /ro/allMandatoryValues":
		f.mu.Lock()
		var data []any
		for uid, v := range f.values {
			data = append(data, map[string]any{"uid": uid, "value": v})
		}
		f.mu.Unlock()
		f.respond(ws, m, data...)
	case "GET /ro/allDescriptionChanges", "POST /ro/selectedProgram":
		f.respond(ws, m)
	case "POST /ro/activeProgram":
		f.respond(ws, m)
		f.notify(map[int]any{uidOp: 3, uidActive: uidEco})
	case "POST /ro/values":
		f.respond(ws, m)
		var it struct {
			UID int `json:"uid"`
		}
		if len(m.Data) > 0 && json.Unmarshal(m.Data[0], &it) == nil && it.UID == uidAbort {
			f.notify(map[int]any{uidOp: 1, uidActive: 0})
		}
	}
}

// last - последний запрос шлюза к адресу resource и его данные.
func (f *fake) last(resource string) (*message, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.received) - 1; i >= 0; i-- {
		if m := f.received[i]; m.Resource == resource {
			var data map[string]any
			if len(m.Data) > 0 {
				json.Unmarshal(m.Data[0], &data)
			}
			return m, data
		}
	}
	return nil, nil
}

// drop обрывает соединение с шлюзом.
func (f *fake) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ws != nil {
		f.ws.Close()
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// run запускает прибор и возвращает события уведомлений и остановку.
func run(t *testing.T, a *Appliance) (func() []string, func()) {
	t.Helper()
	var mu sync.Mutex
	var events []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx, func(ev []string) {
			mu.Lock()
			events = append(events, ev...)
			mu.Unlock()
		})
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}, stop
}

func TestAppliance(t *testing.T) {
	for _, mode := range []string{ModeTLS, ModeAES} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t, mode)
			a, err := Open("dish", writeFiles(t, mode, testPSK), testHaID, f.addr, discard())
			if err != nil {
				t.Fatal(err)
			}
			d := a.Describe()
			if d.Type != "Dishwasher" || !d.HasProgram("Dishcare.Dishwasher.Program.Eco50") || !d.HasCommand(commandAbort) {
				t.Fatalf("описание: %+v", d)
			}
			if o := d.Options["BSH.Common.Option.StartInRelative"]; o.Max != 86340 || o.Step != 60 {
				t.Fatalf("параметр: %+v", o)
			}
			if o := d.Options[appliance.SettingPowerState]; !slices.Equal(o.Values, []string{appliance.PowerOff, appliance.PowerOn}) {
				t.Fatalf("настройка: %+v", o)
			}
			events, stop := run(t, a)
			eventually(t, "связь", func() bool { return a.State().Online })
			st := a.State()
			if st.Values[appliance.StatusDoorState] != appliance.DoorClosed || st.Values[appliance.StatusRemoteStartAllowed] != true ||
				st.Values[appliance.StatusOperationState] != appliance.OperationReady || st.SelectedProgram != "Dishcare.Dishwasher.Program.Eco50" {
				t.Fatalf("состояние: %+v", st)
			}
			if _, reply := f.last("/ei/initialValues"); reply["deviceType"] != "Application" || reply["deviceName"] != deviceName {
				t.Fatalf("ответ на /ei/initialValues: %v", reply)
			}
			wantOrigin := "http://" + f.addr
			if mode == ModeTLS {
				wantOrigin = "https://" + f.addr
			}
			f.mu.Lock()
			origin := f.origins[0]
			f.mu.Unlock()
			if origin != wantOrigin {
				t.Fatalf("Origin: %q", origin)
			}

			ctx := context.Background()
			if err := a.Start(ctx, "Dishcare.Dishwasher.Program.Eco50", map[string]any{"BSH.Common.Option.StartInRelative": 600.0}); err != nil {
				t.Fatal(err)
			}
			m, data := f.last("/ro/activeProgram")
			opts, _ := data["options"].([]any)
			if m.Version != 2 || data["program"] != float64(uidEco) || len(opts) != 1 ||
				opts[0].(map[string]any)["uid"] != float64(uidStartIn) || opts[0].(map[string]any)["value"] != float64(600) {
				t.Fatalf("запуск: версия %d, %v", m.Version, data)
			}
			eventually(t, "программа идёт", func() bool {
				st := a.State()
				return st.Running() && st.ActiveProgram == "Dishcare.Dishwasher.Program.Eco50"
			})

			f.notify(map[int]any{uidFinished: 1})
			eventually(t, "событие", func() bool { return slices.Contains(events(), "BSH.Common.Event.ProgramFinished") })

			if err := a.Set(ctx, appliance.SettingPowerState, appliance.PowerOff); err != nil {
				t.Fatal(err)
			}
			if _, data := f.last("/ro/values"); data["uid"] != float64(uidPower) || data["value"] != float64(1) {
				t.Fatalf("настройка: %v", data)
			}
			if err := a.Set(ctx, appliance.SettingPowerState, "BSH.Common.EnumType.PowerState.Bogus"); !errors.Is(err, appliance.ErrInvalidValue) {
				t.Fatalf("чужое значение: %v", err)
			}
			if err := a.Set(ctx, appliance.StatusDoorState, appliance.DoorOpen); !errors.Is(err, appliance.ErrNotSupported) {
				t.Fatalf("состояние только для чтения: %v", err)
			}
			if err := a.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			if _, data := f.last("/ro/values"); data["uid"] != float64(uidAbort) || data["value"] != true {
				t.Fatalf("остановка: %v", data)
			}
			eventually(t, "программа остановлена", func() bool { return !a.State().Running() && a.State().ActiveProgram == "" })

			f.mu.Lock()
			f.reject = "/ro/selectedProgram"
			f.mu.Unlock()
			if err := a.Select(ctx, "Dishcare.Dishwasher.Program.Quick45"); !errors.Is(err, appliance.ErrWrongMode) {
				t.Fatalf("отклонённый выбор: %v", err)
			}

			// Обрыв: прибор не на связи, затем шлюз переподключается сам.
			f.drop()
			eventually(t, "обрыв", func() bool { return !a.State().Online })
			if err := a.Command(ctx, "BSH.Common.Command.PauseProgram"); !errors.Is(err, appliance.ErrUnreachable) {
				t.Fatalf("команда без связи: %v", err)
			}
			eventually(t, "переподключение", func() bool { return a.State().Online })
			stop()
			if a.State().Online {
				t.Fatal("прибор на связи после остановки")
			}
		})
	}
}

func TestSilent(t *testing.T) {
	every, idle := pingEvery, readTimeout
	pingEvery, readTimeout = 50*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { pingEvery, readTimeout = every, idle })
	f := newFake(t, ModeTLS)
	a, err := Open("dish", writeFiles(t, ModeTLS, testPSK), testHaID, f.addr, discard())
	if err != nil {
		t.Fatal(err)
	}
	_, stop := run(t, a)
	eventually(t, "связь", func() bool { return a.State().Online })
	// Прибор отвечает на ping: соединение держится дольше readTimeout.
	time.Sleep(3 * readTimeout)
	if !a.State().Online {
		t.Fatal("соединение оборвалось при ответах на ping")
	}
	f.mu.Lock()
	f.silent = true
	f.mu.Unlock()
	f.drop()
	eventually(t, "второе соединение", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.connects >= 2
	})
	eventually(t, "обрыв молчащего соединения", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.connects >= 3
	})
	stop()
}

func TestWrongKey(t *testing.T) {
	f := newFake(t, ModeTLS)
	a, err := Open("dish", writeFiles(t, ModeTLS, []byte("чужой ключ")), testHaID, f.addr, discard())
	if err != nil {
		t.Fatal(err)
	}
	_, stop := run(t, a)
	eventually(t, "отказ рукопожатия", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.tlsErrs) > 0
	})
	stop()
	if a.State().Online {
		t.Fatal("прибор на связи с чужим ключом")
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open("dish", filepath.Join(t.TempDir(), testHaID), testHaID, "127.0.0.1", discard()); !errors.Is(err, errNoKeys) {
		t.Fatalf("без ключей: %v", err)
	}
	dir := writeFiles(t, ModeTLS, testPSK)
	if _, err := Open("dish", dir, "OTHER-HAID", "127.0.0.1", discard()); err == nil || !strings.Contains(err.Error(), "haId") {
		t.Fatalf("чужой haId: %v", err)
	}
	for name, kf := range map[string]keyFile{
		"неизвестный режим": {Info: Info{HaID: testHaID, HaType: "Dishwasher", Mode: "ssl"}, Key: "AAAA"},
		"AES без iv":        {Info: Info{HaID: testHaID, HaType: "Dishwasher", Mode: ModeAES}, Key: "AAAA"},
		"пустой ключ":       {Info: Info{HaID: testHaID, HaType: "Dishwasher", Mode: ModeTLS}},
		"без типа":          {Info: Info{HaID: testHaID, Mode: ModeTLS}, Key: "AAAA"},
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(kf)
			os.WriteFile(filepath.Join(dir, keyFileName), data, 0o600)
			if _, err := Open("dish", dir, testHaID, "127.0.0.1", discard()); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
	if list := List(filepath.Dir(dir)); len(list) != 1 || list[0].HaID != testHaID {
		t.Fatalf("список: %+v", list)
	}
}

func TestAESCodec(t *testing.T) {
	app, _ := newAESCodec(testPSK, testIV, false)
	dev, _ := newAESCodec(testPSK, testIV, true)
	// Цепочка CBC и HMAC идёт из сообщения в сообщение в обе стороны; длины - вокруг границ блока.
	for n := range 40 {
		msg := bytes.Repeat([]byte{'x'}, n)
		got, err := dev.open(app.seal(msg))
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("к прибору, %d байт: %v", n, err)
		}
		if got, err = app.open(dev.seal(msg)); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("от прибора, %d байт: %v", n, err)
		}
	}
	sealed := app.seal([]byte(`{"action":"GET"}`))
	sealed[3] ^= 1
	if _, err := dev.open(sealed); err == nil {
		t.Fatal("испорченное сообщение принято")
	}
	other, _ := newAESCodec([]byte("чужой ключ"), testIV, true)
	if _, err := other.open(app.seal([]byte("{}"))); err == nil {
		t.Fatal("сообщение с чужим ключом принято")
	}
}
