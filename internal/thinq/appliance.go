package thinq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"hcgateway/internal/appliance"
)

var _ appliance.Appliance = (*Appliance)(nil)

// Appliance - прибор LG или его камера как прибор по профилю: функции - свойства
// «ресурс.свойство», значения - как в ThinQ Connect, заглавными. Программ, команд и паузы
// у прибора LG нет: всё, что он умеет, - записать свойство.
type Appliance struct {
	cloud    *Cloud
	deviceID string
	location string

	mu    sync.Mutex
	model *model
	// values и have - состояние и то, что оно получено.
	values map[string]any
	have   bool
	notify appliance.Notify
	runs   int
}

// DeviceID - deviceId прибора.
func (a *Appliance) DeviceID() string {
	return a.deviceID
}

// setModel задаёт профиль прибора. Без профиля прибора нет в Алисе.
func (a *Appliance) setModel(m *model) {
	a.mu.Lock()
	a.model = m
	a.changedLocked(nil)
}

func (a *Appliance) Describe() appliance.Description {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.model
	if m == nil {
		return appliance.Description{Options: map[string]appliance.Option{}}
	}
	// Профиль перечисляет все свойства и уведомления прибора. В Reports входят и свойства только
	// для записи: питание стиральной машины такое, а по Reports устройство Алисы решает, есть ли
	// у него on_off. Значение, которого нет в профиле, но которое прибор прислал, - тоже: его мог
	// сопоставить владелец.
	d := appliance.Description{Type: m.DeviceType, Options: map[string]appliance.Option{}, Events: slices.Clone(m.Push),
		EventsKnown: true, Reports: []string{RemainKey}}
	for key, p := range m.props {
		d.Options[key] = appliance.Option{Key: key, Min: p.Min, Max: p.Max, Step: p.Step,
			Values: slices.Clone(p.Values), ReadOnly: !p.Write, Setting: p.Write}
		d.Reports = append(d.Reports, key)
	}
	for key := range a.values {
		if _, ok := d.Options[key]; !ok && key != RemainKey {
			d.Reports = append(d.Reports, key)
		}
	}
	slices.Sort(d.Reports)
	return d
}

func (a *Appliance) State() appliance.State {
	connected := a.cloud.Connected()
	a.mu.Lock()
	defer a.mu.Unlock()
	return appliance.State{Online: connected && a.have, Values: maps.Clone(a.values)}
}

// Run подписывает прибор на состояние из облака: MQTT держит Cloud, один на аккаунт.
func (a *Appliance) Run(ctx context.Context, notify appliance.Notify) {
	a.mu.Lock()
	a.runs++
	gen := a.runs
	a.notify = notify
	a.mu.Unlock()
	a.cloud.attach(a)
	<-ctx.Done()
	a.cloud.detach(a)
	a.mu.Lock()
	if a.runs == gen {
		a.notify = nil
	}
	a.mu.Unlock()
}

// changedLocked отпускает блокировку и сообщает об изменении.
func (a *Appliance) changedLocked(events []string) {
	notify := a.notify
	a.mu.Unlock()
	if notify != nil {
		notify(events)
	}
}

// apply применяет состояние или его часть. full - всё состояние, а не изменения.
func (a *Appliance) apply(raw json.RawMessage, full bool) {
	a.mu.Lock()
	if a.model == nil {
		a.mu.Unlock()
		return
	}
	if full || a.values == nil {
		a.values = map[string]any{}
	}
	a.model.flatten(raw, a.values)
	remain(a.values)
	a.model.derive(a.values)
	a.have = true
	a.changedLocked(nil)
}

// RemainKey - оставшееся время работы в секундах. ThinQ присылает часы и минуты отдельно, а
// свойству Алисы remaining_time нужно одно число: его шлюз собирает сам.
const RemainKey = "timer.remainSeconds"

// remain складывает timer.remainHour и timer.remainMinute в RemainKey. Если прибор не
// прислал ни того, ни другого, значения нет.
func remain(values map[string]any) {
	h, hasH := values["timer.remainHour"].(float64)
	m, hasM := values["timer.remainMinute"].(float64)
	if !hasH && !hasM {
		delete(values, RemainKey)
		return
	}
	values[RemainKey] = h*3600 + m*60
}

// push - уведомление прибора: код заглавными уходит событием.
func (a *Appliance) push(code string) {
	a.mu.Lock()
	a.changedLocked([]string{code})
}

// offline - связи с облаком нет: состояние неизвестно, пока его не загрузят заново.
func (a *Appliance) offline() {
	a.mu.Lock()
	had := a.have
	a.have = false
	if !had {
		a.mu.Unlock()
		return
	}
	a.changedLocked(nil)
}

func (a *Appliance) Select(context.Context, string) error {
	return fmt.Errorf("%w: ThinQ Connect не выбирает программы", appliance.ErrNotSupported)
}

func (a *Appliance) Start(context.Context, string, map[string]any) error {
	return fmt.Errorf("%w: ThinQ Connect не запускает программы", appliance.ErrNotSupported)
}

func (a *Appliance) Stop(context.Context) error {
	return fmt.Errorf("%w: остановка прибора LG - через свойство из power", appliance.ErrNotSupported)
}

func (a *Appliance) Command(context.Context, string) error {
	return fmt.Errorf("%w: команд у прибора LG нет", appliance.ErrNotSupported)
}

// Set записывает свойство. Значение шлюз проверяет по профилю до запроса к облаку.
func (a *Appliance) Set(ctx context.Context, key string, value any) error {
	a.mu.Lock()
	m := a.model
	state := maps.Clone(a.values)
	a.mu.Unlock()
	if m == nil {
		return fmt.Errorf("%w: профиль прибора LG не загружен", appliance.ErrUnreachable)
	}
	body, err := m.command(key, value, state)
	switch {
	case errors.Is(err, errReadOnly):
		return fmt.Errorf("%w: %s: %w", appliance.ErrNotSupported, key, err)
	case errors.Is(err, errWrongState):
		return fmt.Errorf("%w: %s: %w", appliance.ErrWrongMode, key, err)
	case err != nil:
		return fmt.Errorf("%w: %s = %v: %w", appliance.ErrInvalidValue, key, value, err)
	}
	return mapErr(a.cloud.API.Control(ctx, a.deviceID, body))
}

// mapErr переводит ошибку облака в ошибку прибора: из неё hcdevice получает код Яндекса.
func mapErr(err error) error {
	var ae *APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotConfigured):
		return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
	case errors.As(err, &ae):
		switch ae.Code {
		case codeRemoteOff:
			return fmt.Errorf("%w: %w", appliance.ErrRemoteDisabled, err)
		case codeNotInState, codeNotInMode, codePowerOff, codeInError:
			return fmt.Errorf("%w: %w", appliance.ErrWrongMode, err)
		case codeNotConnectedDevice, codeResponseDelay:
			return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
		case codeExceededCalls:
			return fmt.Errorf("%w: %w", appliance.ErrBusy, err)
		}
		if ae.Status >= 500 {
			return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
		}
		return err
	case errors.Is(err, context.Canceled):
		return err
	}
	// Сбой сети или таймаут.
	return fmt.Errorf("%w: %w", appliance.ErrUnreachable, err)
}
