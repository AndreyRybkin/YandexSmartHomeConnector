package hcdevice

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/profile"
)

// described - демо-прибор с заданными описанием и значениями.
type described struct {
	*appliance.Demo
	desc   appliance.Description
	values map[string]any
}

func (f *described) Describe() appliance.Description { return f.desc }

func (f *described) State() appliance.State {
	return appliance.State{Online: true, Values: f.values}
}

func describedDevice(t *testing.T, kind string, desc appliance.Description, values map[string]any) *Device {
	t.Helper()
	d := newDevice(t, "x", "Oven")
	d.Profile = profile.BuiltinProfile(kind)
	d.Appliance = &described{Demo: d.Appliance.(*appliance.Demo), desc: desc, values: values}
	return d
}

func infoJSON(t *testing.T, d *Device) string {
	t.Helper()
	raw, err := json.Marshal(d.Info())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Прибор без новых настроек не получает новых умений, а прибор с ними получает.
func TestNewSkillsOnlyWithDescription(t *testing.T) {
	const light = "Refrigeration.Common.Setting.Light.Internal.Power"
	const bright = "Refrigeration.Common.Setting.Light.Internal.Brightness"
	bare := infoJSON(t, describedDevice(t, "Refrigerator", appliance.Description{Options: map[string]appliance.Option{}}, nil))
	if strings.Contains(bare, "backlight") || strings.Contains(bare, "brightness") {
		t.Fatalf("умения без настройки: %s", bare)
	}
	full := infoJSON(t, describedDevice(t, "Refrigerator", appliance.Description{Options: map[string]appliance.Option{
		light:  {Key: light},
		bright: {Key: bright, Min: 0, Max: 100, Step: 1},
	}}, nil))
	if !strings.Contains(full, "backlight") || !strings.Contains(full, "brightness") {
		t.Fatalf("нет умений: %s", full)
	}
	// Настройка только для чтения и настройка без пределов умения не дают.
	ro := infoJSON(t, describedDevice(t, "Refrigerator", appliance.Description{Options: map[string]appliance.Option{
		light:  {Key: light, ReadOnly: true},
		bright: {Key: bright},
	}}, nil))
	if strings.Contains(ro, "backlight") || strings.Contains(ro, "brightness") {
		t.Fatalf("умения без права записи или пределов: %s", ro)
	}

	// Режим появляется по значениям прибора: Roxxter не получает значений Spotless и скорости всасывания.
	const mode = "ConsumerProducts.CleaningRobot.Option.CleaningMode"
	robot := infoJSON(t, describedDevice(t, "CleaningRobot", appliance.Description{Options: map[string]appliance.Option{
		mode: {Key: mode, Values: []string{"ConsumerProducts.CleaningRobot.EnumType.CleaningModes.Silent"}},
	}}, nil))
	if !strings.Contains(robot, "cleanup_mode") || !strings.Contains(robot, "quiet") ||
		strings.Contains(robot, "wet_cleaning") || strings.Contains(robot, "work_speed") {
		t.Fatalf("робот: %s", robot)
	}
}

// Встроенная вытяжка панели: Off не имеет пары, значит, состояния режима нет, а команда меняет настройку.
func TestHobVentilation(t *testing.T) {
	const key = "Cooking.Hob.Setting.Ventilation"
	values := []string{"Cooking.Hob.EnumType.Ventilation.Off", "Cooking.Hob.EnumType.Ventilation.Level01",
		"Cooking.Hob.EnumType.Ventilation.BoostLevel1"}
	a := &fixedOptions{options: map[string][]string{key: values},
		values: map[string]any{key: "Cooking.Hob.EnumType.Ventilation.Off"}}
	d := newDevice(t, "x", "Oven")
	a.Demo = d.Appliance.(*appliance.Demo)
	d.Appliance, d.Profile = a, profile.BuiltinProfile("Hob")
	if info := infoJSON(t, d); !strings.Contains(info, "fan_speed") || !strings.Contains(info, "turbo") || strings.Contains(info, `"max"`) {
		t.Fatalf("панель: %s", info)
	}
	raw, _ := json.Marshal(d.States())
	if strings.Contains(string(raw), "fan_speed") {
		t.Fatalf("состояние без пары: %s", raw)
	}
	code := d.Act(context.Background(), "x", &alice.Action{Type: alice.CapMode,
		State: alice.ActionState{Instance: "fan_speed", Value: "turbo"}})
	if code != "" || len(a.set) != 1 || a.set[0] != key+"=Cooking.Hob.EnumType.Ventilation.BoostLevel1" {
		t.Fatalf("команда: %q %v", code, a.set)
	}
}

// Дверца духовки с номером камеры в имени: свойство показывает её, а запуск отказывает.
func TestCavityDoor(t *testing.T) {
	const cavity = "Cooking.Oven.Status.Cavity.001.DoorState"
	d := describedDevice(t, "Oven", appliance.Description{
		Programs: []string{"Cooking.Oven.Program.HeatingMode.HotAir"},
		Options:  map[string]appliance.Option{},
	}, map[string]any{cavity: appliance.DoorOpen})
	raw, _ := json.Marshal(d.States())
	if !strings.Contains(string(raw), `"instance":"open","value":"opened"`) {
		t.Fatalf("состояние: %s", raw)
	}
	code := d.Act(context.Background(), "x", &alice.Action{Type: alice.CapOnOff,
		State: alice.ActionState{Instance: "on", Value: true}})
	if code != alice.CodeDoorOpen {
		t.Fatalf("запуск при открытой дверце: %q", code)
	}
}

// Настройка температуры под другим именем: и пределы, и значение, и команда идут к ней.
func TestFridgeSetpointOtherName(t *testing.T) {
	const other = "Refrigeration.Common.Setting.Refrigerator.SetpointTemperature"
	a := &setRecorder{described: described{
		desc:   appliance.Description{Options: map[string]appliance.Option{other: {Key: other, Min: 2, Max: 8, Step: 1}}},
		values: map[string]any{other: 4.0},
	}}
	d := newDevice(t, "x", "Oven")
	a.Demo = d.Appliance.(*appliance.Demo)
	d.Appliance, d.Profile = a, profile.BuiltinProfile("Refrigerator")
	if info := infoJSON(t, d); !strings.Contains(info, `"instance":"temperature","unit":"unit.temperature.celsius","random_access":true,"range":{"min":2,"max":8,"precision":1}`) {
		t.Fatalf("умение: %s", info)
	}
	raw, _ := json.Marshal(d.States())
	if !strings.Contains(string(raw), `"instance":"temperature","value":4`) {
		t.Fatalf("состояние: %s", raw)
	}
	code := d.Act(context.Background(), "x", &alice.Action{Type: alice.CapRange,
		State: alice.ActionState{Instance: "temperature", Value: 5.0}})
	if code != "" || a.key != other {
		t.Fatalf("команда: %q %q", code, a.key)
	}
}

// setRecorder запоминает настройку последней команды.
type setRecorder struct {
	described
	key string
}

func (a *setRecorder) Set(_ context.Context, key string, _ any) error {
	a.key = key
	return nil
}

// Дверь холодильника под другим именем - то же свойство open.
func TestFridgeDoorOtherName(t *testing.T) {
	d := describedDevice(t, "FridgeFreezer", appliance.Description{Options: map[string]appliance.Option{}},
		map[string]any{"Refrigeration.FridgeFreezer.Status.DoorRefrigerator": "Refrigeration.Common.EnumType.Door.States.Open"})
	raw, _ := json.Marshal(d.States())
	if !strings.Contains(string(raw), `"instance":"open","value":"opened"`) {
		t.Fatalf("состояние: %s", raw)
	}
}
