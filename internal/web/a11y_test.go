package web

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Кнопка питания на плитке со скриптом получает итог команды в JSON, без скрипта - переход
// со списком приборов.
func TestCommandJSON(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	f.get("/")
	send := func(form url.Values) (int, map[string]any) {
		form.Set("csrf", f.csrf)
		req, _ := http.NewRequest(http.MethodPost, f.hs.URL+"/devices/oven/command", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("ответ не JSON: %d %s", resp.StatusCode, body)
		}
		return resp.StatusCode, out
	}
	status, out := send(url.Values{"type": {"devices.capabilities.on_off"}, "instance": {"on"}, "value": {"true"}, "from": {"list"}})
	if status != http.StatusOK || out["ok"] != true || out["text"] != "Команда выполнена." {
		t.Fatalf("команда: %d %v", status, out)
	}
	if _, out = send(url.Values{"type": {"devices.capabilities.nothing"}, "from": {"list"}}); out["ok"] != false ||
		!strings.Contains(out["text"].(string), "неизвестное умение") {
		t.Fatalf("неверная команда: %v", out)
	}
	if _, _, to := f.post("/devices/oven/command", url.Values{"type": {"devices.capabilities.on_off"}, "instance": {"on"},
		"value": {"false"}, "from": {"list"}}); to != "/#d-oven" {
		t.Fatalf("без скрипта: переход %s", to)
	}
}

var (
	themeRe = regexp.MustCompile(`:root(\[data-theme=dark\])?\{(color-scheme:(?:light|dark);[^}]*)\}`)
	tokenRe = regexp.MustCompile(`--([a-z0-9-]+):#([0-9a-f]{6}|[0-9a-f]{3})[;}]?`)
)

// luminance - относительная яркость цвета #rrggbb по WCAG.
func luminance(hex string) float64 {
	var out float64
	for i, w := range []float64{0.2126, 0.7152, 0.0722} {
		v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		c := float64(v) / 255
		if c <= 0.04045 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		out += w * c
	}
	return out
}

// Контраст текста и фона обеих тем - не ниже 4,5:1 (WCAG AA).
func TestContrast(t *testing.T) {
	data, err := os.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	themes := themeRe.FindAllStringSubmatch(string(data), -1)
	if len(themes) != 2 {
		t.Fatalf("тем в стилях: %d, нужно светлая и тёмная", len(themes))
	}
	pairs := [][2]string{{"fg", "bg"}, {"fg", "surface"}, {"fg", "surface-2"}, {"muted", "bg"}, {"muted", "surface"},
		{"accent", "bg"}, {"accent", "surface"}, {"accent-fg", "accent"}, {"ok", "ok-bg"}, {"ok", "surface"}, {"ok-fg", "ok"},
		{"note", "note-bg"}, {"note", "surface"}, {"err", "err-bg"}, {"err", "surface"}}
	for _, th := range themes {
		name := "светлая"
		if th[1] != "" {
			name = "тёмная"
		}
		colors := map[string]string{}
		for _, m := range tokenRe.FindAllStringSubmatch(th[2], -1) {
			hex := m[2]
			if len(hex) == 3 {
				hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
			}
			colors[m[1]] = "#" + hex
		}
		for _, p := range pairs {
			fg, bg := colors[p[0]], colors[p[1]]
			if fg == "" || bg == "" {
				t.Fatalf("%s тема: нет --%s или --%s", name, p[0], p[1])
			}
			a, b := luminance(fg), luminance(bg)
			ratio := (math.Max(a, b) + 0.05) / (math.Min(a, b) + 0.05)
			if ratio < 4.5 {
				t.Errorf("%s тема: --%s %s на --%s %s - контраст %.2f, нужно не ниже 4,5", name, p[0], fg, p[1], bg, ratio)
			}
		}
	}
}
