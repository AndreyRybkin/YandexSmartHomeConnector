package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/appliance"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/profile"
)

// tileAppliance - прибор с заданными значениями.
type tileAppliance struct {
	*appliance.Demo
	values  map[string]any
	offline bool
}

func (a *tileAppliance) State() appliance.State {
	return appliance.State{Online: !a.offline, Values: a.values}
}

func (a *tileAppliance) Describe() appliance.Description {
	options := map[string]appliance.Option{}
	for k, v := range a.values {
		if _, ok := v.(float64); ok {
			options[k] = appliance.Option{Key: k, Min: -300, Max: 3000, Step: 1, Setting: true}
		}
	}
	return appliance.Description{Type: "X", Options: options}
}

func TestTileText(t *testing.T) {
	builtin := func(name string) *profile.Profile {
		t.Helper()
		p, err := profile.Resolve(name, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	kettle := map[string]any{"sensor/temperature": 62.4, "weight": 1105.0, "mode": "3", "temperature": 80.0}
	sensor := &profile.Profile{YandexType: "sensor.climate", OnOff: "none", Properties: []profile.Property{
		{Type: "event", Instance: "open", Status: "c", Values: map[string]string{"open": "opened", "closed": "closed"}},
		{Type: "float", Instance: "battery_level", Status: "b", Unit: "unit.percent"},
		{Type: "float", Instance: "my_counter", Status: "n"},
		{Type: "float", Instance: "temperature", Status: "t", Unit: "unit.temperature.celsius"},
		{Type: "float", Instance: "humidity", Status: "h", Unit: "unit.percent"}}}
	for _, c := range []struct {
		name   string
		p      *profile.Profile
		values map[string]any
		st     status
		want   string
	}{
		{"чайник греет: температура, вода, цель", builtin(profile.PolarisKettleType), kettle, status{Online: true, Switch: true, On: true},
			"62.4 °C · вода 65 % · цель 80 °C"},
		{"чайник с тревогой воды: уровень только в метке", builtin(profile.PolarisKettleType),
			map[string]any{"sensor/temperature": 62.4, "weight": 1105.0, "mode": "3", "temperature": 80.0, "Polaris.Alarm.Water": appliance.EventPresent},
			status{Online: true, Switch: true, On: true}, "62.4 °C · вода 65 % · цель 80 °C"},
		{"чайник выключен: без цели", builtin(profile.PolarisKettleType), kettle, status{Online: true, Switch: true},
			"62.4 °C · вода 65 %"},
		{"нет связи: показаний нет", builtin(profile.PolarisKettleType), kettle, status{}, ""},
		{"датчик: событие всегда, затем три числа не помещаются", sensor,
			map[string]any{"c": "closed", "t": 21.0, "h": 40.0, "b": 90.0, "n": 5.0}, status{Online: true},
			"закрыто · 21 °C · влажность 40 %"},
		{"низкий заряд есть в метке: на плитке его нет", sensor, map[string]any{"b": 10.0, "n": 5.0},
			status{Online: true, Problem: "battery"}, "my_counter 5"},
		{"холодильник: дверца только открытая, настройка видна", builtin("Refrigerator"),
			map[string]any{"Refrigeration.FridgeFreezer.Setting.SetpointTemperatureRefrigerator": 4.0,
				"Refrigeration.Common.Status.Door.Refrigerator": "BSH.Common.EnumType.DoorState.Open"}, status{Online: true},
			"дверца открыта · задано 4 °C"},
		{"холодильник с закрытой дверцей", builtin("Refrigerator"),
			map[string]any{"Refrigeration.FridgeFreezer.Setting.SetpointTemperatureRefrigerator": 4.0,
				"Refrigeration.Common.Status.Door.Refrigerator": "BSH.Common.EnumType.DoorState.Closed"}, status{Online: true},
			"задано 4 °C"},
		{"своё значение прибора с подписью и единицей", &profile.Profile{YandexType: "other", OnOff: "none",
			Tile: []profile.TileItem{{Status: "p", Label: "налито", Unit: "unit.percent"}, {Status: "нет"}}},
			map[string]any{"p": 40.4}, status{Online: true}, "налито 40 %"},
		{"выбранное показание: порядок и подпись", &profile.Profile{YandexType: "other", OnOff: "none",
			Properties: sensor.Properties, Tile: []profile.TileItem{{Item: "float/humidity", Label: "воздух"}, {Item: "float/temperature"}}},
			map[string]any{"t": 21.0, "h": 40.0, "b": 90.0}, status{Online: true}, "воздух 40 % · 21 °C"},
	} {
		t.Run(c.name, func(t *testing.T) {
			demo, err := appliance.NewDemo("Oven", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			a := &tileAppliance{Demo: demo, values: c.values, offline: !c.st.Online}
			hc := &hcdevice.Device{ID: "d", Profile: c.p, Appliance: a}
			if got := tileText(hc, &c.st); got != c.want {
				t.Fatalf("%q, нужно %q", got, c.want)
			}
		})
	}
}

// Плитка показывает значения и в разметке, и в потоке обновлений; раздел редактора сохраняет выбор.
func TestTileEditor(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	f.get("/devices/oven")
	form := url.Values{"op": {"save"}, "t.0.item": {"float/temperature"}, "t.0.label": {"в камере"},
		"t.1.item": {"status"}, "t.1.status": {"Cooking.Oven.Status.CurrentCavityTemperature"}, "t.1.unit": {"unit.temperature.celsius"}, "t.2.item": {""}}
	if status, _, _ := f.post("/devices/oven/profile/tile", form); status != 303 {
		t.Fatalf("сохранение: %d", status)
	}
	items := f.config().Devices["oven"].Profile.Tile
	if len(items) != 2 || items[0].Item != "float/temperature" || items[0].Label != "в камере" || items[1].Unit != "unit.temperature.celsius" {
		t.Fatalf("в файле: %+v", items)
	}
	_, body := f.get("/")
	if !strings.Contains(body, `<p class="vals">в камере`) {
		t.Fatalf("на плитке нет показаний: %s", body)
	}
	_, body = f.get("/devices/oven")
	for _, want := range []string{`id="tile"`, `name="t.0.label" value="в камере"`, `<option value="float/temperature" selected>`, `<option value="status" selected>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("в редакторе нет %s", want)
		}
	}
	// Больше трёх показаний и чужой вид не принимаются.
	if err := (&profile.Profile{YandexType: "other", OnOff: "none", Tile: make([]profile.TileItem, 4)}).Validate(); err == nil {
		t.Fatal("четыре показания приняты")
	}
	for _, it := range []profile.TileItem{{}, {Item: "a", Status: "b"}, {Item: "плохо"}, {Item: "float/x", Unit: "unit.percent"},
		{Status: "s", Unit: "percent"}, {Status: "s", Label: strings.Repeat("я", 33)}} {
		if err := (&profile.Profile{YandexType: "other", OnOff: "none", Tile: []profile.TileItem{it}}).Validate(); err == nil {
			t.Fatalf("принято %+v", it)
		}
	}
}
