// Команда hcgateway - шлюз между приборами Home Connect и Умным домом Яндекса.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hcgateway/internal/config"
)

// version - версия шлюза в журнале и в метрике hcgateway_build_info. Задаётся при сборке:
// -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hcgateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/config/config.yaml", "путь к файлу настроек")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "использование: hcgateway [-config путь] serve | fetch-keys [-region eu|na|cn|global] | web-user [-password-stdin] [имя] | unban all|адрес")
		fmt.Fprintln(stderr, "  serve      - принимать запросы умного дома Яндекса, управлять приборами и показывать веб-интерфейс")
		fmt.Fprintln(stderr, "  fetch-keys - войти в аккаунт Home Connect и сохранить ключи и описания приборов в data_dir/appliances")
		fmt.Fprintln(stderr, "  web-user   - создать пользователя веб-интерфейса или сменить его пароль")
		fmt.Fprintln(stderr, "  unban      - снять все баны или бан адреса; работающий шлюз подхватит это сам")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("не указана команда")
	}
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	if cmd != "web-user" && cmd != "fetch-keys" && cmd != "unban" && len(rest) > 0 {
		fs.Usage()
		return errors.New("лишние аргументы")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, *configPath, slog.New(slog.NewTextHandler(stderr, nil)))
	case "fetch-keys", "web-user", "unban":
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		if cmd == "web-user" {
			return webUser(cfg.DataDir, rest, stdin, stdout, stderr)
		}
		if cmd == "unban" {
			return unban(cfg.DataDir, rest, stdout, time.Now())
		}
		return fetchKeys(ctx, cfg.DataDir, rest, stdin, stdout, stderr)
	default:
		fs.Usage()
		return fmt.Errorf("неизвестная команда %q", cmd)
	}
}
