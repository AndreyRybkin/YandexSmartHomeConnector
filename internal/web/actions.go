package web

import (
	"encoding/json"
	"maps"
	"math"
	"net/http"
	"time"

	"hcgateway/internal/alice"
	"hcgateway/internal/statedb"
	"hcgateway/internal/yandex"
)

// initiatorNames - кто дал команду.
var initiatorNames = map[string]string{yandex.InitiatorAlice: "Алиса", yandex.InitiatorGateway: "Шлюз"}

// actionRow - строка журнала действий.
type actionRow struct {
	Time, ID, Device, Label, Value, Failure, Initiator string
}

type actionsView struct {
	// Name - прибор, действия которого показаны; пусто - все приборы.
	Name string
	Rows []actionRow
}

// actionsPage - журнал действий всех приборов, а с ?device=<id> - одного.
func (s *Server) actionsPage(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, _ := s.file.Current()
	var v actionsView
	id := r.URL.Query().Get("device")
	if id != "" {
		dc, ok := cfg.Devices[id]
		if !ok {
			s.notFound(w, sess)
			return
		}
		v.Name = dc.Name
	}
	unitsOf := map[string]map[string]string{}
	for _, a := range s.gw.Actions(id) {
		// Запись удалённого прибора база стирает не сразу.
		dc, ok := cfg.Devices[a.Device]
		if !ok {
			continue
		}
		if _, seen := unitsOf[a.Device]; !seen {
			unitsOf[a.Device] = s.deviceUnits(a.Device)
		}
		row := actionRow{Time: a.Time.Local().Format(time.DateTime), ID: a.Device, Device: dc.Name,
			Label: actionLabel(&a), Value: actionValue(&a, unitsOf[a.Device]), Initiator: nameOr(initiatorNames, a.Initiator)}
		if a.Code != "" {
			row.Failure = "Не выполнено: " + nameOr(errorTexts, a.Code) + " (" + a.Code + ")."
		}
		v.Rows = append(v.Rows, row)
	}
	s.render(w, http.StatusOK, "actions", s.newPage(sess, "actions", "Действия", v))
}

// deviceUnits - единицы чисел прибора по виду и instance.
func (s *Server) deviceUnits(id string) map[string]string {
	out := map[string]string{}
	if dev, ok := s.gw.Device(id); ok {
		for _, info := range dev.Info() {
			maps.Copy(out, units(info))
		}
	}
	return out
}

// actionLabel - что меняет команда: «Питание», «Громкость».
func actionLabel(a *statedb.Action) string {
	switch a.Type {
	case alice.CapOnOff:
		return "Питание"
	case alice.CapColor:
		return itemLabel(a.Type, a.Instance)
	}
	if names := instanceNames(a.Type); names != nil {
		return nameOr(names, a.Instance)
	}
	return a.Type + " " + a.Instance
}

// actionValue - значение команды по-русски. Значение, которое не удалось прочитать, например
// обрезанное при сохранении, показывается как есть.
func actionValue(a *statedb.Action, unitsOf map[string]string) string {
	var v any
	if err := json.Unmarshal([]byte(a.Value), &v); err != nil {
		return a.Value
	}
	sv := alice.StateValue{Instance: a.Instance, Value: v}
	if a.Type == alice.CapColor {
		switch x := v.(type) {
		case float64:
			if x >= 0 && x <= math.MaxInt32 && x == math.Trunc(x) {
				sv.Value = int(x)
			}
		case map[string]any:
			var hsv alice.HSV
			if json.Unmarshal([]byte(a.Value), &hsv) == nil {
				sv.Value = hsv
			}
		}
	}
	text := itemValue(a.Type, sv, unitsOf[a.Type+"/"+a.Instance])
	if f, ok := v.(float64); ok && a.Relative && f > 0 {
		return "+" + text
	}
	return text
}
