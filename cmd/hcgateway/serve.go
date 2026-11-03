package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"hcgateway/internal/auth"
	"hcgateway/internal/config"
	"hcgateway/internal/gateway"
	"hcgateway/internal/hcapi"
	"hcgateway/internal/linking"
	"hcgateway/internal/metrics"
	"hcgateway/internal/mqttbroker"
	"hcgateway/internal/statedb"
	"hcgateway/internal/web"
)

func serve(ctx context.Context, configPath string, log *slog.Logger) error {
	file, err := config.OpenFile(configPath)
	if err != nil {
		return err
	}
	cfg, _ := file.Current()
	if err := cfg.ValidateServe(); err != nil {
		return err
	}
	store, err := linking.OpenStore(filepath.Join(cfg.DataDir, "yandex-tokens.json"))
	if err != nil {
		return err
	}
	users := auth.NewUsers(filepath.Join(cfg.DataDir, auth.UserFile), log)
	state := statedb.Open(filepath.Join(cfg.DataDir, statedb.FileName), log)
	defer state.Close()
	// Проверка входа общая для веб-интерфейса и страницы привязки: у них один бан.
	login := auth.New(users, cfg.Proxies, log)
	login.Bans.Attach(state)
	gw, err := gateway.New(cfg.DataDir, store, login, log)
	if err != nil {
		return err
	}
	gw.Attach(state)
	if err := gw.Apply(cfg); err != nil {
		return err
	}
	// Без пользователя нельзя войти ни в интерфейс, ни на страницу привязки: об этом пишет журнал.
	users.Current()
	metrics.SetBuildInfo(version)
	metrics.Watch(metricsState{gw: gw, bans: login.Bans})
	// Один порт: запросы Яндекса на /v1.0 и /auth, веб-интерфейс - на остальных адресах.
	ui := web.New(login, gw, file, cfg, log)
	// Без пароля порт брокера закрыт. Логин и пароль берутся из применённых настроек: новый пароль
	// действует сразу.
	var broker *mqttbroker.Broker
	if cfg.MQTT.Password != "" {
		broker = mqttbroker.New(mqttbroker.Config{Listen: cfg.MQTT.Listen, Credentials: func() (string, string) {
			c := gw.Config()
			return c.MQTT.Username, c.MQTT.Password
		}}, state, log)
		ui.AttachBroker(broker)
	}
	servers := []*http.Server{newServer(cfg.Listen, web.Handler(ui, gw.Handler()))}
	metricsAddr := "закрыт: не задан metrics.token"
	if cfg.Metrics.Token != "" {
		// Токен берётся из применённых настроек: новый токен из интерфейса действует сразу.
		servers = append(servers, newServer(cfg.Metrics.Listen, metrics.Handler(func() string { return gw.Config().Metrics.Token })))
		metricsAddr = cfg.Metrics.Listen
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		gw.Run(runCtx)
	}()
	brokerDone := make(chan struct{})
	go func() {
		defer close(brokerDone)
		if broker != nil {
			broker.Run(runCtx)
		}
	}()

	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() { errc <- srv.ListenAndServe() }()
	}
	log.Info("шлюз запущен", "version", version, "listen", cfg.Listen, "metrics", metricsAddr,
		"devices", len(cfg.Devices))
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	for _, srv := range servers {
		srv.Shutdown(shutdown)
	}
	cancel()
	<-brokerDone
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// metricsState - состояние шлюза для метрик.
type metricsState struct {
	gw   *gateway.Gateway
	bans *auth.Bans
}

func (m metricsState) Linked() bool              { return m.gw.Tokens.Linked() }
func (m metricsState) Banned() int               { return m.bans.Active() }
func (m metricsState) Devices() []metrics.Device { return m.gw.Status() }
func (m metricsState) TokenExpires() map[string]time.Time {
	if t, ok := m.gw.Cloud.API.Expires(); ok {
		return map[string]time.Time{hcapi.SourceName: t}
	}
	return nil
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}
