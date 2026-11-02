// Package appliance - граница между шлюзом и приборами Home Connect.
// Пакет hcdevice, который показывает прибор в Алисе, видит его только через интерфейс
// Appliance: состояние, описание и команды с именами Home Connect вида BSH.Common.Status.DoorState.
// Как прибор подключён - облако, локальная сеть или демо в памяти, - ему не важно.
// Прибор LG выглядит так же, а его имена - пути свойств ThinQ вида runState.currentState.
package appliance

import (
	"context"
	"errors"
)

// Имена Home Connect, которые шлюз проверяет перед командами.
const (
	StatusOperationState     = "BSH.Common.Status.OperationState"
	StatusDoorState          = "BSH.Common.Status.DoorState"
	StatusRemoteStartAllowed = "BSH.Common.Status.RemoteControlStartAllowed"
	SettingPowerState        = "BSH.Common.Setting.PowerState"

	CommandPause  = "BSH.Common.Command.PauseProgram"
	CommandResume = "BSH.Common.Command.ResumeProgram"

	OperationReady    = "BSH.Common.EnumType.OperationState.Ready"
	OperationRun      = "BSH.Common.EnumType.OperationState.Run"
	OperationPause    = "BSH.Common.EnumType.OperationState.Pause"
	OperationFinished = "BSH.Common.EnumType.OperationState.Finished"
	OperationDelayed  = "BSH.Common.EnumType.OperationState.DelayedStart"
	DoorOpen          = "BSH.Common.EnumType.DoorState.Open"
	DoorClosed        = "BSH.Common.EnumType.DoorState.Closed"
	PowerOn           = "BSH.Common.EnumType.PowerState.On"
	PowerOff          = "BSH.Common.EnumType.PowerState.Off"
	PowerStandby      = "BSH.Common.EnumType.PowerState.Standby"
	EventPresent      = "BSH.Common.EnumType.EventPresentState.Present"
	EventOff          = "BSH.Common.EnumType.EventPresentState.Off"
	EventConfirmed    = "BSH.Common.EnumType.EventPresentState.Confirmed"
)

// Ошибки команд. Пакет hcdevice переводит их в коды ошибок Яндекса.
var (
	ErrUnreachable    = errors.New("прибор недоступен")
	ErrRemoteDisabled = errors.New("удалённый старт на приборе не разрешён")
	ErrDoorOpen       = errors.New("дверца открыта")
	ErrBusy           = errors.New("прибор занят")
	ErrInvalidValue   = errors.New("недопустимое значение")
	ErrNotSupported   = errors.New("прибор не поддерживает команду")
	ErrWrongMode      = errors.New("команда недоступна в текущем состоянии прибора")
)

// Option - параметр программы или настройка прибора с допустимыми значениями.
type Option struct {
	Key  string
	Min  float64
	Max  float64
	Step float64
	// Values - допустимые значения перечисления; пусто для чисел и флагов.
	Values []string
	// ReadOnly - прибор сообщает значение, но менять его не разрешает.
	ReadOnly bool
	// Setting - настройка: меняется сразу, без программы. У Home Connect настройку видно
	// и по имени с .Setting.
	Setting bool
}

// Description - что умеет прибор.
type Description struct {
	// Type - тип прибора в Home Connect, например Oven или Dishwasher.
	Type     string
	Programs []string
	// Options - параметры программ и настройки по имени.
	Options  map[string]Option
	Commands []string
	// Events - события, о которых прибор может сообщить; пусто - не известно заранее.
	Events []string
	// Reports - все значения, которые прибор может сообщить; пусто - не известно заранее.
	// По нему устройство Алисы не обещает свойств и переключателей, которых у прибора нет.
	Reports []string
}

// Knows сообщает, может ли прибор сообщить значение key. Если список значений прибора не
// известен, - может.
func (d *Description) Knows(key string) bool {
	return len(d.Reports) == 0 || contains(d.Reports, key)
}

// HasProgram сообщает, есть ли у прибора программа.
func (d *Description) HasProgram(key string) bool {
	return contains(d.Programs, key)
}

// HasCommand сообщает, есть ли у прибора команда.
func (d *Description) HasCommand(key string) bool {
	return contains(d.Commands, key)
}

func contains(list []string, key string) bool {
	for _, k := range list {
		if k == key {
			return true
		}
	}
	return false
}

// State - снимок состояния прибора.
type State struct {
	Online bool
	// Values - состояния, настройки, параметры и события по имени.
	// Значение - bool, float64 или string (имя значения перечисления).
	Values map[string]any
	// ActiveProgram - работающая программа, пусто, если программа не идёт.
	ActiveProgram string
	// SelectedProgram - выбранная, но не запущенная программа.
	SelectedProgram string
}

// Running сообщает, идёт ли программа: работает, на паузе или ждёт отложенного старта.
func (s State) Running() bool {
	switch s.Values[StatusOperationState] {
	case OperationRun, OperationPause, OperationDelayed:
		return true
	}
	return false
}

// Notify вызывается прибором после каждого изменения состояния.
// events - имена событий, которые произошли в этом изменении, например BSH.Common.Event.ProgramFinished.
type Notify func(events []string)

// Appliance - прибор. Методы можно вызывать из разных горутин.
type Appliance interface {
	Describe() Description
	State() State
	// Run держит связь с прибором и вызывает notify после изменений, пока ctx не отменён.
	Run(ctx context.Context, notify Notify)
	// Select выбирает программу, не запуская её.
	Select(ctx context.Context, program string) error
	// Start запускает программу с параметрами.
	Start(ctx context.Context, program string, options map[string]any) error
	// Stop останавливает программу.
	Stop(ctx context.Context) error
	// Command выполняет команду, например паузу.
	Command(ctx context.Context, key string) error
	// Set меняет параметр работающей программы или настройку прибора.
	Set(ctx context.Context, key string, value any) error
}

// Stepper - прибор, который меняет число на шаг сам, не сообщая текущего значения: так
// телевизор кнопками пульта переключает канал. «Следующий канал» без текущего значения
// уходит прибору шагом.
type Stepper interface {
	Step(ctx context.Context, key string, delta float64) error
}
