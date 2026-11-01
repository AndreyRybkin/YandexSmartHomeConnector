package alice

// Типы JSON протокола провайдера умного дома Яндекса.

// DeviceInfo - устройство в списке устройств.
type DeviceInfo struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Room         string       `json:"room,omitempty"`
	Type         string       `json:"type"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	Properties   []Capability `json:"properties,omitempty"`
	DeviceInfo   Manufacturer `json:"device_info"`
}

// Manufacturer - производитель и модель устройства.
type Manufacturer struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model,omitempty"`
}

// Capability - умение или свойство в списке устройств.
type Capability struct {
	Type        string `json:"type"`
	Retrievable bool   `json:"retrievable"`
	Reportable  bool   `json:"reportable"`
	Parameters  any    `json:"parameters,omitempty"`
}

type ModeParams struct {
	Instance string      `json:"instance"`
	Modes    []ValueItem `json:"modes"`
}

type ValueItem struct {
	Value string `json:"value"`
}

type RangeParams struct {
	Instance     string     `json:"instance"`
	Unit         string     `json:"unit,omitempty"`
	RandomAccess bool       `json:"random_access"`
	Range        RangeLimit `json:"range"`
}

type RangeLimit struct {
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Precision float64 `json:"precision"`
}

type InstanceParams struct {
	Instance string `json:"instance"`
}

type EventParams struct {
	Instance string      `json:"instance"`
	Events   []ValueItem `json:"events"`
}

type FloatParams struct {
	Instance string `json:"instance"`
	Unit     string `json:"unit,omitempty"`
}

// CapState - умение или свойство с состоянием в ответе на query и в уведомлениях.
// State - StateValue для состояния или результат команды.
type CapState struct {
	Type  string `json:"type"`
	State any    `json:"state"`
}

// StateValue - значение умения или свойства. Value без omitempty: false и 0 - тоже значения.
type StateValue struct {
	Instance string `json:"instance"`
	Value    any    `json:"value"`
}

// DeviceState - состояние устройства, а у недоступного - ErrorCode.
type DeviceState struct {
	ID           string     `json:"id"`
	Capabilities []CapState `json:"capabilities,omitempty"`
	Properties   []CapState `json:"properties,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
}

// Action - команда Алисы одному умению.
type Action struct {
	Type  string      `json:"type"`
	State ActionState `json:"state"`
}

// ActionState - что сделать: instance, значение и, для range, прибавить ли его к текущему.
type ActionState struct {
	Instance string `json:"instance"`
	Value    any    `json:"value"`
	Relative bool   `json:"relative"`
}

// Items превращает значения в список для mode и event.
func Items(values []string) []ValueItem {
	out := make([]ValueItem, len(values))
	for i, v := range values {
		out[i] = ValueItem{Value: v}
	}
	return out
}

// ColorParams - параметры умения color_setting: цветовая модель, пределы цветовой
// температуры и сцены.
type ColorParams struct {
	ColorModel   string        `json:"color_model,omitempty"`
	TemperatureK *KelvinLimits `json:"temperature_k,omitempty"`
	ColorScene   *SceneParams  `json:"color_scene,omitempty"`
}

// KelvinLimits - пределы цветовой температуры в кельвинах.
type KelvinLimits struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// HSV - цвет в модели HSV: оттенок 0-360, насыщенность и яркость 0-100.
type HSV struct {
	H int `json:"h"`
	S int `json:"s"`
	V int `json:"v"`
}

// SceneParams - сцены умения color_setting.
type SceneParams struct {
	Scenes []SceneItem `json:"scenes"`
}

// SceneItem - одна сцена.
type SceneItem struct {
	ID string `json:"id"`
}
