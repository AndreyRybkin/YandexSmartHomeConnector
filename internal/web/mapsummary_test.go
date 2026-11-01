package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/profile"
)

func TestCount(t *testing.T) {
	for n, want := range map[int]string{1: "1 значение", 3: "3 значения", 5: "5 значений", 11: "11 значений", 21: "21 значение",
		22: "22 значения", 112: "112 значений"} {
		if got := count(n, "значение", "значения", "значений"); got != want {
			t.Errorf("count(%d) = %q, нужно %q", n, got, want)
		}
	}
}

// Сводка на вкладке «Сопоставления»: функция прибора и её значение, пересчёт и умение Алисы.
// У полей редактора - значение сейчас и пределы, у показания - пересчёт значения сейчас.
func TestMapSummary(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/devices/oven")
	for _, want := range []string{`id="summary"`, `<tbody id="mappings">`, `<td>программа прибора</td><td>не идёт</td>`,
		`<code title="Cooking.Oven.Status.CurrentCavityTemperature">CurrentCavityTemperature</code></td><td>22</td>`, `<a href="#properties">Изменить</a>`,
		`<datalist id="dl-numeric" data-keys>`, `<option value="Cooking.Oven.Option.SetpointTemperature" label="от 30 до 275">`,
		`<option value="Cooking.Oven.Status.CurrentCavityTemperature" label="сейчас 22" data-now="22">`,
		`<span class="hint" data-hint>сейчас 22</span>`, `<p class="hint" data-result data-raw="22">сейчас 22 → 22 °C</p>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице прибора нет %s", want)
		}
	}
	// Работающая программа отмечена в таблице режима.
	form := url.Values{"type": {"devices.capabilities.on_off"}, "instance": {"on"}, "value": {"true"}}
	f.post("/devices/oven/command", form)
	if _, body = f.get("/devices/oven"); !strings.Contains(body, `<span class="chip run">сейчас</span>`) {
		t.Fatal("работающая программа не отмечена")
	}
}

// Предупреждения редактора: функция не того вида и функция, которую меняют два умения.
func TestEditorWarnings(t *testing.T) {
	tg := &editTarget{builtin: &profile.Profile{YandexType: "cooking", OnOff: "none",
		Ranges:     []profile.Range{{Instance: "temperature", Option: "flag"}, {Instance: "brightness", Option: "level"}},
		Toggles:    []profile.Toggle{{Instance: "backlight", Setting: "level"}, {Instance: "keep_warm", Setting: "mode"}},
		Properties: []profile.Property{{Type: "float", Instance: "temperature", Status: "mode"}}},
		desc: appliance.Description{Options: map[string]appliance.Option{"mode": {Values: []string{"A", "B", "C"}}, "level": {Min: 0, Max: 100}}},
		now:  map[string]any{"flag": true, "level": 40.0, "mode": "A"}}
	v := buildEditor(tg, "", "", nil, "", "")
	for i, want := range []string{"у функции значения да/нет: числу Алисы она не подходит", "эту функцию уже меняет «Переключатель «Подсветка»»"} {
		if !strings.Contains(v.Ranges[i].Warn, want) {
			t.Fatalf("число %d: %q, нужно %q", i, v.Ranges[i].Warn, want)
		}
	}
	if w := v.Toggles[0].Warn; !strings.Contains(w, "у функции число") || !strings.Contains(w, "«Число «Яркость»»") {
		t.Fatalf("переключатель на число: %q", w)
	}
	if w := v.Toggles[1].Warn; !strings.Contains(w, "задайте значения «включено» и «выключено»") {
		t.Fatalf("переключатель на список: %q", w)
	}
	if v.Properties[0].Fit != "у функции значения из списка: показанию Алисы она не подходит" {
		t.Fatalf("показание на список: %q", v.Properties[0].Fit)
	}
	if v.Ranges[1].Hint != "сейчас 40 · от 0 до 100" {
		t.Fatalf("подсказка: %q", v.Ranges[1].Hint)
	}
}

// Предпросмотр словами: что добавится, изменится и пропадёт; JSON - свёрнутым блоком.
func TestPreviewWords(t *testing.T) {
	cur := []alice.DeviceInfo{{ID: "oven", Name: "Духовка", Type: "devices.types.cooking", Capabilities: []alice.Capability{
		{Type: alice.CapOnOff},
		{Type: alice.CapRange, Parameters: alice.RangeParams{Instance: "temperature", Unit: "unit.temperature.celsius", Range: alice.RangeLimit{Min: 30, Max: 275, Precision: 5}}},
		{Type: alice.CapToggle, Parameters: alice.InstanceParams{Instance: "pause"}}}}}
	next := []alice.DeviceInfo{{ID: "oven", Name: "Духовка", Type: "devices.types.cooking.kettle", Capabilities: []alice.Capability{
		{Type: alice.CapOnOff},
		{Type: alice.CapRange, Parameters: alice.RangeParams{Instance: "temperature", Unit: "unit.temperature.celsius", Range: alice.RangeLimit{Min: 40, Max: 250, Precision: 5}}},
		{Type: alice.CapToggle, Parameters: alice.InstanceParams{Instance: "backlight"}}}}}
	v := previewOf(next, cur, "{}")
	got := map[string]string{}
	for _, it := range v.Devices[0].Items {
		got[it.Tech] = it.Change
	}
	if got["on_off"] != "" || got["range temperature"] != "изменится" || got["toggle backlight"] != "добавится" ||
		got["toggle pause"] != "пропадёт" || v.Devices[0].TypeChange != "изменится" {
		t.Fatalf("предпросмотр: %v, тип %q", got, v.Devices[0].TypeChange)
	}
	if previewOf(next, cur, "") != nil {
		t.Fatal("предпросмотр без JSON")
	}
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	f.get("/devices/oven")
	status, body, _ := f.post("/devices/oven/profile/yandex_type", url.Values{"yandex_type": {"cooking.kettle"}, "op": {"preview"}})
	if status != http.StatusOK || !strings.Contains(body, `<span class="chip warn">тип изменится</span>`) ||
		!strings.Contains(body, "<summary>JSON для Яндекса</summary>") || !strings.Contains(body, "не изменится") {
		t.Fatalf("предпросмотр на странице: %d", status)
	}
}
