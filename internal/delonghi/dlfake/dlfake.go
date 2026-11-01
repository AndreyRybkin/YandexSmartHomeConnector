// Package dlfake - поддельные Gigya, облако приборов, брокер AWS IoT и кофемашина De'Longhi
// для тестов: те же адреса, сообщения и проверки, что у настоящих.
package dlfake

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Учётная запись подделки.
const (
	Email    = "owner@example.com"
	Password = "test-password-1"
	// Pool - пул Gigya, в котором живёт аккаунт: шлюз должен перебрать EU и EU_US.
	Pool    = "CH"
	poolKey = "3_WP_c8OVu_yOoqYXN3Dq-Oi7nNkbS2bwqS3rQXJ6SPkodgE4FOpyuE_UVlrCuSGEm"
	// Serial и Thing - машина аккаунта.
	Serial = "000000ZZ00000000000"
	Thing  = "EUPDL01COF000000000000"

	sessionToken  = "st2.test-session"
	sessionSecret = "c2VjcmV0LWtleS0xMjM=" // base64 «secret-key-123»
)

// Cloud - Gigya, REST облака и брокер AWS IoT на одном сервере без TLS.
type Cloud struct {
	Server *httptest.Server

	mu sync.Mutex
	// tokens - выданные JWT и их срок.
	tokens map[string]time.Time
	// FailJWT - отказывать в getJWT, как при отозванной сессии.
	FailJWT bool
	// Logins и JWTs - сколько было входов и выпусков JWT.
	Logins, JWTs int
	// Shadow - reported тени MachineStatus.
	Shadow map[string]any
	// ShadowReads - сколько раз читали тень; ShadowTime - время документа тени.
	ShadowReads int
	ShadowTime  time.Time
	// connects - сколько раз подключались к брокеру.
	connects int
	n        int
}

// NewCloud запускает подделку облака.
func NewCloud() *Cloud {
	c := &Cloud{tokens: map[string]time.Time{}, Shadow: map[string]any{"CurrMode": 7.0, "Status": "Idle",
		"alarms": map[string]any{"TankLevel": true, "NoCoffee": false}}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts.login", c.login)
	mux.HandleFunc("POST /accounts.getJWT", c.getJWT)
	mux.HandleFunc("GET /dlg-prod/devices", c.devices)
	mux.HandleFunc("GET /mqtt", c.mqtt)
	c.Server = httptest.NewServer(mux)
	return c
}

// URLs - адреса Gigya, REST и брокера для Account.Configure.
func (c *Cloud) URLs() (gigya, api, iot string) {
	u := c.Server.URL
	return u, u + "/dlg-prod", "ws" + strings.TrimPrefix(u, "http") + "/mqtt"
}

func (c *Cloud) Close() {
	c.Server.Close()
}

// SetShadow задаёт тень MachineStatus и время её документа.
func (c *Cloud) SetShadow(reported map[string]any, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Shadow, c.ShadowTime = reported, at
}

// Reads - сколько раз читали тень; Connects - сколько раз подключались к брокеру.
func (c *Cloud) Reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ShadowReads
}

func (c *Cloud) Connects() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connects
}

// Valid сообщает, действует ли токен.
func (c *Cloud) Valid(token string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.tokens[token]
	return ok && time.Now().Before(exp)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (c *Cloud) login(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c.mu.Lock()
	c.Logins++
	c.mu.Unlock()
	if r.Form.Get("apiKey") != poolKey {
		reply(w, map[string]any{"errorCode": 400093, "errorMessage": "Invalid ApiKey parameter"})
		return
	}
	if r.Form.Get("loginID") != Email || r.Form.Get("password") != Password {
		reply(w, map[string]any{"errorCode": 403042, "errorMessage": "Invalid LoginID"})
		return
	}
	if r.Form.Get("targetEnv") != "mobile" {
		reply(w, map[string]any{"errorCode": 400006, "errorMessage": "Invalid parameter value"})
		return
	}
	reply(w, map[string]any{"errorCode": 0, "sessionInfo": map[string]any{"sessionToken": sessionToken, "sessionSecret": sessionSecret}})
}

// getJWT проверяет подпись запроса секретом сессии, как Gigya: без неё - 403005.
func (c *Cloud) getJWT(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c.mu.Lock()
	fail := c.FailJWT
	c.mu.Unlock()
	if fail || r.Form.Get("oauth_token") != sessionToken || r.Form.Get("apiKey") != poolKey {
		reply(w, map[string]any{"errorCode": 403005, "errorMessage": "Unauthorized user"})
		return
	}
	params := url.Values{}
	for k, v := range r.Form {
		if k != "sig" {
			params[k] = v
		}
	}
	want := sign("POST", "http://"+r.Host+r.URL.Path, params)
	if !hmac.Equal([]byte(want), []byte(r.Form.Get("sig"))) {
		reply(w, map[string]any{"errorCode": 403005, "errorMessage": "Unauthorized user"})
		return
	}
	ttl, err := strconv.Atoi(r.Form.Get("expiration"))
	if err != nil || ttl <= 0 {
		reply(w, map[string]any{"errorCode": 400006, "errorMessage": "Invalid parameter value"})
		return
	}
	c.mu.Lock()
	c.JWTs++
	c.n++
	exp := time.Now().Add(time.Duration(ttl) * time.Second)
	token := jwt(exp, c.n)
	c.tokens[token] = exp
	c.mu.Unlock()
	reply(w, map[string]any{"errorCode": 0, "id_token": token})
}

// sign - подпись Gigya, написанная отдельно от шлюза: так тест сверяет две реализации.
func sign(method, rawURL string, params url.Values) string {
	esc := func(s string) string {
		var b strings.Builder
		for _, ch := range []byte(s) {
			if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || strings.IndexByte("-._~", ch) >= 0 {
				b.WriteByte(ch)
			} else {
				fmt.Fprintf(&b, "%%%02X", ch)
			}
		}
		return b.String()
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, esc(k)+"="+esc(params.Get(k)))
	}
	key, _ := base64.StdEncoding.DecodeString(sessionSecret)
	mac := hmac.New(sha1.New, key)
	mac.Write([]byte(method + "&" + esc(rawURL) + "&" + esc(strings.Join(parts, "&"))))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func jwt(exp time.Time, n int) string {
	enc := base64.RawURLEncoding.EncodeToString
	head := enc([]byte(`{"alg":"RS256"}`))
	body := enc(fmt.Appendf(nil, `{"iss":"https://fidm.gigya.com/jwt/test/","sub":"uid-1","exp":%d,"n":%d}`, exp.Unix(), n))
	return head + "." + body + ".sig"
}

func (c *Cloud) bearer(r *http.Request) bool {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && c.Valid(t)
}

func (c *Cloud) devices(w http.ResponseWriter, r *http.Request) {
	if !c.bearer(r) || r.Header.Get("source") != "mycoffeelounge" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	reply(w, map[string]any{"ownedByMe": []any{map[string]any{"machineName": Thing, "serialNumber": Serial,
		"machineModel": "ECAM63070", "sku": "0000000000", "status": "ONLINE"}}, "sharedToMe": []any{}})
}

// mqtt - брокер AWS IoT: CONNECT с JWT, SUBSCRIBE на точные топики, PUBLISH в .../get.
func (c *Cloud) mqtt(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("x-amz-customauthorizer-name") != "dlg-prod-token-authorizer" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	up := websocket.Upgrader{Subprotocols: []string{"mqtt"}}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	rd := bufio.NewReader(&wsReader{ws: ws})
	send := func(typ byte, body []byte) {
		_ = ws.WriteMessage(websocket.BinaryMessage, append(varInt([]byte{typ}, len(body)), body...))
	}
	typ, body, err := readPacket(rd)
	if err != nil || typ != 0x10 {
		return
	}
	user, pass, ok := connectAuth(body)
	if !ok || user != "x-amz-customauthorizer-name=dlg-prod-token-authorizer" || !c.Valid(pass) {
		send(0x20, []byte{0, 0x87, 0})
		return
	}
	send(0x20, []byte{0, 0, 0})
	c.mu.Lock()
	c.connects++
	c.mu.Unlock()
	for {
		typ, body, err := readPacket(rd)
		if err != nil {
			return
		}
		switch typ {
		case 0x80:
			id := body[:2]
			send(0x90, append(append([]byte{}, id...), 0, 0, 0))
		case 0x30:
			tl := int(binary.BigEndian.Uint16(body))
			topic := string(body[2 : 2+tl])
			prefix := "$aws/things/" + Thing + "/shadow/name/"
			if topic == prefix+"BrewingStatisticsTotals/get" {
				doc, _ := json.Marshal(map[string]any{"state": map[string]any{"reported": map[string]any{"Brew": 1001,
					"Recipes": []any{map[string]any{"ID": 1, "Brew": 85}, map[string]any{"ID": 6, "Brew": 986}, map[string]any{"ID": 9, "Brew": 0}}}}})
				send(0x30, append(append(str(nil, topic+"/accepted"), 0), doc...))
				continue
			}
			if topic != prefix+"MachineStatus/get" {
				out := append(str(nil, prefix+strings.TrimSuffix(strings.TrimPrefix(topic, prefix), "/get")+"/get/rejected"), 0)
				send(0x30, append(out, `{"code":404}`...))
				continue
			}
			c.mu.Lock()
			c.ShadowReads++
			doc := map[string]any{"state": map[string]any{"reported": c.Shadow}}
			if !c.ShadowTime.IsZero() {
				doc["timestamp"] = c.ShadowTime.Unix()
			}
			raw, _ := json.Marshal(doc)
			c.mu.Unlock()
			out := append(str(nil, topic+"/accepted"), 0)
			send(0x30, append(out, raw...))
		case 0xE0:
			return
		}
	}
}

type wsReader struct {
	ws  *websocket.Conn
	cur io.Reader
}

func (r *wsReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			_, rd, err := r.ws.NextReader()
			if err != nil {
				return 0, err
			}
			r.cur = rd
		}
		n, err := r.cur.Read(p)
		if err == io.EOF {
			r.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func varInt(b []byte, n int) []byte {
	for {
		c := byte(n % 128)
		n /= 128
		if n > 0 {
			c |= 0x80
		}
		b = append(b, c)
		if n == 0 {
			return b
		}
	}
}

func str(b []byte, s string) []byte {
	return append(binary.BigEndian.AppendUint16(b, uint16(len(s))), s...)
}

func readPacket(r *bufio.Reader) (byte, []byte, error) {
	h, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mul := 0, 1
	for {
		c, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(c&0x7F) * mul
		if c&0x80 == 0 {
			break
		}
		mul *= 128
	}
	body := make([]byte, n)
	_, err = io.ReadFull(r, body)
	return h & 0xF0, body, err
}

// connectAuth достаёт имя и пароль из CONNECT MQTT 5 без свойств.
func connectAuth(b []byte) (string, string, bool) {
	// "MQTT", версия, флаги, keep alive, длина свойств 0.
	if len(b) < 11 || string(b[2:6]) != "MQTT" || b[6] != 5 || b[7]&0xC0 != 0xC0 || b[10] != 0 {
		return "", "", false
	}
	rest := b[11:]
	next := func() (string, bool) {
		if len(rest) < 2 {
			return "", false
		}
		l := int(binary.BigEndian.Uint16(rest))
		if 2+l > len(rest) {
			return "", false
		}
		s := string(rest[2 : 2+l])
		rest = rest[2+l:]
		return s, true
	}
	if _, ok := next(); !ok {
		return "", "", false
	}
	user, ok1 := next()
	pass, ok2 := next()
	return user, pass, ok1 && ok2
}

// Machine - кофемашина в домашней сети: WebSocket /ws/lan2lan по TLS.
type Machine struct {
	Server *httptest.Server
	cloud  *Cloud

	mu sync.Mutex
	// Auths - попытки входа; Commands - команды после входа.
	Auths    []map[string]any
	Commands []map[string]any
	// Busy - машина занята: на BeverageRequest - отказ.
	Busy  bool
	conns map[*websocket.Conn]bool
	next  int
}

// NewMachine запускает машину, которая принимает токены облака cloud.
func NewMachine(cloud *Cloud) *Machine {
	m := &Machine{cloud: cloud, conns: map[*websocket.Conn]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/lan2lan", m.serve)
	m.Server = httptest.NewTLSServer(mux)
	return m
}

// Host - адрес машины для настроек.
func (m *Machine) Host() string {
	return strings.TrimPrefix(m.Server.URL, "https://")
}

// Pin - отпечаток сертификата машины.
func (m *Machine) Pin() string {
	sum := sha256.Sum256(m.Server.Certificate().Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func (m *Machine) Close() {
	m.mu.Lock()
	for ws := range m.conns {
		ws.Close()
	}
	m.mu.Unlock()
	m.Server.Close()
}

func (m *Machine) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	var auth map[string]any
	if ws.ReadJSON(&auth) != nil {
		return
	}
	m.mu.Lock()
	m.Auths = append(m.Auths, auth)
	m.mu.Unlock()
	token, _ := auth["AuthToken"].(string)
	if auth["Message"] != "auth" {
		_ = ws.WriteJSON(map[string]any{"Message": auth["Message"], "Response": "UNAUTHORIZED"})
		return
	}
	if auth["SerialNo"] != Serial || !m.cloud.Valid(token) {
		_ = ws.WriteJSON(map[string]any{"Message": "auth", "Response": "KO"})
		return
	}
	// Соединение попадает в conns после ответа на вход: Status пишет в него под m.mu,
	// а gorilla допускает одного пишущего.
	m.mu.Lock()
	m.next++
	id := m.next
	err = ws.WriteJSON(map[string]any{"Message": "auth", "Response": "OK", "ConnectionId": id})
	if err == nil {
		m.conns[ws] = true
	}
	m.mu.Unlock()
	if err != nil {
		return
	}
	defer func() {
		m.mu.Lock()
		delete(m.conns, ws)
		m.mu.Unlock()
	}()
	for {
		var cmd map[string]any
		if ws.ReadJSON(&cmd) != nil {
			return
		}
		m.mu.Lock()
		m.Commands = append(m.Commands, cmd)
		busy := m.Busy
		m.mu.Unlock()
		resp := map[string]any{"Message": fmt.Sprint(cmd["Message"]) + "Response", "Response": "OK", "AppId": "MCL",
			"RequestId": cmd["RequestId"], "OperationId": 1}
		switch cmd["Message"] {
		case "BeverageRequest":
			resp["ProfileId"], resp["RecipeId"], resp["ErrorDescription"] = cmd["ProfileId"], cmd["RecipeId"], ""
			if busy {
				resp["Response"], resp["ErrorDescription"] = "KO", "machine busy"
			}
		case "MachineUI":
			resp["TurnOnOff"] = cmd["TurnOnOff"]
		default:
			resp["Response"] = "KO"
		}
		m.mu.Lock()
		err := ws.WriteJSON(resp)
		m.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// Status отправляет всем входившим соединениям StatusService с reported.
func (m *Machine) Status(reported map[string]any) {
	raw, _ := json.Marshal(reported)
	m.mu.Lock()
	defer m.mu.Unlock()
	for ws := range m.conns {
		_ = ws.WriteJSON(map[string]any{"Message": "StatusService", "Response": string(raw)})
	}
}

// Connected - сколько соединений прошло вход и открыто.
func (m *Machine) Connected() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns)
}

// Drop закрывает все соединения, как машина при включении.
func (m *Machine) Drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ws := range m.conns {
		ws.Close()
	}
}

// Snapshot - копии попыток входа и команд.
func (m *Machine) Snapshot() (auths, commands []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.Auths...), append([]map[string]any(nil), m.Commands...)
}
