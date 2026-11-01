// Команда hcprobe - отладочный проб протокола прибора Home Connect. Открывает
// WebSocket /homeconnect в одном из режимов (plain, aes, tls), отвечает на
// /ei/initialValues как приложение и дальше шлёт произвольные сообщения, печатая
// все входящие. Нужна, чтобы проверить pairing вживую: принимает ли прибор в
// режиме SAP запись хаба (/ce/hubInformation) и что делает при /ce/hubConnected.
//
// Запускать с машины, подключённой к сети прибора (в SAP - к Wi-Fi HomeConnect).
// Адрес прибора в SAP - шлюз этой сети.
//
// Примеры:
//
//	hcprobe -host 192.168.0.1 -mode plain -hub CN-PRODUCTION
//	hcprobe -host 192.168.0.1 -mode plain        # далее ввод сообщений построчно
//	hcprobe -host 1.2.3.4 -mode tls -key <base64> # прибор с TLS-PSK
//
// Строка ввода: ДЕЙСТВИЕ ресурс [версия] [json]. Примеры:
//
//	GET /ce/hubInformation
//	POST /ce/hubInformation 2 {"hub":"EU-PRODUCTION"}
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/hclocal"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

// Версия ресурсов /ce: по именам в библиотеке (kResourceCEV2HubInformation) - 2.
const ceVersion = 2

// Пределы ожидания: подключение с рукопожатием, ответ на /ei/initialValues.
const (
	dialWait      = 20 * time.Second
	handshakeWait = 15 * time.Second
)

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hcprobe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "адрес прибора: имя или IP, порт по желанию")
	mode := fs.String("mode", hclocal.ModePlain, "режим канала: plain, aes или tls")
	identity := fs.String("identity", hclocal.ProbeIdentity, "PSK-identity для режима tls")
	keyArg := fs.String("key", "", "ключ прибора в base64, для режимов aes и tls")
	ivArg := fs.String("iv", "", "IV в base64, для режима aes")
	hub := fs.String("hub", "", "сценарий смены хаба: прочитать /ce/hubInformation, записать этот хаб, ждать /ce/hubConnected")
	version := fs.Int("version", 1, "версия по умолчанию для строк ввода без версии")
	timeout := fs.Duration("timeout", 30*time.Second, "сколько ждать /ce/hubConnected в сценарии -hub")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "использование: hcprobe -host адрес [-mode plain|aes|tls] [-key base64] [-iv base64] [-identity id] [-hub хаб] [-timeout d]")
		fmt.Fprintln(stderr, "без -hub читает сообщения из стандартного ввода: ДЕЙСТВИЕ ресурс [версия] [json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *host == "" {
		fs.Usage()
		return errors.New("не задан -host")
	}
	psk, err := decodeArg(*keyArg)
	if err != nil {
		return fmt.Errorf("-key: %w", err)
	}
	iv, err := decodeArg(*ivArg)
	if err != nil {
		return fmt.Errorf("-iv: %w", err)
	}

	dctx, cancel := context.WithTimeout(context.Background(), dialWait)
	p, err := hclocal.Dial(dctx, *host, *mode, psk, iv, *identity)
	cancel()
	if err != nil {
		return fmt.Errorf("подключение (%s): %w", *mode, err)
	}
	defer p.Close()
	fmt.Fprintf(stdout, "подключено к %s (%s)\n", *host, *mode)

	hctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	sid, err := p.Handshake(hctx)
	cancel()
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "рукопожатие: прибор назначил сессию %d\n", sid)
	case p.Err() != nil:
		return fmt.Errorf("соединение закрыто при рукопожатии: %w", p.Err())
	default:
		fmt.Fprintln(stdout, "прибор не прислал /ei/initialValues, продолжаю с номером сессии 0")
	}

	if *hub != "" {
		return hubScenario(p, stdout, *hub, *timeout)
	}
	return interactive(p, stdin, stdout, stderr, *version)
}

// decodeArg разбирает base64: сначала raw-url (как ключи Home Connect), потом стандартный.
func decodeArg(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	s = strings.TrimRight(s, "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("не разобран base64")
	}
	return b, nil
}

// hubScenario проверяет смену хаба: читает хаб, записывает заданный и ждёт /ce/hubConnected.
func hubScenario(p *hclocal.Probe, stdout io.Writer, hub string, timeout time.Duration) error {
	data, _ := json.Marshal(map[string]any{"hub": hub})
	fmt.Fprintln(stdout, "-> GET /ce/hubInformation")
	if err := p.Send("GET", "/ce/hubInformation", ceVersion, nil); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "-> POST /ce/hubInformation {\"hub\":%q}\n", hub)
	if err := p.Send("POST", "/ce/hubInformation", ceVersion, json.RawMessage(data)); err != nil {
		return err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case m, ok := <-p.Incoming():
			if !ok {
				return fmt.Errorf("соединение закрыто: %w", p.Err())
			}
			printMsg(stdout, m)
			if m.Resource == "/ce/hubConnected" {
				fmt.Fprintln(stdout, "итог: прибор прислал /ce/hubConnected")
				return nil
			}
		case <-deadline.C:
			fmt.Fprintf(stdout, "итог: за %v прибор не прислал /ce/hubConnected\n", timeout)
			return nil
		}
	}
}

// interactive шлёт прибору сообщения из стандартного ввода и печатает все входящие.
func interactive(p *hclocal.Probe, stdin io.Reader, stdout, stderr io.Writer, defVersion int) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range p.Incoming() {
			printMsg(stdout, m)
		}
	}()
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		action, resource, version, data, err := parseLine(line, defVersion)
		if err != nil {
			fmt.Fprintln(stderr, "строка не разобрана:", err)
			continue
		}
		fmt.Fprintf(stdout, "-> %s %s v%d\n", action, resource, version)
		if err := p.Send(action, resource, version, data); err != nil {
			fmt.Fprintln(stderr, "отправка не удалась:", err)
			if p.Err() != nil {
				break
			}
		}
	}
	p.Close()
	<-done
	if err := sc.Err(); err != nil {
		return err
	}
	return nil
}

// parseLine разбирает строку ввода: ДЕЙСТВИЕ ресурс [версия] [json]. Версия - если
// третье слово целое число; остальное после него - тело JSON.
func parseLine(line string, defVersion int) (action, resource string, version int, data json.RawMessage, err error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", 0, nil, errors.New("нужно хотя бы ДЕЙСТВИЕ и ресурс")
	}
	action = strings.ToUpper(fields[0])
	resource = fields[1]
	version = defVersion
	lead := 2
	if len(fields) > 2 {
		if v, e := strconv.Atoi(fields[2]); e == nil {
			version = v
			lead = 3
		}
	}
	rem := line
	for i := 0; i < lead; i++ {
		rem = strings.TrimSpace(rem)
		_, rem, _ = strings.Cut(rem, " ")
	}
	if rem = strings.TrimSpace(rem); rem != "" {
		if !json.Valid([]byte(rem)) {
			return "", "", 0, nil, errors.New("тело не является JSON")
		}
		data = json.RawMessage(rem)
	}
	return action, resource, version, data, nil
}

// printMsg печатает входящее сообщение прибора.
func printMsg(w io.Writer, m hclocal.ProbeMessage) {
	var b strings.Builder
	fmt.Fprintf(&b, "<- %s %s v%d sID=%d msg=%d", m.Action, m.Resource, m.Version, m.SID, m.MsgID)
	if m.Code != "" {
		fmt.Fprintf(&b, " code=%s", m.Code)
	}
	for _, d := range m.Data {
		fmt.Fprintf(&b, " %s", string(d))
	}
	fmt.Fprintln(w, b.String())
}
