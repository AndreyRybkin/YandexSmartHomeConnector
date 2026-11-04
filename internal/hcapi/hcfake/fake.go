// Package hcfake - поддельное облако Home Connect для тестов: вход по Device Flow, запросы
// к приборам, команды и поток событий. Ведёт себя так, как описано в документации облака.
package hcfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	ClientID     = "test-client"
	ClientSecret = "test-secret"
	UserCode     = "ABCD-1234"
	mediaType    = "application/vnd.bsh.sdk.v1+json"
)

// Program - программа поддельного прибора и описание её параметров, как его отдаёт облако.
type Program struct {
	Key     string
	Options []map[string]any
}

// Appliance - поддельный прибор.
type Appliance struct {
	HaID, Name, Type, Brand string
	Connected               bool
	Status                  map[string]any
	Settings                map[string]any
	// SettingInfo - что облако добавляет к настройке в GET /settings/{key}: unit, type, constraints.
	SettingInfo      map[string]map[string]any
	Selected, Active string
	Programs         []Program
	Commands         []string
}

// Cloud - поддельное облако.
type Cloud struct {
	*httptest.Server

	mu         sync.Mutex
	appliances map[string]*Appliance
	order      []string
	approved   bool
	slowDown   bool
	access     string
	refresh    int
	tokens     int
	streams    []chan string
	// Requests - запросы к API: метод и путь.
	Requests []string
	// Fail - ответить этим кодом на следующий запрос к API.
	Fail int
	// failSettings - чем отвечать на GET /settings/{key}, пока не снято.
	failSettings map[string]int
	// holdSettings - GET /settings/{key} ждёт закрытия канала.
	holdSettings chan struct{}
}

// New запускает облако с приборами.
func New(t *testing.T, appliances ...*Appliance) *Cloud {
	t.Helper()
	c := &Cloud{appliances: map[string]*Appliance{}}
	for _, a := range appliances {
		c.appliances[a.HaID] = a
		c.order = append(c.order, a.HaID)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /security/oauth/device_authorization", c.deviceAuthorization)
	mux.HandleFunc("POST /security/oauth/token", c.token)
	mux.HandleFunc("GET /api/homeappliances", c.auth(c.list))
	mux.HandleFunc("GET /api/homeappliances/events", c.auth(c.events))
	mux.HandleFunc("GET /api/homeappliances/{haId}/status", c.auth(c.values("status")))
	mux.HandleFunc("GET /api/homeappliances/{haId}/settings", c.auth(c.values("settings")))
	mux.HandleFunc("GET /api/homeappliances/{haId}/settings/{key}", c.auth(c.setting))
	mux.HandleFunc("GET /api/homeappliances/{haId}/programs/selected", c.auth(c.program(false)))
	mux.HandleFunc("GET /api/homeappliances/{haId}/programs/active", c.auth(c.program(true)))
	mux.HandleFunc("GET /api/homeappliances/{haId}/programs/available", c.auth(c.available))
	mux.HandleFunc("GET /api/homeappliances/{haId}/programs/available/{key}", c.auth(c.programOptions))
	mux.HandleFunc("GET /api/homeappliances/{haId}/commands", c.auth(c.commands))
	mux.HandleFunc("PUT /api/homeappliances/{haId}/programs/active", c.auth(c.start))
	mux.HandleFunc("DELETE /api/homeappliances/{haId}/programs/active", c.auth(c.stop))
	mux.HandleFunc("PUT /api/homeappliances/{haId}/programs/selected", c.auth(c.selectProgram))
	mux.HandleFunc("PUT /api/homeappliances/{haId}/programs/active/options/{key}", c.auth(c.put))
	mux.HandleFunc("PUT /api/homeappliances/{haId}/settings/{key}", c.auth(c.put))
	mux.HandleFunc("PUT /api/homeappliances/{haId}/commands/{key}", c.auth(c.command))
	c.Server = httptest.NewServer(mux)
	t.Cleanup(c.Close)
	return c
}

// FailSetting - отвечать кодом status на GET настройки key; 0 - снова отвечать как обычно.
func (c *Cloud) FailSetting(key string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failSettings == nil {
		c.failSettings = map[string]int{}
	}
	c.failSettings[key] = status
}

// HoldSettings задерживает ответы на GET настройки, пока тест не вызовет возвращённую функцию.
func (c *Cloud) HoldSettings() (release func()) {
	ch := make(chan struct{})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holdSettings = ch
	return func() { close(ch) }
}

// Approve - владелец вошёл в аккаунт: следующий опрос получит токены.
func (c *Cloud) Approve() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.approved = true
}

// SlowDown - следующий опрос получит slow_down.
func (c *Cloud) SlowDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slowDown = true
}

// TokenFile выдаёт токены и пишет их в файл в формате клиента hcapi: так тест начинает с выполненным входом.
func (c *Cloud) TokenFile(t *testing.T, path string) {
	t.Helper()
	c.mu.Lock()
	c.tokens++
	c.access = fmt.Sprintf("access-%d", c.tokens)
	data, _ := json.Marshal(map[string]any{"access_token": c.access, "refresh_token": "refresh",
		"expires": time.Now().Add(24 * time.Hour)})
	c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Revoke отзывает access token: запросы с ним получают 401.
func (c *Cloud) Revoke() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.access = "revoked"
}

// Refreshes - сколько раз токен обновили.
func (c *Cloud) Refreshes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refresh
}

// Streams - сколько потоков событий открыто.
func (c *Cloud) Streams() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams)
}

// Count - сколько было запросов method path.
func (c *Cloud) Count(method, path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.Requests {
		if r == method+" "+path {
			n++
		}
	}
	return n
}

// Send отправляет событие во все открытые потоки.
func (c *Cloud) Send(typ, haID string, items ...map[string]any) {
	data, _ := json.Marshal(map[string]any{"items": items})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendLocked(typ, haID, string(data))
}

func (c *Cloud) sendLocked(typ, haID, data string) {
	msg := fmt.Sprintf("event: %s\ndata: %s\nid: %s\n\n", typ, data, haID)
	for _, s := range c.streams {
		s <- msg
	}
}

// Item - одно изменение в событии.
func Item(key string, value any) map[string]any {
	return map[string]any{"key": key, "value": value, "timestamp": time.Now().Unix(), "level": "hint", "handling": "none"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, key string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"key": key, "description": key}})
}

func (c *Cloud) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("scope") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device_code": "device-1", "user_code": UserCode,
		"verification_uri": c.URL + "/verify", "verification_uri_complete": c.URL + "/verify?user_code=" + UserCode,
		"expires_in": 300, "interval": 5})
}

func (c *Cloud) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "device_code":
		switch {
		case r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("device_code") != "device-1":
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		case c.slowDown:
			c.slowDown = false
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slow_down"})
		case !c.approved:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
		default:
			c.issueLocked(w)
		}
	case "refresh_token":
		if r.PostForm.Get("client_secret") != ClientSecret || r.PostForm.Get("refresh_token") != "refresh" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		c.refresh++
		c.issueLocked(w)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (c *Cloud) issueLocked(w http.ResponseWriter) {
	c.tokens++
	c.access = fmt.Sprintf("access-%d", c.tokens)
	writeJSON(w, http.StatusOK, map[string]any{"access_token": c.access, "refresh_token": "refresh",
		"expires_in": 86400, "token_type": "Bearer", "scope": "IdentifyAppliance Monitor Control Settings"})
}

// auth пропускает только запросы с действующим токеном и записывает их.
func (c *Cloud) auth(next func(w http.ResponseWriter, r *http.Request, a *Appliance)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.Requests = append(c.Requests, r.Method+" "+r.URL.Path)
		ok := c.access != "" && r.Header.Get("Authorization") == "Bearer "+c.access
		fail := c.Fail
		c.Fail = 0
		a := c.appliances[r.PathValue("haId")]
		c.mu.Unlock()
		switch {
		case !ok:
			apiError(w, http.StatusUnauthorized, "invalid_token")
		case fail != 0:
			if fail == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "52")
			}
			apiError(w, fail, "SDK.Error.Fake")
		case r.PathValue("haId") != "" && a == nil:
			apiError(w, http.StatusNotFound, "SDK.Error.HomeApplianceNotFound")
		default:
			next(w, r, a)
		}
	}
}

func (c *Cloud) list(w http.ResponseWriter, r *http.Request, _ *Appliance) {
	c.mu.Lock()
	var list []map[string]any
	for _, id := range c.order {
		a := c.appliances[id]
		list = append(list, map[string]any{"haId": a.HaID, "name": a.Name, "type": a.Type, "brand": a.Brand,
			"vib": "VIB", "enumber": "E", "connected": a.Connected})
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"homeappliances": list}})
}

func (c *Cloud) values(kind string) func(w http.ResponseWriter, r *http.Request, a *Appliance) {
	return func(w http.ResponseWriter, r *http.Request, a *Appliance) {
		c.mu.Lock()
		src := a.Status
		if kind == "settings" {
			src = a.Settings
		}
		var list []map[string]any
		for k, v := range src {
			list = append(list, map[string]any{"key": k, "value": v})
		}
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{kind: list}})
	}
}

// setting - настройка с описанием из SettingInfo. Настройки нет у прибора - 404.
func (c *Cloud) setting(w http.ResponseWriter, r *http.Request, a *Appliance) {
	key := r.PathValue("key")
	c.mu.Lock()
	hold, fail := c.holdSettings, c.failSettings[key]
	value, ok := a.Settings[key]
	data := map[string]any{"key": key, "value": value}
	for k, v := range a.SettingInfo[key] {
		data[k] = v
	}
	c.mu.Unlock()
	if hold != nil {
		<-hold
	}
	switch {
	case fail != 0:
		if fail == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "52")
		}
		apiError(w, fail, "SDK.Error.Fake")
	case !ok:
		apiError(w, http.StatusNotFound, "SDK.Error.UnsupportedSetting")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	}
}

func (c *Cloud) program(active bool) func(w http.ResponseWriter, r *http.Request, a *Appliance) {
	return func(w http.ResponseWriter, r *http.Request, a *Appliance) {
		c.mu.Lock()
		key := a.Selected
		if active {
			key = a.Active
		}
		c.mu.Unlock()
		switch {
		case key == "" && active:
			apiError(w, http.StatusNotFound, "SDK.Error.NoProgramActive")
		case key == "":
			apiError(w, http.StatusNotFound, "SDK.Error.NoProgramSelected")
		default:
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"key": key, "options": []any{}}})
		}
	}
}

func (c *Cloud) available(w http.ResponseWriter, r *http.Request, a *Appliance) {
	var list []map[string]any
	for _, p := range a.Programs {
		list = append(list, map[string]any{"key": p.Key, "constraints": map[string]any{"execution": "selectandstart"}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"programs": list}})
}

func (c *Cloud) programOptions(w http.ResponseWriter, r *http.Request, a *Appliance) {
	for _, p := range a.Programs {
		if p.Key == r.PathValue("key") {
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"key": p.Key, "options": p.Options}})
			return
		}
	}
	apiError(w, http.StatusNotFound, "SDK.Error.UnsupportedProgram")
}

func (c *Cloud) commands(w http.ResponseWriter, r *http.Request, a *Appliance) {
	var list []map[string]any
	for _, k := range a.Commands {
		list = append(list, map[string]any{"key": k, "name": k})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"commands": list}})
}

// body - поле data тела команды.
func body(r *http.Request) (map[string]any, bool) {
	if r.Header.Get("Content-Type") != mediaType {
		return nil, false
	}
	var b struct {
		Data map[string]any `json:"data"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil || b.Data["key"] == nil {
		return nil, false
	}
	return b.Data, true
}

// start запускает программу: прибор переходит в Run и сообщает об этом в поток.
func (c *Cloud) start(w http.ResponseWriter, r *http.Request, a *Appliance) {
	data, ok := body(r)
	if !ok {
		apiError(w, http.StatusBadRequest, "SDK.Error.InvalidRequest")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.Status["BSH.Common.Status.OperationState"] != "BSH.Common.EnumType.OperationState.Ready" {
		apiError(w, http.StatusConflict, "SDK.Error.WrongOperationState")
		return
	}
	key, _ := data["key"].(string)
	a.Active, a.Selected = key, key
	a.Status["BSH.Common.Status.OperationState"] = "BSH.Common.EnumType.OperationState.Run"
	w.WriteHeader(http.StatusNoContent)
	c.sendJSONLocked("NOTIFY", a.HaID, Item("BSH.Common.Root.ActiveProgram", key))
	c.sendJSONLocked("STATUS", a.HaID, Item("BSH.Common.Status.OperationState", "BSH.Common.EnumType.OperationState.Run"))
}

// stop останавливает программу: прибор проходит Aborting и через 200 мс возвращается в Ready.
func (c *Cloud) stop(w http.ResponseWriter, r *http.Request, a *Appliance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.Active == "" {
		apiError(w, http.StatusNotFound, "SDK.Error.NoProgramActive")
		return
	}
	a.Active = ""
	a.Status["BSH.Common.Status.OperationState"] = "BSH.Common.EnumType.OperationState.Aborting"
	w.WriteHeader(http.StatusNoContent)
	c.sendJSONLocked("STATUS", a.HaID, Item("BSH.Common.Status.OperationState", "BSH.Common.EnumType.OperationState.Aborting"))
	time.AfterFunc(200*time.Millisecond, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		a.Status["BSH.Common.Status.OperationState"] = "BSH.Common.EnumType.OperationState.Ready"
		c.sendJSONLocked("STATUS", a.HaID, Item("BSH.Common.Status.OperationState", "BSH.Common.EnumType.OperationState.Ready"))
	})
}

func (c *Cloud) selectProgram(w http.ResponseWriter, r *http.Request, a *Appliance) {
	data, ok := body(r)
	if !ok {
		apiError(w, http.StatusBadRequest, "SDK.Error.InvalidRequest")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a.Selected, _ = data["key"].(string)
	w.WriteHeader(http.StatusNoContent)
}

func (c *Cloud) put(w http.ResponseWriter, r *http.Request, a *Appliance) {
	data, ok := body(r)
	if !ok || data["key"] != r.PathValue("key") {
		apiError(w, http.StatusBadRequest, "SDK.Error.InvalidRequest")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a.Settings[r.PathValue("key")] = data["value"]
	w.WriteHeader(http.StatusNoContent)
	c.sendJSONLocked("NOTIFY", a.HaID, Item(r.PathValue("key"), data["value"]))
}

func (c *Cloud) command(w http.ResponseWriter, r *http.Request, a *Appliance) {
	data, ok := body(r)
	if !ok || data["value"] != true {
		apiError(w, http.StatusBadRequest, "SDK.Error.InvalidRequest")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Cloud) sendJSONLocked(typ, haID string, items ...map[string]any) {
	data, _ := json.Marshal(map[string]any{"items": items})
	c.sendLocked(typ, haID, string(data))
}

// events - поток событий: KEEP-ALIVE сразу после подключения, затем события из Send.
func (c *Cloud) events(w http.ResponseWriter, r *http.Request, _ *Appliance) {
	if r.Header.Get("Accept") != "text/event-stream" {
		apiError(w, http.StatusNotAcceptable, "SDK.Error.UnsupportedMediaType")
		return
	}
	ch := make(chan string, 64)
	c.mu.Lock()
	c.streams = append(c.streams, ch)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		for i, s := range c.streams {
			if s == ch {
				c.streams = append(c.streams[:i], c.streams[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: KEEP-ALIVE\ndata: \n\n")
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprint(w, msg)
			w.(http.Flusher).Flush()
		}
	}
}
