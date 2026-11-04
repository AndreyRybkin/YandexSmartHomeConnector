package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"hcgateway/internal/i18n"
)

// textPackages - пакеты, тексты которых доходят до страниц: сам интерфейс, ошибки настроек и профиля,
// страница привязки, причины сбоя SmartThings и поиска, время и имя шлюза в журнале брокера.
var textPackages = []string{".", "../config", "../profile", "../linking", "../discovery", "../smartthings", "../mqttbroker", "../hcdevice"}

var (
	goVerb         = regexp.MustCompile(`%[-+# 0]*[0-9]*(\.[0-9]+)?[a-zA-Z]`)
	templateNoise  = regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)
	templateAct    = regexp.MustCompile(`(?s)\{\{-?\s*(.*?)\s*-?\}\}`)
	templateString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	templateCtrl   = regexp.MustCompile(`^(if|else|end|range|with|define|template|block|\$[A-Za-z]+\s*:=)\b?`)
)

func cyrillic(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) >= 0
}

// normText - текст в виде ключа каталога: значения - %s, без краевых пробелов.
func normText(s string) string {
	s = strings.ReplaceAll(s, "%%", "\x00")
	s = goVerb.ReplaceAllString(s, "%s")
	return strings.TrimSpace(strings.ReplaceAll(s, "\x00", "%"))
}

// foldText - текст из склейки строк: всё, что не строка, - %s.
func foldText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				return s
			}
		}
	case *ast.ParenExpr:
		return foldText(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return foldText(x.X) + foldText(x.Y)
		}
	}
	return "%s"
}

// logCall - вызов журнала: его тексты не переводятся.
func logCall(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Info", "Warn", "Error", "Debug":
	default:
		return false
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name == "log" || x.Name == "slog" || x.Name == "logger"
	case *ast.SelectorExpr:
		return x.Sel.Name == "log"
	}
	return false
}

// goTexts - тексты интерфейса из исходников Go.
func goTexts(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	add := func(s string) {
		if strings.HasPrefix(s, "<!doctype") || strings.HasPrefix(s, "%s<!doctype") {
			out = append(out, templateTexts(s)...)
			return
		}
		// Отдельная буква - таблица транслитерации, а не текст.
		if s = normText(s); cyrillic(s) && utf8.RuneCountInString(s) > 1 {
			out = append(out, s)
		}
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				return !logCall(x)
			case *ast.BinaryExpr:
				if x.Op != token.ADD {
					return true
				}
				add(foldText(x))
				// Значения внутри склейки могут содержать свои тексты, а её строки уже учтены.
				var inner func(e ast.Expr)
				inner = func(e ast.Expr) {
					switch y := e.(type) {
					case *ast.BinaryExpr:
						if y.Op == token.ADD {
							inner(y.X)
							inner(y.Y)
							return
						}
						ast.Inspect(y, func(c ast.Node) bool { return visitText(c, add) })
					case *ast.BasicLit:
					default:
						ast.Inspect(e, func(c ast.Node) bool { return visitText(c, add) })
					}
				}
				inner(x)
				return false
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if s, err := strconv.Unquote(x.Value); err == nil {
						add(s)
					}
				}
			}
			return true
		})
	}
	return out
}

func visitText(n ast.Node, add func(string)) bool {
	switch x := n.(type) {
	case *ast.CallExpr:
		return !logCall(x)
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				add(s)
			}
		}
	}
	return true
}

// templateTexts - тексты шаблона: вывод значения - %s, условие и цикл разрывают текст.
func templateTexts(src string) []string {
	src = templateNoise.ReplaceAllString(src, "")
	var out []string
	src = templateAct.ReplaceAllStringFunc(src, func(m string) string {
		// Текст может быть и внутри вызова: {{template "шаг" (args "Вход по коду")}}.
		for _, lit := range templateString.FindAllString(m, -1) {
			if s, err := strconv.Unquote(lit); err == nil && cyrillic(s) {
				out = append(out, normText(s))
			}
		}
		if templateCtrl.MatchString(templateAct.FindStringSubmatch(m)[1]) {
			return "\x01"
		}
		return "%s"
	})
	for _, text := range i18n.Texts([]byte(src)) {
		for _, part := range strings.Split(text, "\x01") {
			if part = normText(part); cyrillic(part) {
				out = append(out, part)
			}
		}
	}
	return out
}

// uiTexts - все тексты интерфейса: код, шаблоны и скрипт.
func uiTexts(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range textPackages {
		out = append(out, goTexts(t, dir)...)
	}
	pages, err := filepath.Glob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, templateTexts(string(data))...)
	}
	script, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range scriptKey.FindAllSubmatch(script, -1) {
		if key, err := strconv.Unquote(`"` + string(m[1]) + `"`); err == nil {
			out = append(out, key)
		}
	}
	manifest, err := os.ReadFile("static/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`"[^"]*[А-Яа-я][^"]*"`).FindAll(manifest, -1) {
		out = append(out, strings.Trim(string(m), `"`))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Каталог полон: у каждого текста интерфейса есть перевод, а значит, новый текст нельзя забыть.
func TestCatalogComplete(t *testing.T) {
	var missing []string
	for _, s := range uiTexts(t) {
		if !i18n.Known(s) {
			missing = append(missing, s)
		}
	}
	if dump := os.Getenv("I18N_DUMP"); dump != "" {
		if err := os.WriteFile(dump, []byte(strings.Join(missing, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("нет перевода у %d текстов, первые: %q", len(missing), missing[:min(len(missing), 10)])
	}
}

// englishConfig - приборы всех источников: у каждого свои разделы на страницах.
const englishConfig = `mqtt:
  password: abcdefghijklmnopqrstuvwx
thinq:
  pat: test-pat
  country: NL
smartthings:
  client_id: id
  client_secret: secret
devices:
  oven:
    name: Духовка
    room: Кухня
    source: demo
    demo: Oven
  dish:
    name: Посудомойка
    source: demo
    demo: Dishwasher
  strip:
    name: Лента
    source: wled
    host: 192.168.1.20
    presets:
      1: party
  tv:
    name: Телевизор
    source: webos
    host: 192.168.1.30
  samsung:
    name: Самсунг
    source: samsung-tv
    host: 192.168.1.31
  coffee:
    name: Кофе
    source: delonghi
    host: 192.168.1.32
    serial: ABC123
  kettle:
    name: Чайник
    source: polaris
    device_id: aabbccddeeff
  lg:
    name: Стирка
    source: thinq
    device_id: dev-1
  st:
    name: Лампа
    source: smartthings
    device_id: 00000000-0000-0000-0000-000000000001
`

// В английском интерфейсе нет русского текста, кроме имён владельца.
func TestEnglishPages(t *testing.T) {
	f := newFixture(t, englishConfig)
	f.writeUser(testPassword)
	f.accept = "en-US,en;q=0.9"
	own := []string{"Духовка", "Кухня", "Посудомойка", "Лента", "Телевизор", "Самсунг", "Кофе", "Чайник", "Стирка", "Лампа"}
	check := func(path string) {
		t.Helper()
		_, body := f.get(path)
		if !strings.Contains(body, `<html lang="en"`) && strings.Contains(body, "<html") {
			t.Errorf("%s: язык страницы", path)
		}
		for _, text := range i18n.Texts([]byte(body)) {
			for _, name := range own {
				text = strings.ReplaceAll(text, name, "")
			}
			if cyrillic(text) {
				t.Errorf("%s: %q", path, strings.TrimSpace(text))
			}
		}
	}
	check("/login")
	check("/notfound-page")
	f.accept = ""
	f.login()
	f.accept = "en"
	for _, path := range []string{"/", "/?problems=1", "/connections", "/logins", "/mqtt", "/mqtt/publish?topic=a%2Fb&payload=1", "/mqtt/password", "/profiles", "/profiles?all=1", "/profiles/Oven",
		"/profiles/DEVICE_WASHER", "/profiles/PolarisKettle", "/connections/yandex/unlink", "/connections/homeconnect/logout", "/connections/delonghi/logout", "/connections/smartthings/logout", "/connections/local/logout",
		"/devices/oven", "/devices/oven/info", "/devices/oven/delete", "/devices/dish", "/devices/strip", "/devices/strip/info", "/devices/tv", "/devices/tv/info", "/devices/tv/forget", "/devices/samsung", "/devices/coffee", "/devices/coffee/info", "/devices/kettle", "/devices/kettle/info", "/devices/lg", "/devices/st"} {
		check(path)
	}
}

// Язык: заголовок браузера, выбор в интерфейсе главнее него, «как в браузере» возвращает заголовок.
func TestLanguageChoice(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/")
	if !strings.Contains(body, `<html lang="ru"`) || !strings.Contains(body, "Приборы") || strings.Contains(body, "data-i18n") {
		t.Fatalf("без заголовка - русский: %.200s", body)
	}
	f.accept = "en-US,en;q=0.9"
	status, body, resp := f.do(http.MethodGet, "/", nil)
	if status != http.StatusOK || !strings.Contains(body, `<html lang="en"`) || !strings.Contains(body, ">Devices<") || !strings.Contains(body, "data-i18n") ||
		resp.Header.Get("Content-Language") != "en" {
		t.Fatalf("английский браузер: %d %.300s", status, body)
	}
	// Имя владельца не переводится.
	if !strings.Contains(body, `translate="no">Духовка<`) {
		t.Fatal("имя прибора изменилось")
	}
	f.get("/")
	status, _, resp = f.do(http.MethodPost, "/lang", url.Values{"csrf": {f.csrf}, "lang": {"ru"}, "back": {"/connections"}})
	if status != http.StatusSeeOther || resp.Header.Get("Location") != "/connections" {
		t.Fatalf("выбор языка: %d %s", status, resp.Header.Get("Location"))
	}
	var chosen *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == i18n.CookieName {
			chosen = c
		}
	}
	if chosen == nil || chosen.Value != "ru" || !chosen.Secure || chosen.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie языка: %+v", chosen)
	}
	f.langCookie = chosen.Value
	if _, body = f.get("/"); !strings.Contains(body, `<html lang="ru"`) || !strings.Contains(body, `value="ru" selected`) {
		t.Fatalf("выбранный язык главнее браузера: %.200s", body)
	}
	f.langCookie = ""
	f.get("/")
	status, _, resp = f.do(http.MethodPost, "/lang", url.Values{"csrf": {f.csrf}, "lang": {""}, "back": {"https://evil.example/"}})
	if status != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("сброс языка: %d %s", status, resp.Header.Get("Location"))
	}
	if c := resp.Cookies()[0]; c.MaxAge >= 0 {
		t.Fatalf("cookie не снята: %+v", c)
	}
}

// Составной текст на английском: состояние, поток списка и скрипт.
func TestEnglishComposite(t *testing.T) {
	for in, want := range map[string]string{
		"работает, Выпечка, осталось 1 ч 25 мин":  "running, Baking, 1 h 25 min left",
		"выключен, выбрана программа Экономичный": "off, program Eco selected",
		"нет связи 5 мин":                             "offline 5 min",
		"62.4 °C · вода 65 % · цель 80 °C":            "62.4 °C · water 65 % · target 80 °C",
		"дверца открыта · задано 4 °C":                "door open · set to 4 °C",
		"вход первый · громкость 20 % · канал 5":      "input first · volume 20 % · channel 5",
		"скорость второй":                             "speed second",
		"заряд 12 %":                                  "battery 12 %",
		"Прибора oven нет в настройках":               "Device oven is not in the settings",
		"нужно пополнить, удалите накипь и ещё 1":     "needs refilling, descale the machine and 1 more",
		"тревога машины: Imbocco":                     "machine alarm: Imbocco",
		"датчик: button double_click; удалите накипь": "sensor: button double_click; descale the machine",
	} {
		if got := i18n.Translate(i18n.EN, in); got != want {
			t.Errorf("%q: %q, нужно %q", in, got, want)
		}
	}
}
