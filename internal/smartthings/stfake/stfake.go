// Package stfake - поддельное облако SmartThings для тестов: вход OAuth, список устройств,
// описания capability, состояние, команды и поток событий SSE так, как их описывает
// документация SmartThings.
package stfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Вход в поддельное облако.
const (
	ClientID     = "st-client"
	ClientSecret = "st-secret-0123456789"
	Code         = "CODE-1"
)

// Устройства поддельного аккаунта.
const (
	TV     = "tv-1"
	Lamp   = "lamp-1"
	Fridge = "fridge-1"
)

// Cloud - поддельное облако.
type Cloud struct {
	srv *httptest.Server

	mu       sync.Mutex
	refresh  int
	state    map[string]map[string]map[string]map[string]any
	commands []string
	streams  []chan string
	subs     int
	deleted  int
	// Calls - запросы по пути.
	calls map[string]int
}

// New запускает поддельное облако.
func New() *Cloud {
	c := &Cloud{calls: map[string]int{}, state: map[string]map[string]map[string]map[string]any{
		TV: {"main": {
			"switch":                     {"switch": "on"},
			"audioVolume":                {"volume": 15},
			"audioMute":                  {"mute": "unmuted"},
			"tvChannel":                  {"tvChannel": "5", "tvChannelName": "Первый"},
			"samsungvd.mediaInputSource": {"inputSource": "dtv", "supportedInputSourcesMap": []map[string]string{{"id": "dtv", "name": "TV"}, {"id": "HDMI1", "name": "PC"}}},
			"mediaPlayback":              {"playbackStatus": "playing"},
		}},
		Lamp: {"main": {
			"switch":           {"switch": "off"},
			"switchLevel":      {"level": 40},
			"colorControl":     {"hue": 50, "saturation": 100},
			"colorTemperature": {"colorTemperature": 3000},
		}},
		Fridge: {
			"main":    {"contactSensor": {"contact": "closed"}, "temperatureMeasurement": {"temperature": 4}},
			"freezer": {"temperatureMeasurement": {"temperature": -18}, "thermostatCoolingSetpoint": {"coolingSetpoint": -18}},
		},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", c.token)
	mux.HandleFunc("GET /v1/devices", c.auth(c.devices))
	mux.HandleFunc("GET /v1/capabilities/{id}/{version}", c.auth(c.capability))
	mux.HandleFunc("GET /v1/devices/{id}/status", c.auth(c.status))
	mux.HandleFunc("GET /v1/devices/{id}/health", c.auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"deviceId": r.PathValue("id"), "state": "ONLINE"})
	}))
	mux.HandleFunc("POST /v1/devices/{id}/commands", c.auth(c.command))
	mux.HandleFunc("POST /v1/sse/subscriptions", c.auth(c.subscribe))
	mux.HandleFunc("DELETE /v1/sse/subscriptions/{id}", c.auth(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.deleted++
		c.mu.Unlock()
	}))
	mux.HandleFunc("GET /filters/{id}/activate", c.auth(c.stream))
	c.srv = httptest.NewServer(mux)
	return c
}

// URL - адрес облака для smartthings.api_url.
func (c *Cloud) URL() string { return c.srv.URL }

// Close останавливает облако и закрывает потоки.
func (c *Cloud) Close() {
	c.mu.Lock()
	for _, s := range c.streams {
		close(s)
	}
	c.streams = nil
	c.mu.Unlock()
	c.srv.CloseClientConnections()
	c.srv.Close()
}

// Commands - команды, которые получило облако: «устройство компонент capability команда аргументы».
func (c *Cloud) Commands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.commands...)
}

// Calls - сколько запросов пришло на путь.
func (c *Cloud) Calls(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[path]
}

// Streams - сколько подписок создано и удалено.
func (c *Cloud) Streams() (created, deleted int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subs, c.deleted
}

// Goodbye закрывает поток, как SmartThings перед концом подписки.
func (c *Cloud) Goodbye() {
	c.push("event: CONTROL_EVENT\ndata: goodbye\n\n")
}

// Set меняет значение на устройстве и сообщает его в поток событий.
func (c *Cloud) Set(device, component, capability, attribute string, value any) {
	c.mu.Lock()
	c.state[device][component][capability][attribute] = value
	c.mu.Unlock()
	c.event(device, component, capability, attribute, value)
}

func (c *Cloud) event(device, component, capability, attribute string, value any) {
	data, _ := json.Marshal(map[string]any{"eventTime": "2026-11-01T10:00:00Z", "eventType": "DEVICE_EVENT",
		"deviceEvent": map[string]any{"eventId": "e1", "locationId": "loc-1", "deviceId": device, "componentId": component,
			"capability": capability, "attribute": attribute, "value": value, "stateChange": true}})
	c.push("event: DEVICE_EVENT\ndata: " + string(data) + "\n\n")
}

func (c *Cloud) push(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.streams {
		select {
		case s <- msg:
		default:
		}
	}
}

func (c *Cloud) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls[r.URL.Path]++
		want := fmt.Sprintf("Bearer access-%d", c.refresh)
		c.mu.Unlock()
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (c *Cloud) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	r.ParseForm()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !ok || id != ClientID || secret != ClientSecret || r.PostForm.Get("client_id") != ClientID {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if r.PostForm.Get("code") != Code || !strings.HasSuffix(r.PostForm.Get("redirect_uri"), "/connections/smartthings/callback") {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
	case "refresh_token":
		if r.PostForm.Get("refresh_token") != fmt.Sprintf("refresh-%d", c.refresh) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		c.refresh++
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", c.refresh),
		"refresh_token": fmt.Sprintf("refresh-%d", c.refresh), "expires_in": 86399, "installed_app_id": "app-1"})
}

func caps(ids ...string) []map[string]any {
	var out []map[string]any
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "version": 1})
	}
	return out
}

func component(id, category string, capabilities ...string) map[string]any {
	c := map[string]any{"id": id, "label": id, "capabilities": caps(capabilities...), "categories": []map[string]string{}}
	if category != "" {
		c["categories"] = []map[string]string{{"name": category, "categoryType": "manufacturer"}}
	}
	return c
}

func (c *Cloud) devices(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
		{"deviceId": TV, "name": "Samsung TV", "label": "Телевизор", "manufacturerName": "Samsung Electronics",
			"components": []map[string]any{component("main", "Television", "switch", "audioVolume", "audioMute", "tvChannel",
				"samsungvd.mediaInputSource", "mediaPlayback")}},
		{"deviceId": Lamp, "name": "Bulb", "label": "Лампа", "manufacturerName": "IKEA",
			"components": []map[string]any{component("main", "Light", "switch", "switchLevel", "colorControl", "colorTemperature")}},
		{"deviceId": Fridge, "name": "Fridge", "label": "Холодильник", "manufacturerName": "Samsung Electronics",
			"components": []map[string]any{component("main", "Refrigerator", "contactSensor", "temperatureMeasurement"),
				component("freezer", "", "temperatureMeasurement", "thermostatCoolingSetpoint")}},
	}, "_links": map[string]any{}})
}

func str(enum ...string) map[string]any {
	s := map[string]any{"type": "string"}
	if len(enum) > 0 {
		s["enum"] = enum
	}
	return s
}

func num(typ string, lo, hi float64) map[string]any {
	return map[string]any{"type": typ, "minimum": lo, "maximum": hi}
}

func attr(value map[string]any, setter string, enum ...[2]string) map[string]any {
	a := map[string]any{"schema": map[string]any{"type": "object", "properties": map[string]any{"value": value}}}
	if setter != "" {
		a["setter"] = setter
	}
	var ec []map[string]string
	for _, e := range enum {
		ec = append(ec, map[string]string{"command": e[0], "value": e[1]})
	}
	if ec != nil {
		a["enumCommands"] = ec
	}
	return a
}

func cmd(args ...map[string]any) map[string]any {
	if args == nil {
		args = []map[string]any{}
	}
	return map[string]any{"arguments": args}
}

func arg(name string, schema map[string]any) map[string]any {
	return map[string]any{"name": name, "optional": false, "schema": schema}
}

// defs - описания capability поддельного облака.
var defs = map[string]map[string]any{
	"switch": {"attributes": map[string]any{"switch": attr(str("on", "off"), "", [2]string{"on", "on"}, [2]string{"off", "off"})},
		"commands": map[string]any{"on": cmd(), "off": cmd()}},
	"audioVolume": {"attributes": map[string]any{"volume": attr(num("integer", 0, 100), "setVolume")},
		"commands": map[string]any{"setVolume": cmd(arg("volume", num("integer", 0, 100))), "volumeUp": cmd(), "volumeDown": cmd()}},
	"audioMute": {"attributes": map[string]any{"mute": attr(str("muted", "unmuted"), "setMute", [2]string{"mute", "muted"}, [2]string{"unmute", "unmuted"})},
		"commands": map[string]any{"mute": cmd(), "unmute": cmd(), "setMute": cmd(arg("state", str("muted", "unmuted")))}},
	"tvChannel": {"attributes": map[string]any{"tvChannel": attr(str(), "setTvChannel"), "tvChannelName": attr(str(), "")},
		"commands": map[string]any{"setTvChannel": cmd(arg("tvChannel", str())), "channelUp": cmd(), "channelDown": cmd()}},
	"samsungvd.mediaInputSource": {"attributes": map[string]any{"inputSource": attr(str(), "setInputSource"),
		"supportedInputSourcesMap": attr(map[string]any{"type": "array"}, "")},
		"commands": map[string]any{"setInputSource": cmd(arg("id", str()))}},
	"mediaPlayback": {"attributes": map[string]any{"playbackStatus": attr(str("paused", "playing", "stopped"), "setPlaybackStatus",
		[2]string{"play", "playing"}, [2]string{"pause", "paused"}, [2]string{"stop", "stopped"})},
		"commands": map[string]any{"play": cmd(), "pause": cmd(), "stop": cmd(), "setPlaybackStatus": cmd(arg("status", str("paused", "playing", "stopped")))}},
	"switchLevel": {"attributes": map[string]any{"level": attr(num("integer", 0, 100), "setLevel")},
		"commands": map[string]any{"setLevel": cmd(arg("level", num("integer", 0, 100)))}},
	"colorControl": {"attributes": map[string]any{"hue": attr(num("number", 0, 100), "setHue"), "saturation": attr(num("number", 0, 100), "setSaturation")},
		"commands": map[string]any{"setColor": cmd(arg("color", map[string]any{"type": "object"})), "setHue": cmd(arg("hue", num("number", 0, 100))),
			"setSaturation": cmd(arg("saturation", num("number", 0, 100)))}},
	"colorTemperature": {"attributes": map[string]any{"colorTemperature": attr(num("integer", 1, 30000), "setColorTemperature")},
		"commands": map[string]any{"setColorTemperature": cmd(arg("temperature", num("integer", 1, 30000)))}},
	"contactSensor":          {"attributes": map[string]any{"contact": attr(str("closed", "open"), "")}, "commands": map[string]any{}},
	"temperatureMeasurement": {"attributes": map[string]any{"temperature": attr(num("number", -460, 10000), "")}, "commands": map[string]any{}},
	"thermostatCoolingSetpoint": {"attributes": map[string]any{"coolingSetpoint": attr(num("number", -460, 10000), "setCoolingSetpoint")},
		"commands": map[string]any{"setCoolingSetpoint": cmd(arg("setpoint", num("number", -460, 10000)))}},
}

func (c *Cloud) capability(w http.ResponseWriter, r *http.Request) {
	d, ok := defs[r.PathValue("id")]
	if !ok || r.PathValue("version") != "1" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	out := map[string]any{"id": r.PathValue("id"), "version": 1}
	for k, v := range d {
		out[k] = v
	}
	json.NewEncoder(w).Encode(out)
}

func (c *Cloud) status(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dev, ok := c.state[r.PathValue("id")]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	out := map[string]any{}
	for comp, capsState := range dev {
		cm := map[string]any{}
		for capability, attrs := range capsState {
			am := map[string]any{}
			for a, v := range attrs {
				av := map[string]any{"value": v}
				if a == "temperature" || a == "coolingSetpoint" {
					av["unit"] = "C"
				}
				am[a] = av
			}
			cm[capability] = am
		}
		out[comp] = cm
	}
	json.NewEncoder(w).Encode(map[string]any{"components": out})
}

// effects - что меняет команда: capability -> команда -> атрибут и значение; пустое значение -
// значение из аргумента.
var effects = map[string]map[string][2]string{
	"switch":                     {"on": {"switch", "on"}, "off": {"switch", "off"}},
	"audioVolume":                {"setVolume": {"volume", ""}},
	"audioMute":                  {"mute": {"mute", "muted"}, "unmute": {"mute", "unmuted"}},
	"tvChannel":                  {"setTvChannel": {"tvChannel", ""}},
	"samsungvd.mediaInputSource": {"setInputSource": {"inputSource", ""}},
	"mediaPlayback":              {"pause": {"playbackStatus", "paused"}, "play": {"playbackStatus", "playing"}},
	"switchLevel":                {"setLevel": {"level", ""}},
	"colorTemperature":           {"setColorTemperature": {"colorTemperature", ""}},
	"thermostatCoolingSetpoint":  {"setCoolingSetpoint": {"coolingSetpoint", ""}},
}

func (c *Cloud) command(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Commands []struct {
			Component  string `json:"component"`
			Capability string `json:"capability"`
			Command    string `json:"command"`
			Arguments  []any  `json:"arguments"`
		} `json:"commands"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Commands) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	k := body.Commands[0]
	args, _ := json.Marshal(k.Arguments)
	c.mu.Lock()
	c.commands = append(c.commands, fmt.Sprintf("%s %s %s %s %s", id, k.Component, k.Capability, k.Command, args))
	c.mu.Unlock()
	if e, ok := effects[k.Capability][k.Command]; ok {
		var v any = e[1]
		if e[1] == "" && len(k.Arguments) > 0 {
			v = k.Arguments[0]
		}
		c.Set(id, k.Component, k.Capability, e[0], v)
	}
	if k.Capability == "colorControl" && k.Command == "setColor" && len(k.Arguments) > 0 {
		if m, ok := k.Arguments[0].(map[string]any); ok {
			c.Set(id, k.Component, "colorControl", "hue", m["hue"])
			c.Set(id, k.Component, "colorControl", "saturation", m["saturation"])
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{{"id": "r1", "status": "ACCEPTED"}}})
}

func (c *Cloud) subscribe(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.subs++
	n := c.subs
	c.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"subscriptionId": fmt.Sprintf("sub-%d", n),
		"registrationUrl": fmt.Sprintf("%s/filters/%d/activate?integration=true", c.srv.URL, n), "version": 1})
}

func (c *Cloud) stream(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("User-Agent") == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ch := make(chan string, 64)
	c.mu.Lock()
	c.streams = append(c.streams, ch)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: CONTROL_EVENT\ndata: welcome\n\n")
	f.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, msg)
			f.Flush()
			if strings.Contains(msg, "goodbye") {
				c.mu.Lock()
				for i, s := range c.streams {
					if s == ch {
						c.streams = append(c.streams[:i], c.streams[i+1:]...)
						break
					}
				}
				c.mu.Unlock()
				return
			}
		}
	}
}
