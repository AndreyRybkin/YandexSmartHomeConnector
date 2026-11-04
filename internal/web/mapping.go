package web

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"hcgateway/internal/alice"
	"hcgateway/internal/appliance"
	"hcgateway/internal/config"
	"hcgateway/internal/hcdevice"
	"hcgateway/internal/profile"
)

// Помощь в сопоставлениях на странице прибора: несопоставленные значения, предпросмотр
// устройства Алисы и кнопка «Проверить» у режима.

// unmappedRow - значение прибора, которого Алиса не видит, и раздел редактора для него.
type unmappedRow struct {
	Key, Value, Section, Label string
}

// unmapped - значения прибора, которые не берёт ни одно поле профиля и не читает сам шлюз.
// Раздел выбирается по виду значения: перечисление - режим, число - range, флаг -
// переключатель, событие - раздел событий. Питание Home Connect - в раздел «включи».
func unmapped(st *appliance.State, p *profile.Profile, desc *appliance.Description) []unmappedRow {
	used := mappings(p)
	var out []unmappedRow
	for _, k := range slices.Sorted(maps.Keys(st.Values)) {
		if used[k] != "" {
			continue
		}
		v := st.Values[k]
		row := unmappedRow{Key: k, Value: formatValue(v)}
		o, isOption := desc.Options[k]
		switch x := v.(type) {
		case string:
			if k == appliance.SettingPowerState {
				row.Section, row.Label = "on_off", "питание"
				break
			}
			switch {
			case x == appliance.EventPresent || x == appliance.EventOff || x == appliance.EventConfirmed:
				row.Section, row.Label = "events", "событие"
			case isOption && len(o.Values) > 0:
				row.Section, row.Label = "modes", "режим"
			default:
				row.Section, row.Label = "properties", "свойство event"
			}
		case bool:
			row.Section, row.Label = "toggles", "переключатель"
		case float64:
			row.Section, row.Label = "ranges", "число range"
			if !isOption {
				row.Section, row.Label = "properties", "свойство float"
			}
		default:
			continue
		}
		out = append(out, row)
	}
	return out
}

// previewProfile показывает устройство Алисы по полю из формы, не сохраняя его.
func (s *Server) previewProfile(w http.ResponseWriter, sess *session, id, field string, patch *profile.Profile, hc *hcdevice.Device) {
	cfg, _ := s.file.Current()
	dc := cfg.Devices[id]
	haType := hc.Appliance.Describe().Type
	t := &editTarget{builtin: profile.BuiltinProfile(haType), forType: cfg.Profiles[haType], forDevice: dc.Profile, device: true}
	next := applyField(t, field, patch, false)
	forDevice := next
	if dc.Source == config.SourceDeLonghi {
		forDevice = profile.Merge(profile.DeLonghiLayer(dc.Recipes, dc.DefaultDrink), next)
	}
	p, err := profile.Resolve(haType, cfg.Profiles[haType], forDevice)
	if err != nil {
		s.renderDevice(w, sess, http.StatusBadRequest, id, &draft{section: field, err: err.Error(), profile: next})
		return
	}
	dev := &hcdevice.Device{ID: id, Name: dc.Name, Room: dc.Room, Profile: p, Appliance: hc.Appliance, Manufacturer: hc.Manufacturer}
	info := dev.Info()
	out, _ := json.MarshalIndent(info, "", "  ")
	s.renderDevice(w, sess, http.StatusOK, id, &draft{section: field, profile: next, preview: string(out), previewInfo: info})
}

// previewView - устройство Алисы до сохранения: умения словами и что с ними станет.
type previewView struct {
	Devices []previewDevice
	JSON    string
}

// previewDevice - устройство Алисы в предпросмотре; TypeChange - тип после сохранения другой.
type previewDevice struct {
	Name, Type, TypeChange string
	Items                  []previewItem
}

// previewItem - умение или свойство и что с ним станет после сохранения: Change -
// «добавится», «изменится» или «пропадёт», пусто - не изменится; Tone - вид метки.
type previewItem struct {
	aliceItem
	Change, Tone string
}

// previewOf сравнивает устройства Алисы после сохранения next с нынешними cur. raw - JSON
// для Яндекса; пусто - предпросмотра нет.
func previewOf(next, cur []alice.DeviceInfo, raw string) *previewView {
	if raw == "" {
		return nil
	}
	items := func(info alice.DeviceInfo) []aliceItem {
		var out []aliceItem
		for _, c := range append(slices.Clone(info.Capabilities), info.Properties...) {
			out = append(out, describe(c))
		}
		return out
	}
	old := map[string]alice.DeviceInfo{}
	for _, info := range cur {
		old[info.ID] = info
	}
	v := &previewView{JSON: raw}
	for _, info := range next {
		d := previewDevice{Name: info.Name, Type: yandexTypeName(info.Type)}
		was, existed := old[info.ID]
		if existed && was.Type != info.Type {
			d.TypeChange = "изменится"
		}
		prev := map[string]aliceItem{}
		for _, it := range items(was) {
			prev[it.Tech] = it
		}
		for _, it := range items(info) {
			p := previewItem{aliceItem: it}
			switch o, ok := prev[it.Tech]; {
			case !ok:
				p.Change, p.Tone = "добавится", "run"
			case o != it:
				p.Change, p.Tone = "изменится", "warn"
			}
			delete(prev, it.Tech)
			d.Items = append(d.Items, p)
		}
		for _, tech := range slices.Sorted(maps.Keys(prev)) {
			d.Items = append(d.Items, previewItem{aliceItem: prev[tech], Change: "пропадёт", Tone: "err"})
		}
		delete(old, info.ID)
		v.Devices = append(v.Devices, d)
	}
	// Устройства, которых после сохранения не будет.
	for _, info := range cur {
		if _, ok := old[info.ID]; !ok {
			continue
		}
		d := previewDevice{Name: info.Name, Type: yandexTypeName(info.Type)}
		for _, it := range items(info) {
			d.Items = append(d.Items, previewItem{aliceItem: it, Change: "пропадёт", Tone: "err"})
		}
		v.Devices = append(v.Devices, d)
	}
	return v
}

// probe отправляет прибору команду режима, как Алиса: probe - «instance|режим».
func (s *Server) probe(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	to := "/devices/" + id + "#modes"
	instance, value, ok := strings.Cut(r.PostForm.Get("probe"), "|")
	if !ok || !slices.Contains(profile.ModeInstances, instance) || !slices.Contains(profile.ModeValues, value) {
		s.failed(w, r, sess, to, "Не выполнено: неверный режим.")
		return
	}
	a := alice.Action{Type: alice.CapMode, State: alice.ActionState{Instance: instance, Value: value}}
	if code := s.gw.Service.Command(r.Context(), id, &a); code != "" {
		s.failed(w, r, sess, to, "Не выполнено: "+errorTexts[code]+" ("+code+").")
		return
	}
	s.done(w, r, sess, to, "Команда «"+nameOr(modeInstanceNames, instance)+": "+nameOr(modeValueNames, value)+"» выполнена.")
}
