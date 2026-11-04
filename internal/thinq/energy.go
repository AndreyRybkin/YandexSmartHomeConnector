package thinq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"hcgateway/internal/atomicfile"
)

const (
	// EnergyKey - потребление прибора за текущий месяц в кВт·ч. Значение собирает сам шлюз:
	// в отчётах прибора его нет.
	EnergyKey = "energy.monthUsageKwh"
	// energyUsageProp - свойство потребления в профиле энергии, у части колонны - с окончанием _<часть>.
	energyUsageProp = "energyUsage"
	// energyPoll - как часто запрашивать энергию прибора: запросы тратят лимит API.
	energyPoll = time.Hour
	// energyAbsentRetry - через сколько снова спросить профиль энергии у прибора, у которого его нет.
	energyAbsentRetry = 24 * time.Hour
	// energyTick - как часто искать приборы, которым пора запросить энергию.
	energyTick = time.Minute
)

// watchEnergy запрашивает потребление приборов, пока ctx не отменён.
func (c *Cloud) watchEnergy(ctx context.Context) {
	t := time.NewTicker(energyTick)
	defer t.Stop()
	for {
		c.pollEnergy(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollEnergy запрашивает энергию работающих приборов, которым пора.
func (c *Cloud) pollEnergy(ctx context.Context, now time.Time) {
	if !c.API.Ready() {
		return
	}
	for _, id := range c.runningIDs() {
		if ctx.Err() != nil {
			return
		}
		c.pollDevice(ctx, id, now)
	}
}

// pollDevice обновляет показание одного прибора. Ошибка только откладывает следующий запрос: состояние
// прибора и прошлое показание она не меняет.
func (c *Cloud) pollDevice(ctx context.Context, id string, now time.Time) {
	c.mu.Lock()
	next := c.energyNext[id]
	st := c.profileLocked(id)
	c.mu.Unlock()
	if st == nil || now.Before(next) {
		return
	}
	props := st.Energy
	if len(props) == 0 {
		var err error
		if props, err = c.API.EnergyProfile(ctx, id); err != nil && !energyAbsent(err) {
			c.log.Warn("ThinQ: профиль энергии не загружен", "device_id", id, "err", err)
			c.setEnergyNext(id, now.Add(energyPoll))
			return
		}
		if props = energyProps(props); err != nil || len(props) == 0 {
			c.setEnergyNext(id, now.Add(energyAbsentRetry))
			return
		}
		c.saveEnergy(id, props)
	}
	c.setEnergyNext(id, now.Add(energyPoll))
	month := now.Format("200601")
	usage := map[string]*float64{}
	for _, a := range c.running(id) {
		prop := a.useEnergy(props)
		if prop == "" {
			continue
		}
		if _, asked := usage[prop]; !asked {
			usage[prop] = nil
			wh, err := c.API.EnergyUsage(ctx, id, prop, month)
			if err != nil {
				c.log.Warn("ThinQ: потребление не загружено", "device_id", id, "err", err)
				continue
			}
			kwh := wh / 1000
			usage[prop] = &kwh
		}
		if v := usage[prop]; v != nil {
			a.setEnergy(*v)
		}
	}
}

// energyAbsent сообщает, что облако не знает профиля энергии этого прибора.
func energyAbsent(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusNotFound)
}

// energyProps - свойства потребления из ответа: шлюз хранит только те, что умеет читать.
func energyProps(props []string) []string {
	var out []string
	for _, p := range props {
		if p == energyUsageProp || strings.HasPrefix(p, energyUsageProp+"_") && len(p) <= len(energyUsageProp)+16 {
			out = append(out, p)
		}
	}
	return out
}

func (c *Cloud) setEnergyNext(id string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.energyNext[id] = at
}

// saveEnergy запоминает свойства потребления рядом с профилем прибора.
func (c *Cloud) saveEnergy(id string, props []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.profiles[id]
	if st == nil {
		return
	}
	own := *st
	own.Energy = props
	c.profiles[id] = &own
	data, err := json.MarshalIndent(&own, "", "  ")
	if err == nil {
		err = atomicfile.Write(c.profilePath(id), data)
	}
	if err != nil {
		c.log.Warn("ThinQ: профиль энергии не сохранён", "device_id", id, "err", err)
	}
}

// useEnergy выбирает свойство потребления прибора из props и возвращает его; пусто - показания нет.
func (a *Appliance) useEnergy(props []string) string {
	a.mu.Lock()
	var prop string
	if a.model != nil {
		prop = a.model.energyProperty(props)
	}
	if prop == a.energyProp {
		a.mu.Unlock()
		return prop
	}
	a.energyProp = prop
	// Новое свойство Яндекс должен увидеть в списке раньше, чем в состоянии.
	signal(a.cloud.changes)
	a.changedLocked(nil)
	return prop
}

// setEnergy запоминает показание в кВт·ч и сообщает о нём, если оно изменилось.
func (a *Appliance) setEnergy(kwh float64) {
	a.mu.Lock()
	if a.energy != nil && *a.energy == kwh {
		a.mu.Unlock()
		return
	}
	a.energy = &kwh
	if a.values == nil {
		a.values = map[string]any{}
	}
	a.values[EnergyKey] = kwh
	a.changedLocked(nil)
}
