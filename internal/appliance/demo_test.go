package appliance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newOven(t *testing.T) (*Demo, *[]string) {
	t.Helper()
	d, err := NewDemo("Oven", 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		events []string
	)
	d.notify = func(e []string) {
		mu.Lock()
		events = append(events, e...)
		mu.Unlock()
	}
	return d, &events
}

func TestDemoProgramRuns(t *testing.T) {
	d, events := newOven(t)
	ctx := context.Background()
	hotAir := "Cooking.Oven.Program.HeatingMode.HotAir"
	if err := d.Start(ctx, hotAir, map[string]any{OptionSetpointTemperature: 200.0}); err != nil {
		t.Fatal(err)
	}
	st := d.State()
	if !st.Running() || st.ActiveProgram != hotAir || st.Values[OptionSetpointTemperature] != 200.0 || st.Values[OptionRemainingTime] != 4.0 {
		t.Fatalf("после запуска: %+v", st)
	}
	if err := d.Start(ctx, hotAir, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("повторный запуск: %v", err)
	}
	d.advance(time.Second)
	if got := d.State().Values[StatusCavityTemperature]; got != 200.0 {
		t.Fatalf("разогрев за четверть программы: %v", got)
	}
	if got := d.State().Values[OptionRemainingTime]; got != 3.0 {
		t.Fatalf("оставшееся время: %v", got)
	}
	d.advance(3 * time.Second)
	st = d.State()
	if st.Running() || st.Values[StatusOperationState] != OperationFinished || st.Values[EventProgramFinished] != EventPresent ||
		st.Values[OptionRemainingTime] != 0.0 {
		t.Fatalf("после окончания: %+v", st)
	}
	want := []string{EventPreheatFinished, EventProgramFinished}
	if len(*events) != 2 || (*events)[0] != want[0] || (*events)[1] != want[1] {
		t.Fatalf("события %v", *events)
	}
}

func TestDemoPauseAndSet(t *testing.T) {
	d, _ := newOven(t)
	ctx := context.Background()
	if err := d.Set(ctx, OptionSetpointTemperature, 150.0); !errors.Is(err, ErrWrongMode) {
		t.Fatalf("параметр без программы: %v", err)
	}
	if err := d.Command(ctx, CommandPause); !errors.Is(err, ErrWrongMode) {
		t.Fatalf("пауза без программы: %v", err)
	}
	d.Start(ctx, "Cooking.Oven.Program.HeatingMode.HotAir", nil)
	if err := d.Command(ctx, CommandPause); err != nil {
		t.Fatal(err)
	}
	d.advance(10 * time.Second)
	if d.State().Values[StatusOperationState] != OperationPause {
		t.Fatal("на паузе программа не идёт")
	}
	d.Command(ctx, CommandResume)
	if err := d.Set(ctx, OptionSetpointTemperature, 300.0); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("за пределом: %v", err)
	}
	if err := d.Set(ctx, OptionSetpointTemperature, 160.0); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx); err != nil || d.State().Running() {
		t.Fatalf("остановка: %v", err)
	}
}

func TestDemoStartChecks(t *testing.T) {
	d, _ := newOven(t)
	ctx := context.Background()
	hotAir := "Cooking.Oven.Program.HeatingMode.HotAir"
	if err := d.Start(ctx, "Unknown", nil); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("неизвестная программа: %v", err)
	}
	d.state.Values[StatusDoorState] = DoorOpen
	if err := d.Start(ctx, hotAir, nil); !errors.Is(err, ErrDoorOpen) {
		t.Fatalf("дверца: %v", err)
	}
	d.state.Values[StatusRemoteStartAllowed] = false
	if err := d.Start(ctx, hotAir, nil); !errors.Is(err, ErrRemoteDisabled) {
		t.Fatalf("удалённый старт: %v", err)
	}
	if _, err := NewDemo("Kettle", time.Second); err == nil {
		t.Fatal("ожидалась ошибка: нет такого демо")
	}
}

func TestDemoDishwasherSalt(t *testing.T) {
	d, err := NewDemo("Dishwasher", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d.Start(context.Background(), "Dishcare.Dishwasher.Program.Eco50", nil)
	d.advance(time.Second)
	if d.State().Values[EventSaltNearlyEmpty] != EventPresent {
		t.Fatal("после программы должна кончаться соль")
	}
}

func TestDemoRunStops(t *testing.T) {
	d, _ := newOven(t)
	d.tick = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx, func([]string) {})
		close(done)
	}()
	d.Start(context.Background(), "Cooking.Oven.Program.HeatingMode.HotAir", nil)
	cancel()
	<-done
}
