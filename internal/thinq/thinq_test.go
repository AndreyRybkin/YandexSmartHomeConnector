package thinq

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
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

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/metrics"
	"hcgateway/internal/profile"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testCA - удостоверяющий центр теста: подписывает сертификат брокера и клиента.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func (ca *testCA) sign(t *testing.T, pub any, tmpl *x509.Certificate) []byte {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// Профили и состояния поддельных приборов повторяют раскладку из библиотеки LG, значения выдуманы.
const (
	washerProfile = `{"property":[` +
		`{"location":{"locationName":"MAIN"},` +
		`"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["POWER_OFF","INITIAL","RUNNING","END"]}}},` +
		`"operation":{"washerOperationMode":{"type":"enum","mode":["w"],"value":{"w":["START","STOP","POWER_OFF"]}}},` +
		`"remoteControlEnable":{"remoteControlEnabled":{"type":"boolean","mode":["r"],"value":{"r":[true,false]}}}},` +
		`{"location":{"locationName":"MINI"},` +
		`"runState":{"currentState":{"type":"enum","mode":["r"],"value":{"r":["POWER_OFF","RUNNING"]}}}}],` +
		`"notification":{"push":["WASHING_IS_COMPLETE","ERROR_DURING_WASHING"]}}`
	washerState = `[{"location":{"locationName":"MINI"},"runState":{"currentState":"RUNNING"}},` +
		`{"location":{"locationName":"MAIN"},"runState":{"currentState":"INITIAL"},"remoteControlEnable":{"remoteControlEnabled":true}}]`
	acProfile = `{"property":{` +
		`"airConJobMode":{"currentJobMode":{"type":"enum","mode":["r","w"],"value":{"r":["COOL","HEAT","AIR_DRY","FAN","AUTO"],"w":["COOL","HEAT","AIR_DRY","FAN","AUTO"]}}},` +
		`"operation":{"airConOperationMode":{"type":"enum","mode":["r","w"],"value":{"r":["POWER_ON","POWER_OFF"],"w":["POWER_ON","POWER_OFF"]}}},` +
		`"temperatureInUnits":[{"unit":"C","currentTemperatureC":{"type":"number","mode":["r"]},` +
		`"targetTemperatureC":{"type":"range","mode":["r","w"],"value":{"w":{"min":18,"max":30,"step":0.5}}}},` +
		`{"unit":"F","targetTemperatureF":{"type":"range","mode":["r","w"],"value":{"w":{"min":64,"max":86,"step":1}}}}],` +
		`"airFlow":{"windStrength":{"type":"enum","mode":["r","w"],"value":{"w":["low","mid","high"]}}},` +
		`"windDirection":{"rotateUpDown":{"type":"boolean","mode":["r","w"],"value":{"w":[true,false]}}},` +
		`"airQualitySensor":{"humidity":{"type":"number","mode":["r"]}}},` +
		`"notification":{"push":["WATER_IS_FULL"]}}`
	acState = `{"airConJobMode":{"currentJobMode":"COOL"},"operation":{"airConOperationMode":"POWER_ON"},` +
		`"temperatureInUnits":[{"unit":"C","currentTemperatureC":24,"targetTemperatureC":22},{"unit":"F","targetTemperatureF":72}],` +
		`"airFlow":{"windStrength":"MID"},"windDirection":{"rotateUpDown":false},"airQualitySensor":{"humidity":41}}`
	fridgeProfile = `{"property":{` +
		`"temperatureInUnits":[` +
		`{"locationName":"FRIDGE","targetTemperatureC":{"type":"range","mode":["r","w"],"value":{"w":{"min":1,"max":7,"step":1}}}},` +
		`{"locationName":"FREEZER","targetTemperatureC":{"type":"range","mode":["r","w"],"value":{"w":{"min":-24,"max":-14,"step":1}}}}],` +
		`"doorStatus":[{"locationName":"MAIN","doorState":{"type":"enum","mode":["r"],"value":{"r":["OPEN","CLOSE"]}}}],` +
		`"powerSave":{"powerSaveEnabled":{"type":"boolean","mode":["r","w"]}}},` +
		`"notification":{"push":["DOOR_IS_OPEN"]}}`
	fridgeState = `{"temperatureInUnits":[{"locationName":"FRIDGE","targetTemperatureC":3,"unit":"C"},` +
		`{"locationName":"FREEZER","targetTemperatureC":-18,"unit":"C"}],` +
		`"doorStatus":[{"locationName":"MAIN","doorState":"CLOSE"}],"powerSave":{"powerSaveEnabled":false}}`
)

// fakeDevice - прибор поддельного аккаунта: тип, имя, профиль и состояние.
type fakeDevice struct {
	Type, Alias, Profile, State string
}

var fakeDevices = map[string]fakeDevice{
	"dev-1": {"DEVICE_WASHER", "Стиралка", washerProfile, washerState},
	"dev-2": {"DEVICE_AIR_CONDITIONER", "Кондиционер", acProfile, acState},
	"dev-3": {"DEVICE_REFRIGERATOR", "Холодильник", fridgeProfile, fridgeState},
}

// fakeAPI - поддельное API ThinQ Connect.
type fakeAPI struct {
	*httptest.Server
	ca     *testCA
	broker string

	mu        sync.Mutex
	controls  []string
	clients   int
	pushSubs  int
	eventSubs []string
	profiles  int
	headers   http.Header
	// badProfile - отвечать профилем, который не разбирается.
	badProfile bool
}

func newAPI(t *testing.T, ca *testCA) *fakeAPI {
	t.Helper()
	f := &fakeAPI{ca: ca}
	ok := func(w http.ResponseWriter, v any) {
		json.NewEncoder(w).Encode(map[string]any{"messageId": "m", "response": v})
	}
	raw := func(w http.ResponseWriter, response string) {
		io.WriteString(w, `{"messageId":"m","response":`+response+`}`)
	}
	device := func(w http.ResponseWriter, r *http.Request) (fakeDevice, bool) {
		d, found := fakeDevices[r.PathValue("id")]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "1222", "message": "not connected"}})
		}
		return d, found
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		var list []any
		for _, id := range []string{"dev-1", "dev-2", "dev-3"} {
			d := fakeDevices[id]
			list = append(list, map[string]any{"deviceId": id, "deviceInfo": map[string]any{"deviceType": d.Type,
				"modelName": "M-" + id, "alias": d.Alias}})
		}
		ok(w, list)
	})
	mux.HandleFunc("GET /devices/{id}/profile", func(w http.ResponseWriter, r *http.Request) {
		if d, found := device(w, r); found {
			f.mu.Lock()
			f.profiles++
			bad := f.badProfile
			f.mu.Unlock()
			if bad {
				raw(w, `{"property":"сломан"}`)
				return
			}
			raw(w, d.Profile)
		}
	})
	mux.HandleFunc("GET /devices/{id}/state", func(w http.ResponseWriter, r *http.Request) {
		if d, found := device(w, r); found {
			raw(w, d.State)
		}
	})
	mux.HandleFunc("POST /devices/{id}/control", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.controls = append(f.controls, r.PathValue("id")+" "+string(data))
		f.headers = r.Header.Clone()
		f.mu.Unlock()
		ok(w, map[string]any{})
	})
	mux.HandleFunc("POST /client", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.clients++
		f.mu.Unlock()
		ok(w, map[string]any{})
	})
	mux.HandleFunc("POST /client/certificate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ServiceCode string `json:"service-code"`
			CSR         string `json:"csr"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		der, err := base64.StdEncoding.DecodeString(body.CSR)
		csr, cerr := x509.ParseCertificateRequest(der)
		if err != nil || cerr != nil || body.ServiceCode != "SVC202" || csr.Subject.CommonName != "lg_thinq" ||
			csr.SignatureAlgorithm != x509.SHA512WithRSA {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "1311", "message": "bad csr"}})
			return
		}
		cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.sign(t, csr.PublicKey,
			&x509.Certificate{Subject: pkix.Name{CommonName: "client"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})})
		ok(w, map[string]any{"result": map[string]any{"certificatePem": string(cert),
			"subscriptions": []string{"app/clients/" + r.Header.Get("x-client-id") + "/push"}}})
	})
	mux.HandleFunc("GET /route", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"mqttServer": "mqtts://" + f.broker})
	})
	mux.HandleFunc("POST /push/{id}/subscribe", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.pushSubs++
		again := f.pushSubs > 1
		f.mu.Unlock()
		if again {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "1207", "message": "already"}})
			return
		}
		ok(w, map[string]any{})
	})
	mux.HandleFunc("POST /event/{id}/subscribe", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.eventSubs = append(f.eventSubs, string(data))
		f.mu.Unlock()
		ok(w, map[string]any{})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// broker - поддельный брокер MQTT 3.1.1 со взаимной проверкой TLS: принимает подключение
// и подписку и отправляет клиентам сообщения QoS 0.
type broker struct {
	ln net.Listener

	mu     sync.Mutex
	topic  string
	conns  []net.Conn
	accept int
}

func newBroker(t *testing.T, ca *testCA) *broker {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der := ca.sign(t, &key.PublicKey, &x509.Certificate{Subject: pkix.Name{CommonName: "broker"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.pool}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := &broker{ln: ln}
	t.Cleanup(func() {
		ln.Close()
		b.mu.Lock()
		for _, c := range b.conns {
			c.Close()
		}
		b.mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c)
		}
	}()
	return b
}

func (b *broker) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	for {
		typ, err := rd.ReadByte()
		if err != nil {
			return
		}
		n, mult := 0, 1
		for {
			x, err := rd.ReadByte()
			if err != nil {
				return
			}
			n += int(x&127) * mult
			mult *= 128
			if x&128 == 0 {
				break
			}
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(rd, body); err != nil {
			return
		}
		switch typ >> 4 {
		case 1: // CONNECT
			b.mu.Lock()
			b.accept++
			b.mu.Unlock()
			c.Write([]byte{0x20, 2, 0, 0})
		case 8: // SUBSCRIBE: идентификатор, затем фильтр с длиной и QoS
			l := int(body[2])<<8 | int(body[3])
			b.mu.Lock()
			b.topic = string(body[4 : 4+l])
			b.conns = append(b.conns, c)
			b.mu.Unlock()
			c.Write([]byte{0x90, 3, body[0], body[1], 1})
		case 12: // PINGREQ
			c.Write([]byte{0xD0, 0})
		case 14: // DISCONNECT
			return
		}
	}
}

// publish отправляет сообщение QoS 0 всем подписанным клиентам.
func (b *broker) publish(payload string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	topic := b.topic
	n := 2 + len(topic) + len(payload)
	var length []byte
	for {
		x := byte(n % 128)
		n /= 128
		if n > 0 {
			x |= 128
		}
		length = append(length, x)
		if n == 0 {
			break
		}
	}
	msg := append([]byte{0x30}, length...)
	msg = append(msg, byte(len(topic)>>8), byte(len(topic)))
	msg = append(append(msg, topic...), payload...)
	for _, c := range b.conns {
		c.Write(msg)
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

func newCloud(t *testing.T, dir string, api *fakeAPI, ca *testCA) *Cloud {
	t.Helper()
	c, err := New(filepath.Join(dir, "thinq-client.json"), filepath.Join(dir, "thinq"), discard())
	if err != nil {
		t.Fatal(err)
	}
	c.roots = ca.pool
	c.API.Configure("test-pat", "NL", api.URL)
	return c
}

func payloadJSON(t *testing.T, m *model, key string, v any) string {
	t.Helper()
	data, err := json.Marshal(m.payload(key, v))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestModel(t *testing.T) {
	// Стиральная машина из частей: без камеры - часть MAIN, команда несёт location.
	w, err := parseProfile("DEVICE_WASHER", []byte(washerProfile), "")
	if err != nil || w.Location != "MAIN" || !slices.Equal(w.Locations, []string{"MAIN", "MINI"}) {
		t.Fatalf("стиральная машина: %+v %v", w, err)
	}
	if op := w.props["operation.washerOperationMode"]; op == nil || !op.Write || op.Read ||
		!slices.Equal(op.Values, []string{"START", "STOP", "POWER_OFF"}) {
		t.Fatalf("режим работы: %+v", op)
	}
	values := map[string]any{}
	w.flatten(json.RawMessage(washerState), values)
	if values["runState.currentState"] != "INITIAL" || values["remoteControlEnable.remoteControlEnabled"] != true {
		t.Fatalf("состояние MAIN: %v", values)
	}
	v, err := w.value("operation.washerOperationMode", "start")
	if err != nil || payloadJSON(t, w, "operation.washerOperationMode", v) !=
		`{"location":{"locationName":"MAIN"},"operation":{"washerOperationMode":"START"}}` {
		t.Fatalf("команда: %v %v", v, err)
	}
	mini, err := parseProfile("DEVICE_WASHER", []byte(washerProfile), "MINI")
	values = map[string]any{}
	if mini.flatten(json.RawMessage(washerState), values); err != nil || values["runState.currentState"] != "RUNNING" {
		t.Fatalf("часть MINI: %v %v", values, err)
	}
	if _, err := parseProfile("DEVICE_WASHER", []byte(washerProfile), "TOP"); !errors.Is(err, errNoLocation) {
		t.Fatalf("чужая часть: %v", err)
	}

	// Кондиционер: температура по единицам, а значения перечисления в профиле строчные.
	ac, err := parseProfile("DEVICE_AIR_CONDITIONER", []byte(acProfile), "")
	if err != nil || ac.Location != "" || len(ac.Locations) != 0 || !slices.Equal(ac.Push, []string{"WATER_IS_FULL"}) {
		t.Fatalf("кондиционер: %+v %v", ac, err)
	}
	if tp := ac.props["temperatureInUnits.targetTemperatureC"]; tp == nil || !tp.Ranged || tp.Min != 18 || tp.Max != 30 || tp.Step != 0.5 {
		t.Fatalf("температура: %+v", tp)
	}
	if ws := ac.props["airFlow.windStrength"]; !slices.Equal(ws.Values, []string{"LOW", "MID", "HIGH"}) {
		t.Fatalf("скорость: %+v", ws)
	}
	if v, err := ac.value("airFlow.windStrength", "MID"); err != nil || v != "mid" {
		t.Fatalf("значение как в профиле: %v %v", v, err)
	}
	if got := payloadJSON(t, ac, "temperatureInUnits.targetTemperatureC", 22.5); got != `{"temperatureInUnits":{"targetTemperature":22.5,"unit":"C"}}` {
		t.Fatalf("температура в команде: %s", got)
	}
	for _, c := range []struct {
		key string
		v   any
		err error
	}{
		{"temperatureInUnits.targetTemperatureC", 31.0, errBadValue},
		{"temperatureInUnits.targetTemperatureC", 22.3, errBadValue},
		{"airConJobMode.currentJobMode", "TURBO", errBadValue},
		{"airQualitySensor.humidity", 40.0, errReadOnly},
		{"display.light", "ON", errReadOnly},
	} {
		if _, err := ac.value(c.key, c.v); !errors.Is(err, c.err) {
			t.Errorf("%s = %v: %v", c.key, c.v, err)
		}
	}
	values = map[string]any{}
	ac.flatten(json.RawMessage(acState), values)
	if values["temperatureInUnits.currentTemperatureC"] != 24.0 || values["airFlow.windStrength"] != "MID" ||
		values["windDirection.rotateUpDown"] != false || values["temperatureInUnits.targetTemperatureF"] != 72.0 {
		t.Fatalf("состояние кондиционера: %v", values)
	}

	// Холодильник: камеры внутри ресурсов, дверь MAIN есть у каждой камеры.
	fz, err := parseProfile("DEVICE_REFRIGERATOR", []byte(fridgeProfile), "FREEZER")
	if err != nil || !slices.Equal(fz.Locations, []string{"FRIDGE", "FREEZER"}) {
		t.Fatalf("холодильник: %+v %v", fz, err)
	}
	if tp := fz.props["temperatureInUnits.targetTemperatureC"]; tp == nil || tp.Min != -24 || fz.props["doorStatus.doorState"] == nil {
		t.Fatalf("морозильная камера: %+v", fz.props)
	}
	values = map[string]any{}
	fz.flatten(json.RawMessage(fridgeState), values)
	if values["temperatureInUnits.targetTemperatureC"] != -18.0 || values["doorStatus.doorState"] != "CLOSE" ||
		values["powerSave.powerSaveEnabled"] != false {
		t.Fatalf("состояние морозильной камеры: %v", values)
	}
	if got := payloadJSON(t, fz, "temperatureInUnits.targetTemperatureC", -18.0); got !=
		`{"temperatureInUnits":{"locationName":"FREEZER","targetTemperatureC":-18,"unit":"C"}}` {
		t.Fatalf("температура камеры в команде: %s", got)
	}
	fr, err := parseProfile("DEVICE_REFRIGERATOR", []byte(fridgeProfile), "")
	values = map[string]any{}
	if fr.flatten(json.RawMessage(fridgeState), values); err != nil || fr.Location != "FRIDGE" ||
		values["temperatureInUnits.targetTemperatureC"] != 3.0 {
		t.Fatalf("холодильная камера по умолчанию: %v %v", values, err)
	}
}

// runAll запускает облако и прибор и возвращает остановку обоих.
func runAll(t *testing.T, c *Cloud, d *hcdevice.Device, changed alice.Changed) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() {
		c.Run(ctx)
		done <- struct{}{}
	}()
	go func() {
		d.Run(ctx, changed)
		done <- struct{}{}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestWasher(t *testing.T) {
	ca := newCA(t)
	api := newAPI(t, ca)
	b := newBroker(t, ca)
	api.broker = b.ln.Addr().String()
	dir := t.TempDir()
	c := newCloud(t, dir, api, ca)
	a, err := c.Appliance("dev-1", "")
	if err != nil || a.Describe().Type != "" {
		t.Fatalf("прибор до профиля: %v", err)
	}
	// Профиль шлюз загружает сам: тип прибора берёт из списка аккаунта и сообщает о профиле.
	c.loadMissing(context.Background())
	select {
	case <-c.Changes():
	default:
		t.Fatal("нет сигнала о профиле")
	}
	if a.Describe().Type != "DEVICE_WASHER" || !slices.Equal(a.Describe().Events, []string{"WASHING_IS_COMPLETE", "ERROR_DURING_WASHING"}) {
		t.Fatalf("описание: %+v", a.Describe())
	}

	p, err := profile.Resolve("DEVICE_WASHER", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &hcdevice.Device{ID: "washer", Name: "Стиралка", Room: "Ванная", Profile: p, Appliance: a, Manufacturer: "LG"}
	urgent := make(chan []alice.DeviceState, 8)
	c = newCloud(t, dir, api, ca)
	if a, err = c.Appliance("dev-1", ""); err != nil || a.Describe().Type != "DEVICE_WASHER" {
		t.Fatalf("профиль с диска: %v", err)
	}
	d.Appliance = a
	stop := runAll(t, c, d, func(ev []alice.DeviceState) {
		if len(ev) > 0 {
			urgent <- ev
		}
	})

	// Регистрация клиента, сертификат, MQTT со взаимным TLS, подписки и состояние части MAIN.
	eventually(t, "состояние", func() bool { return a.State().Online && a.State().Values["runState.currentState"] == "INITIAL" })
	b.mu.Lock()
	topic := b.topic
	b.mu.Unlock()
	if !strings.HasPrefix(topic, "app/clients/thinq-open-") {
		t.Fatalf("топик: %s", topic)
	}
	api.mu.Lock()
	subs := api.eventSubs
	api.mu.Unlock()
	if len(subs) != 1 || subs[0] != `{"expire":{"timer":4464,"unit":"HOUR"}}` {
		t.Fatalf("подписка на события: %v", subs)
	}
	info := d.Info()
	if info[0].Type != "devices.types.washing_machine" || info[0].DeviceInfo.Manufacturer != "LG" || len(info) != 2 {
		t.Fatalf("устройство Алисы: %+v", info)
	}
	if st := d.States(); st[0].Capabilities[0].State.(alice.StateValue).Value != false {
		t.Fatalf("состояние: %+v", st)
	}

	on := &alice.Action{Type: alice.CapOnOff, State: alice.ActionState{Instance: "on", Value: true}}
	if code := d.Act(context.Background(), "washer", on); code != "" {
		t.Fatalf("включение: %q", code)
	}
	api.mu.Lock()
	control, h := api.controls[0], api.headers
	api.mu.Unlock()
	if control != `dev-1 {"location":{"locationName":"MAIN"},"operation":{"washerOperationMode":"START"}}` ||
		h.Get("x-conditional-control") != "true" || h.Get("x-api-key") != apiKey || h.Get("Authorization") != "Bearer test-pat" ||
		h.Get("x-country") != "NL" || h.Get("x-service-phase") != "OP" || len(h.Get("x-message-id")) != 22 ||
		!strings.HasPrefix(h.Get("x-client-id"), "thinq-open-") {
		t.Fatalf("команда: %s %v", control, h)
	}

	// Состояние по MQTT: стирка идёт, затем закончилась - событие датчика уходит сразу.
	b.publish(`{"pushType":"DEVICE_STATUS","deviceId":"dev-1","report":{"location":{"locationName":"MAIN"},"runState":{"currentState":"RUNNING"}}}`)
	eventually(t, "стирка", func() bool { return a.State().Values["runState.currentState"] == "RUNNING" })
	if st := d.States(); st[0].Capabilities[0].State.(alice.StateValue).Value != true {
		t.Fatalf("стирка идёт: %+v", st)
	}
	// Оставшееся время: часы и минуты ThinQ - одно число в секундах. Встроенный профиль его
	// Алисе не передаёт: свойства remaining_time нет в документации Яндекса.
	b.publish(`{"pushType":"DEVICE_STATUS","deviceId":"dev-1","report":{"location":{"locationName":"MAIN"},"timer":{"remainHour":1,"remainMinute":25}}}`)
	eventually(t, "оставшееся время", func() bool { return a.State().Values[RemainKey] == 5100.0 })
	for _, pr := range d.States()[0].Properties {
		if sv := pr.State.(alice.StateValue); sv.Instance == hcdevice.RemainingTime {
			t.Fatalf("оставшееся время ушло Алисе: %v", sv.Value)
		}
	}
	b.publish(`{"pushType":"DEVICE_STATUS","deviceId":"dev-1","report":[{"location":{"locationName":"MAIN"},"runState":{"currentState":"END"}}]}`)
	wantButton := func(value string) {
		t.Helper()
		select {
		case ev := <-urgent:
			raw, _ := json.Marshal(ev)
			if string(raw) != `[{"id":"washer.events","properties":[{"type":"devices.properties.event","state":{"instance":"button","value":"`+value+`"}}]}]` {
				t.Fatalf("событие: %s", raw)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("нет события %s", value)
		}
	}
	wantButton("click")
	// Уведомление об окончании второго click не даёт, но считается в метриках; ошибка - long_press.
	before := metrics.Value("hcgateway_device_events_total", "device", "washer", "event", "WASHING_IS_COMPLETE")
	b.publish(`{"pushType":"DEVICE_PUSH","deviceId":"dev-1","pushCode":"washing_is_complete"}`)
	eventually(t, "уведомление", func() bool {
		return metrics.Value("hcgateway_device_events_total", "device", "washer", "event", "WASHING_IS_COMPLETE") == before+1
	})
	b.publish(`{"pushType":"DEVICE_PUSH","deviceId":"dev-1","pushCode":"ERROR_DURING_WASHING"}`)
	wantButton("long_press")
	select {
	case ev := <-urgent:
		t.Fatalf("лишнее событие: %+v", ev)
	default:
	}
	// Удалённое управление выключено на машине: команда не отправляется.
	b.publish(`{"pushType":"DEVICE_STATUS","deviceId":"dev-1","report":{"location":{"locationName":"MAIN"},"remoteControlEnable":{"remoteControlEnabled":false}}}`)
	eventually(t, "удалённое управление", func() bool { return d.Act(context.Background(), "washer", on) == alice.CodeRemoteDisabled })

	stop()
	// После перезапуска клиент уже зарегистрирован, а профиль лежит на диске.
	again, err := New(filepath.Join(dir, "thinq-client.json"), filepath.Join(dir, "thinq"), discard())
	if err != nil || again.creds.Certificate == "" || again.creds.ClientID != c.creds.ClientID {
		t.Fatalf("файл клиента: %v", err)
	}
	api.mu.Lock()
	clients, profiles := api.clients, api.profiles
	api.mu.Unlock()
	if clients != 1 || profiles != 1 {
		t.Fatalf("регистраций клиента %d, загрузок профиля %d", clients, profiles)
	}
}

// Ответ, который не разбирается, не заменяет рабочий профиль на диске.
func TestBadProfileKeepsCopy(t *testing.T) {
	ca := newCA(t)
	api := newAPI(t, ca)
	c := newCloud(t, t.TempDir(), api, ca)
	ctx := context.Background()
	if err := c.RefreshAccount(ctx); err != nil {
		t.Fatal(err)
	}
	path := c.profilePath("dev-2")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.badProfile = true
	api.mu.Unlock()
	if err := c.RefreshAccount(ctx); err == nil {
		t.Fatal("плохой профиль принят")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("файл профиля изменился: %v", err)
	}
}

func TestCameras(t *testing.T) {
	ca := newCA(t)
	api := newAPI(t, ca)
	c := newCloud(t, t.TempDir(), api, ca)
	freezer, err := c.Appliance("dev-3", "FREEZER")
	if err != nil {
		t.Fatal(err)
	}
	// «Обновить список» загружает список аккаунта и профили всех его приборов.
	if err := c.RefreshAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Locations("dev-3"); !slices.Equal(got, []string{"FRIDGE", "FREEZER"}) {
		t.Fatalf("камеры: %v", got)
	}
	if got := c.Locations("dev-2"); got != nil {
		t.Fatalf("камеры кондиционера: %v", got)
	}
	if _, err := c.Appliance("dev-3", "TOP"); !errors.Is(err, errNoLocation) {
		t.Fatalf("чужая камера: %v", err)
	}
	if o := freezer.Describe().Options["temperatureInUnits.targetTemperatureC"]; o.Min != -24 || !o.Setting {
		t.Fatalf("температура морозильной камеры: %+v", o)
	}
	ctx := context.Background()
	if err := freezer.Set(ctx, "temperatureInUnits.targetTemperatureC", -18.0); err != nil {
		t.Fatal(err)
	}
	if err := freezer.Set(ctx, "temperatureInUnits.targetTemperatureC", -30.0); !errors.Is(err, appliance.ErrInvalidValue) {
		t.Fatalf("вне пределов: %v", err)
	}
	if err := freezer.Set(ctx, "doorStatus.doorState", "OPEN"); !errors.Is(err, appliance.ErrNotSupported) {
		t.Fatalf("только для чтения: %v", err)
	}
	if err := freezer.Start(ctx, "P", nil); !errors.Is(err, appliance.ErrNotSupported) {
		t.Fatalf("программа: %v", err)
	}
	ac, _ := c.Appliance("dev-2", "")
	if err := ac.Set(ctx, "airFlow.windStrength", "HIGH"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	controls := slices.Clone(api.controls)
	api.mu.Unlock()
	if !slices.Equal(controls, []string{
		`dev-3 {"temperatureInUnits":{"locationName":"FREEZER","targetTemperatureC":-18,"unit":"C"}}`,
		`dev-2 {"airFlow":{"windStrength":"high"}}`,
	}) {
		t.Fatalf("команды: %v", controls)
	}
}

func TestCodes(t *testing.T) {
	cases := map[error]error{
		&APIError{Status: 400, Code: codeRemoteOff}:          appliance.ErrRemoteDisabled,
		&APIError{Status: 400, Code: codeNotInState}:         appliance.ErrWrongMode,
		&APIError{Status: 400, Code: codePowerOff}:           appliance.ErrWrongMode,
		&APIError{Status: 400, Code: codeNotConnectedDevice}: appliance.ErrUnreachable,
		&APIError{Status: 429, Code: codeExceededCalls}:      appliance.ErrBusy,
		&APIError{Status: 503}:                               appliance.ErrUnreachable,
		ErrNotConfigured:                                     appliance.ErrUnreachable,
		errors.New("dial tcp: refused"):                      appliance.ErrUnreachable,
	}
	for err, want := range cases {
		if got := mapErr(err); !errors.Is(got, want) {
			t.Errorf("%v: %v", err, got)
		}
	}
	other := &APIError{Status: 400, Code: "1000"}
	if got := mapErr(other); got != error(other) {
		t.Errorf("другая ошибка: %v", got)
	}
	for country, want := range map[string]string{"NL": "eic", "RU": "eic", "US": "aic", "KR": "kic"} {
		if r, ok := Region(country); !ok || r != want {
			t.Errorf("%s: %s", country, r)
		}
	}
	if _, ok := Region("XX"); ok {
		t.Error("неизвестная страна")
	}
}
