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
		fmt.Fprintln(stderr, "использование: hcgateway [-config путь] serve | fetch-keys | web-user [-password-stdin] [имя]")
		fmt.Fprintln(stderr, "  serve      - принимать запросы умного дома Яндекса, управлять приборами и показывать веб-интерфейс")
		fmt.Fprintln(stderr, "  fetch-keys - войти в аккаунт Home Connect и сохранить ключи и описания приборов в data_dir/appliances")
		fmt.Fprintln(stderr, "  web-user   - создать пользователя веб-интерфейса или сменить его пароль")
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
	if cmd != "web-user" && len(rest) > 0 {
		fs.Usage()
		return errors.New("лишние аргументы")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, *configPath, slog.New(slog.NewTextHandler(stderr, nil)))
	case "fetch-keys", "web-user":
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		if cmd == "web-user" {
			return webUser(cfg.DataDir, rest, stdin, stdout, stderr)
		}
		return fetchKeys(ctx, cfg.DataDir, stdin, stdout, stderr)
	default:
		fs.Usage()
		return fmt.Errorf("неизвестная команда %q", cmd)
	}
}
