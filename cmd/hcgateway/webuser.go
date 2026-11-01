package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"hcgateway/internal/auth"
)

// maxPasswordInput - сколько байт пароля читать из стандартного ввода.
const maxPasswordInput = 1 << 10

// webUser создаёт пользователя веб-интерфейса или заменяет его пароль.
// Работающий шлюз подхватывает новый файл сам, прежние сессии перестают действовать.
func webUser(dataDir string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("web-user", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromStdin := fs.Bool("password-stdin", false, fmt.Sprintf("прочитать свой пароль из стандартного ввода, от %d до %d символов",
		auth.MinPasswordLen, auth.MaxPasswordLen))
	fs.Usage = func() {
		fmt.Fprintf(stderr, "использование: hcgateway [-config путь] web-user [-password-stdin] [имя, по умолчанию %s]\n", auth.DefaultName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return errors.New("лишние аргументы")
	}
	name := auth.DefaultName
	if fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	if !auth.ValidName(name) {
		return fmt.Errorf("имя %q: нужно от 1 до 32 латинских букв, цифр, _ и -", name)
	}
	var (
		password string
		err      error
	)
	if *fromStdin {
		password, err = readPassword(stdin)
	} else {
		password, err = auth.GeneratePassword()
	}
	if err != nil {
		return err
	}
	u, err := auth.NewUser(name, password)
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, auth.UserFile)
	if err := auth.SaveUser(path, u); err != nil {
		return err
	}
	if *fromStdin {
		fmt.Fprintf(stdout, "Пароль пользователя %s сохранён в %s.\n", name, path)
	} else {
		fmt.Fprintf(stdout, "Пользователь: %s\nПароль: %s\nПароль показан один раз, сохраните его.\n", name, password)
	}
	fmt.Fprintln(stdout, "Работающий шлюз подхватит пароль сам, прежние сессии закрыты.")
	return nil
}

// readPassword читает пароль - одну строку - из стандартного ввода.
func readPassword(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxPasswordInput+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxPasswordInput {
		return "", fmt.Errorf("пароль длиннее %d символов", auth.MaxPasswordLen)
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.ContainsAny(password, "\r\n") {
		return "", errors.New("пароль - одна строка")
	}
	return password, auth.CheckPassword(password)
}
