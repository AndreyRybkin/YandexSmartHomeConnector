package hcdevice

import (
	"context"
	"math"
	"slices"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/profile"
)

// Пределы цветовой температуры, если их не задал ни профиль, ни прибор: так их пишет
// пример лампы в документации Яндекса.
const (
	defaultMinK = 2700
	defaultMaxK = 6500
)

// yandexMinK и yandexMaxK - палитра цветовой температуры Яндекса по умолчанию.
const (
	yandexMinK = 2000
	yandexMaxK = 9000
)

// hasToggle сообщает, есть ли у прибора переключатель. Пауза без настройки - это команды
// паузы и продолжения программы, переключатель с настройкой есть, если прибор её сообщает. Необязательного
// переключателя нет и у прибора, который перечисляет значения настройки без значения «включено»: команда
// ушла бы впустую.
func hasToggle(t *profile.Toggle, desc *appliance.Description) bool {
	if t.Instance == "pause" && t.Setting == "" {
		return desc.HasCommand(appliance.CommandPause) && desc.HasCommand(appliance.CommandResume)
	}
	if t.Optional {
		o, ok := desc.Options[t.Setting]
		return ok && !o.ReadOnly && (t.On == "" || len(o.Values) == 0 || slices.Contains(o.Values, t.On))
	}
	return desc.Knows(t.Setting)
}

// colorParams - параметры умения color_setting: модель HSV, если прибор сообщает оттенок и
// насыщенность, и пределы температуры, если прибор её сообщает. false - умения нет.
func colorParams(c *profile.Color, desc *appliance.Description) (alice.ColorParams, bool) {
	var out alice.ColorParams
	if c == nil {
		return out, false
	}
	if c.Setting != "" && desc.Knows(c.Hue) && desc.Knows(c.Saturation) {
		out.ColorModel = "hsv"
	}
	if c.Temperature != "" && desc.Knows(c.Temperature) {
		lim := alice.KelvinLimits{Min: defaultMinK, Max: defaultMaxK}
		if opt, ok := desc.Options[c.Temperature]; ok && opt.Min > 0 && opt.Max > opt.Min {
			// Пределы шире палитры Яндекса - общая схема capability, а не лампы: шлюз сужает их.
			lim = alice.KelvinLimits{Min: max(int(math.Ceil(opt.Min)), yandexMinK), Max: min(int(math.Floor(opt.Max)), yandexMaxK)}
		}
		if c.MinK != nil {
			lim.Min = *c.MinK
		}
		if c.MaxK != nil {
			lim.Max = *c.MaxK
		}
		if lim.Min < lim.Max {
			out.TemperatureK = &lim
		}
	}
	return out, out.ColorModel != "" || out.TemperatureK != nil
}

// colorState - текущий цвет для Алисы. Белый свет с известной температурой - temperature_k:
// лампа в этом режиме сообщает нулевую насыщенность. Яркость HSV - из умения brightness,
// без него - 100.
func (d *Device) colorState(st *appliance.State, desc *appliance.Description) (alice.StateValue, bool) {
	c := d.Profile.Color
	params, ok := colorParams(c, desc)
	if !ok {
		return alice.StateValue{}, false
	}
	hue, hasHue := st.Values[c.Hue].(float64)
	sat, hasSat := st.Values[c.Saturation].(float64)
	hsv := params.ColorModel != "" && hasHue && hasSat
	if k, ok := st.Values[c.Temperature].(float64); ok && params.TemperatureK != nil && (!hsv || sat == 0) {
		return alice.StateValue{Instance: "temperature_k", Value: int(math.Round(k))}, true
	}
	if !hsv {
		return alice.StateValue{}, false
	}
	v := 100
	if r := findRange(d.Profile.Ranges, "brightness"); r != nil {
		if b, ok := st.Values[r.Option].(float64); ok {
			v = int(math.Round(b))
		}
	}
	h := int(math.Round(hue/c.HueScale()*360)) % 360
	return alice.StateValue{Instance: "hsv", Value: alice.HSV{H: h, S: int(math.Round(sat)), V: v}}, true
}

// setColor выполняет команду color_setting: цвет HSV уходит прибору оттенком и
// насыщенностью в его шкале, температура - числом в кельвинах в пределах умения.
func (d *Device) setColor(ctx context.Context, desc *appliance.Description, s *alice.ActionState) string {
	c := d.Profile.Color
	params, ok := colorParams(c, desc)
	if !ok {
		return alice.CodeInvalidAction
	}
	switch s.Instance {
	case "hsv":
		if params.ColorModel == "" {
			return alice.CodeInvalidAction
		}
		m, ok := s.Value.(map[string]any)
		h, okH := m["h"].(float64)
		sat, okS := m["s"].(float64)
		if !ok || !okH || !okS || h < 0 || h > 360 || sat < 0 || sat > 100 {
			return alice.CodeInvalidValue
		}
		hue := math.Round(h/360*c.HueScale()*100) / 100
		return code(d.Appliance.Set(ctx, c.Setting, map[string]any{"hue": hue, "saturation": sat}))
	case "temperature_k":
		lim := params.TemperatureK
		if lim == nil {
			return alice.CodeInvalidAction
		}
		k, ok := s.Value.(float64)
		if !ok || k < float64(lim.Min) || k > float64(lim.Max) {
			return alice.CodeInvalidValue
		}
		return code(d.Appliance.Set(ctx, c.Temperature, k))
	}
	return alice.CodeInvalidAction
}
