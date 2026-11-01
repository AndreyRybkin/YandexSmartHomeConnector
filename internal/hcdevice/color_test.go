package hcdevice

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/profile"
)

// stepper - прибор без текущего канала, который переключает его шагом, как телевизор пультом.
type stepper struct {
	*props
}

func (s stepper) Step(_ context.Context, key string, delta float64) error {
	s.sets = append(s.sets, fmt.Sprintf("step %s %v", key, delta))
	return nil
}

func lamp(t *testing.T, values map[string]any) (*Device, *props) {
	t.Helper()
	a := newProps(values)
	a.desc = appliance.Description{Type: "SmartThings.Light", Options: map[string]appliance.Option{
		"main.switch.switch":                     {Values: []string{"on", "off"}, Setting: true},
		"main.switchLevel.level":                 {Min: 0, Max: 100, Step: 1, Setting: true},
		"main.colorTemperature.colorTemperature": {Min: 2200, Max: 6500, Setting: true},
		"main.colorControl.setColor":             {Setting: true},
		"main.mediaPlayback.playbackStatus":      {Values: []string{"playing", "paused"}, Setting: true},
	}, Reports: []string{"main.switch.switch", "main.switchLevel.level", "main.colorTemperature.colorTemperature",
		"main.colorControl.hue", "main.colorControl.saturation", "main.mediaPlayback.playbackStatus"}}
	hueMax := 100.0
	p, err := profile.Resolve("X", &profile.Profile{YandexType: "light", OnOff: "power",
		Power:  &profile.Power{Setting: "main.switch.switch", On: "on", Off: "off"},
		Ranges: []profile.Range{{Instance: "brightness", Option: "main.switchLevel.level", Unit: "unit.percent"}},
		Toggles: []profile.Toggle{{Instance: "pause", Setting: "main.mediaPlayback.playbackStatus", On: "paused", Off: "playing"},
			{Instance: "backlight", Setting: "main.light.missing", On: "on", Off: "off"}},
		Color: &profile.Color{Setting: "main.colorControl.setColor", Hue: "main.colorControl.hue",
			Saturation: "main.colorControl.saturation", HueMax: &hueMax, Temperature: "main.colorTemperature.colorTemperature"},
		Properties: []profile.Property{{Type: "float", Instance: "temperature", Status: "main.temperatureMeasurement.temperature",
			Unit: "unit.temperature.celsius"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Device{ID: "lamp", Name: "Лампа", Profile: p, Appliance: a}, a
}

// Цвет: HSV в шкале прибора, температура в пределах прибора, переключатели и свойства - только
// те, что прибор сообщает.
func TestColor(t *testing.T) {
	d, a := lamp(t, map[string]any{"main.switch.switch": "on", "main.switchLevel.level": 80.0,
		"main.colorControl.hue": 50.0, "main.colorControl.saturation": 100.0,
		"main.colorTemperature.colorTemperature": 4000.0, "main.mediaPlayback.playbackStatus": "paused"})
	raw, _ := json.Marshal(d.Info()[0].Capabilities)
	want := `[{"type":"devices.capabilities.on_off","retrievable":true,"reportable":true},` +
		`{"type":"devices.capabilities.range","retrievable":true,"reportable":true,"parameters":{"instance":"brightness","unit":"unit.percent","random_access":true,"range":{"min":0,"max":100,"precision":1}}},` +
		`{"type":"devices.capabilities.toggle","retrievable":true,"reportable":true,"parameters":{"instance":"pause"}},` +
		`{"type":"devices.capabilities.color_setting","retrievable":true,"reportable":true,"parameters":{"color_model":"hsv","temperature_k":{"min":2200,"max":6500}}}]`
	if string(raw) != want {
		t.Fatalf("умения: %s", raw)
	}
	if len(d.Info()[0].Properties) != 0 {
		t.Fatal("свойство, которого прибор не сообщает, попало в Алису")
	}
	raw, _ = json.Marshal(d.States()[0].Capabilities[2:])
	want = `[{"type":"devices.capabilities.toggle","state":{"instance":"pause","value":true}},` +
		`{"type":"devices.capabilities.color_setting","state":{"instance":"hsv","value":{"h":180,"s":100,"v":80}}}]`
	if string(raw) != want {
		t.Fatalf("состояние: %s", raw)
	}
	// Белый свет - нулевая насыщенность: Алиса видит температуру.
	a.state.Values["main.colorControl.saturation"] = 0.0
	if c := d.States()[0].Capabilities[3]; c.State.(alice.StateValue).Instance != "temperature_k" || c.State.(alice.StateValue).Value != 4000 {
		t.Fatalf("температура: %+v", c)
	}
	for _, c := range []struct {
		instance string
		value    any
		want     string
	}{
		{"hsv", map[string]any{"h": 90.0, "s": 50.0, "v": 100.0}, ""},
		{"temperature_k", 2700.0, ""},
		{"temperature_k", 9000.0, alice.CodeInvalidValue},
		{"hsv", map[string]any{"h": 400.0, "s": 50.0}, alice.CodeInvalidValue},
		{"rgb", 255.0, alice.CodeInvalidAction},
	} {
		if code := act(t, d, alice.CapColor, c.instance, c.value, false); code != c.want {
			t.Fatalf("%s %v: %q, нужно %q", c.instance, c.value, code, c.want)
		}
	}
	if code := act(t, d, alice.CapToggle, "pause", false, false); code != "" {
		t.Fatalf("пауза: %s", code)
	}
	want = fmt.Sprint([]string{"main.colorControl.setColor=map[hue:25 saturation:50]", "main.colorTemperature.colorTemperature=2700",
		"main.mediaPlayback.playbackStatus=playing"})
	if fmt.Sprint(a.sets) != want {
		t.Fatalf("команды: %v", a.sets)
	}
}

// «Следующий канал» без текущего канала уходит прибору шагом, а с известным каналом - числом.
func TestStep(t *testing.T) {
	a := stepper{newProps(map[string]any{})}
	a.desc = appliance.Description{Type: "TV", Options: map[string]appliance.Option{
		"TV.Channel": {Min: 1, Max: 999, Step: 1, Setting: true}}}
	p, err := profile.Resolve("X", &profile.Profile{YandexType: "media_device.tv",
		Ranges: []profile.Range{{Instance: "channel", Option: "TV.Channel"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Device{ID: "tv", Name: "Телевизор", Profile: p, Appliance: a}
	if code := act(t, d, alice.CapRange, "channel", 1.0, true); code != "" {
		t.Fatalf("шаг: %s", code)
	}
	a.state.Values["TV.Channel"] = 5.0
	if code := act(t, d, alice.CapRange, "channel", -1.0, true); code != "" {
		t.Fatalf("канал: %s", code)
	}
	if want := []string{"step TV.Channel 1", "TV.Channel=4"}; !slices.Equal(a.sets, want) {
		t.Fatalf("команды: %v", a.sets)
	}
}
