// Package alice - устройство Алисы: граница между провайдером умного дома Яндекса
// и источниками приборов. Провайдер видит любой прибор только через интерфейс Device,
// а источник сам решает, как выглядит устройство, какое у него состояние и как
// выполнить команду. Типы повторяют JSON протокола умного дома Яндекса.
package alice

import "context"

// Типы умений, свойств и устройств Яндекса.
const (
	CapOnOff  = "devices.capabilities.on_off"
	CapMode   = "devices.capabilities.mode"
	CapRange  = "devices.capabilities.range"
	CapToggle = "devices.capabilities.toggle"
	CapColor  = "devices.capabilities.color_setting"
	PropEvent = "devices.properties.event"
	PropFloat = "devices.properties.float"

	TypePrefix = "devices.types."
	TypeButton = "devices.types.sensor.button"
	TypeLight  = "devices.types.light"
)

// Коды ошибок Яндекса.
const (
	CodeDoorOpen       = "DOOR_OPEN"
	CodeRemoteDisabled = "REMOTE_CONTROL_DISABLED"
	CodeUnreachable    = "DEVICE_UNREACHABLE"
	CodeBusy           = "DEVICE_BUSY"
	CodeInternal       = "INTERNAL_ERROR"
	CodeInvalidAction  = "INVALID_ACTION"
	CodeInvalidValue   = "INVALID_VALUE"
	CodeWrongMode      = "NOT_SUPPORTED_IN_CURRENT_MODE"
	CodeNotFound       = "DEVICE_NOT_FOUND"
)

// Device - прибор или лента в Алисе: одно или несколько устройств Алисы одного
// источника, например прибор и его датчик событий. Методы можно вызывать из разных горутин.
type Device interface {
	// IDs - идентификаторы устройств Алисы. Они не меняются, пока прибор есть в настройках.
	IDs() []string
	// Info - описание устройств для списка устройств, в порядке IDs.
	Info() []DeviceInfo
	// States - состояния устройств в порядке IDs. У недоступного устройства вместо состояния - ErrorCode.
	States() []DeviceState
	// Act выполняет команду устройству id и возвращает код ошибки Яндекса или пустую строку.
	Act(ctx context.Context, id string, a *Action) string
	// Run держит связь с источником и вызывает changed после изменений, пока ctx не отменён.
	Run(ctx context.Context, changed Changed)
}

// Changed сообщает об изменении устройства. events - состояния, которые нужно отправить
// Яндексу сразу и по одному, например событие датчика: два нажатия подряд не должны
// слиться в одно. Остальное провайдер соберёт сам и отправит после паузы.
type Changed func(events []DeviceState)

// ColorScenes - сцены умения color_setting из списка Яндекса.
var ColorScenes = []string{"alarm", "alice", "candle", "dinner", "fantasy", "garland", "jungle", "movie", "neon",
	"night", "ocean", "party", "reading", "rest", "romance", "siren"}
