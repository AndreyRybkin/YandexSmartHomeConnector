// Package hcsource - приборы из облака Home Connect. Состояние приходит из одного потока
// событий на все приборы аккаунта, команды уходят через API облака (пакет hcapi).
package hcsource

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"hcgateway/internal/hcapi"
	"hcgateway/internal/metrics"
)

const (
	// Пауза перед переподключением потока: от 10 с до 10 минут, удваивается после каждой неудачи.
	// Каждое открытие потока - запрос из суточного лимита 1000, поэтому пауза не короче 10 с.
	reconnectMin = 10 * time.Second
	reconnectMax = 10 * time.Minute
	// stableStream - поток, который проработал столько, считается удачным: пауза снова минимальная.
	stableStream = time.Minute
)

// Cloud - приборы аккаунта Home Connect и поток событий.
type Cloud struct {
	API *hcapi.Client
	dir string
	log *slog.Logger
	// changes - описание прибора или список приборов аккаунта изменились: шлюз пересобирает приборы.
	changes chan struct{}
	// wake - вход выполнен: подключиться, не дожидаясь паузы.
	wake chan struct{}

	mu         sync.Mutex
	appliances map[string]*Appliance
	account    []hcapi.HomeAppliance
	connected  bool
}

// New создаёт облако. dir - каталог для описаний приборов, data_dir/homeconnect.
func New(api *hcapi.Client, dir string, log *slog.Logger) *Cloud {
	return &Cloud{API: api, dir: dir, log: log, changes: make(chan struct{}, 1), wake: make(chan struct{}, 1),
		appliances: map[string]*Appliance{}}
}

// Changes сообщает, что описание прибора или список приборов аккаунта изменились.
func (c *Cloud) Changes() <-chan struct{} {
	return c.changes
}

func (c *Cloud) changed() {
	select {
	case c.changes <- struct{}{}:
	default:
	}
}

// Appliance - прибор с haId. Для одного haId - всегда один объект: он переживает смену имени
// и профиля прибора в настройках. Описание прибора берётся из кэша на диске.
func (c *Cloud) Appliance(haID string) *Appliance {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.appliances[haID]; a != nil {
		return a
	}
	a := newAppliance(c, haID, filepath.Join(c.dir, haID, "description.json"))
	c.appliances[haID] = a
	// Прибор добавили после загрузки списка аккаунта: тип и связь известны сразу.
	for _, h := range c.account {
		if h.HaID == haID {
			a.identify(h)
		}
	}
	if c.connected {
		go a.load(context.Background())
	}
	return a
}

// Forget забывает объект прибора, которым шлюз больше не управляет через облако: поток событий не
// будет тратить на него запросы. Описание прибора остаётся на диске.
func (c *Cloud) Forget(haID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.appliances, haID)
}

// Account - приборы аккаунта, как их последний раз сообщило облако.
func (c *Cloud) Account() []hcapi.HomeAppliance {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.account)
}

// Connected сообщает, подключён ли поток событий.
func (c *Cloud) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// RefreshAccount загружает список приборов аккаунта и отмечает, какие на связи.
func (c *Cloud) RefreshAccount(ctx context.Context) error {
	list, err := c.API.Appliances(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.account = list
	known := map[string]*Appliance{}
	for k, v := range c.appliances {
		known[k] = v
	}
	c.mu.Unlock()
	for _, h := range list {
		if a := known[h.HaID]; a != nil {
			a.identify(h)
		}
	}
	c.changed()
	return nil
}

// LoggedIn - вход выполнен: подключить поток событий сейчас.
func (c *Cloud) LoggedIn() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run держит поток событий и переподключается после обрыва, пока ctx не отменён.
func (c *Cloud) Run(ctx context.Context) {
	pause := reconnectMin
	for {
		if !c.API.LoggedIn() {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}
		started := time.Now()
		err := c.API.Events(ctx, func() { c.onConnected(ctx) }, c.handle)
		c.setConnected(false)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, hcapi.ErrLoginChanged) {
			// Новый вход подключается сразу, без паузы; после выхода цикл ждёт нового входа.
			pause = reconnectMin
			continue
		}
		if time.Since(started) >= stableStream {
			pause = reconnectMin
		}
		c.log.Warn("поток событий Home Connect оборвался, переподключение", "err", err, "after", pause)
		metrics.SourceReconnect(hcapi.SourceName, "")
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			pause = reconnectMin
		case <-time.After(pause):
			pause = min(pause*2, reconnectMax)
		}
	}
}

func (c *Cloud) setConnected(up bool) {
	c.mu.Lock()
	c.connected = up
	list := make([]*Appliance, 0, len(c.appliances))
	for _, a := range c.appliances {
		list = append(list, a)
	}
	c.mu.Unlock()
	metrics.SourceConnected(hcapi.SourceName, "", up)
	if !up {
		// Без потока шлюз не знает, что с приборами: до переподключения они не на связи.
		for _, a := range list {
			a.setOnline(false)
		}
	}
}

// onConnected - поток открыт: загрузить список приборов и состояние каждого прибора на связи.
// События, которые пришли, пока потока не было, потеряны. Описания загружаются после всех
// состояний: их запросы идут с паузами, а прибор до загрузки состояния не на связи.
func (c *Cloud) onConnected(ctx context.Context) {
	c.setConnected(true)
	c.log.Info("поток событий Home Connect подключён")
	go func() {
		if err := c.RefreshAccount(ctx); err != nil {
			c.log.Warn("список приборов Home Connect не загружен", "err", err)
			return
		}
		c.mu.Lock()
		list := make([]*Appliance, 0, len(c.appliances))
		for _, a := range c.appliances {
			list = append(list, a)
		}
		c.mu.Unlock()
		loaded := make([]*Appliance, 0, len(list))
		for _, a := range list {
			if a.reachable() && a.refresh(ctx) {
				loaded = append(loaded, a)
			}
		}
		for _, a := range loaded {
			a.describe(ctx)
		}
	}()
}

// handle применяет событие потока к прибору.
func (c *Cloud) handle(ev hcapi.Event) {
	switch ev.Type {
	case hcapi.EventKeepAlive:
		return
	case hcapi.EventPaired, hcapi.EventDepaired:
		go func() {
			if err := c.RefreshAccount(context.Background()); err != nil {
				c.log.Warn("список приборов Home Connect не загружен", "err", err)
			}
		}()
	}
	c.mu.Lock()
	a := c.appliances[ev.HaID]
	c.mu.Unlock()
	if a == nil {
		return
	}
	switch ev.Type {
	case hcapi.EventConnected:
		go a.load(context.Background())
	case hcapi.EventDisconnected, hcapi.EventDepaired:
		a.setOnline(false)
	case hcapi.EventStatus, hcapi.EventEvent, hcapi.EventNotify:
		a.apply(ev)
	}
}
