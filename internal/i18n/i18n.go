// Package i18n - язык интерфейса. Шлюз собирает страницы по-русски, а для другого языка заменяет готовый
// текст по каталогу: русский текст - ключ. Поэтому русский интерфейс работает как без перевода, а
// текст без перевода остаётся русским и не ломает страницу. Что каталог полон, проверяет тест в
// internal/web. Журнал, метрики и имена для Яндекса не переводятся.
package i18n

import (
	"bytes"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Lang - язык интерфейса.
type Lang string

const (
	RU Lang = "ru"
	EN Lang = "en"
)

// CookieName - выбранный в интерфейсе язык; без cookie - как в браузере.
const CookieName = "hcgateway-lang"

const (
	// maxHeader и maxTags - предел заголовка Accept-Language: дальше шлюз его не читает.
	maxHeader = 512
	maxTags   = 32
	// maxDepth - глубина разбора составного текста: значение внутри шаблона тоже переводится.
	maxDepth = 4
)

// Valid сообщает, что значение - язык интерфейса.
func Valid(s string) bool {
	return s == string(RU) || s == string(EN)
}

// Negotiate выбирает язык по заголовку Accept-Language: тот из двух, что браузер просит сильнее. Браузеру,
// который не просит ни один, шлюз отвечает по-английски, а запросу без заголовка - по-русски.
func Negotiate(header string) Lang {
	if strings.TrimSpace(header) == "" {
		return RU
	}
	if len(header) > maxHeader {
		header = header[:maxHeader]
	}
	best, bestQ := EN, 0.0
	tags := strings.Split(header, ",")
	for _, item := range tags[:min(len(tags), maxTags)] {
		tag, params, _ := strings.Cut(strings.TrimSpace(item), ";")
		primary, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
		if !Valid(primary) {
			continue
		}
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				continue
			}
			q = f
		}
		if q > bestQ {
			best, bestQ = Lang(primary), q
		}
	}
	return best
}

// Chosen - язык, выбранный в этом браузере; пусто - как в браузере.
func Chosen(r *http.Request) string {
	if c, err := r.Cookie(CookieName); err == nil && Valid(c.Value) {
		return c.Value
	}
	return ""
}

// FromRequest - язык ответа: выбранный в интерфейсе, иначе по заголовку браузера.
func FromRequest(r *http.Request) Lang {
	if c := Chosen(r); c != "" {
		return Lang(c)
	}
	return Negotiate(r.Header.Get("Accept-Language"))
}

// format - текст с местами для значений, например «заряд %s %».
type format struct {
	prefix, suffix string
	re             *regexp.Regexp
	value          string
	literal        int
}

// catalog - тексты одного языка.
type catalog struct {
	exact    map[string]string
	formats  []format
	maxExact int
}

func newCatalog(texts map[string]string) *catalog {
	c := &catalog{exact: map[string]string{}}
	for k, v := range texts {
		if !strings.Contains(k, "%s") {
			c.exact[k] = v
			continue
		}
		parts := strings.Split(k, "%s")
		quoted := make([]string, len(parts))
		literal := 0
		for i, p := range parts {
			quoted[i] = regexp.QuoteMeta(p)
			literal += utf8.RuneCountInString(p)
		}
		c.formats = append(c.formats, format{prefix: parts[0], suffix: parts[len(parts)-1],
			re: regexp.MustCompile(`(?s)^` + strings.Join(quoted, `(.+?)`) + `$`), value: v, literal: literal})
	}
	// Сначала самые конкретные: у «сейчас %s» больше своего текста, чем у «%s мин».
	slices.SortStableFunc(c.formats, func(a, b format) int {
		if a.literal != b.literal {
			return b.literal - a.literal
		}
		return strings.Compare(a.prefix, b.prefix)
	})
	return c
}

// enTexts - английский каталог словарём: русский текст -> перевод.
var enTexts = textsOf(enPairs)

func textsOf(pairs [][2]string) map[string]string {
	texts := make(map[string]string, len(pairs))
	for _, p := range pairs {
		texts[p[0]] = p[1]
	}
	return texts
}

var (
	english     *catalog
	englishOnce sync.Once
)

func catalogOf(l Lang) *catalog {
	if l != EN {
		return nil
	}
	englishOnce.Do(func() { english = newCatalog(enTexts) })
	return english
}

// Known сообщает, что у текста есть перевод. Для теста полноты каталога.
func Known(text string) bool {
	_, ok := enTexts[text]
	return ok
}

// Translate переводит готовый текст. Чего нет в каталоге, остаётся как есть: например, имя прибора.
func Translate(l Lang, s string) string {
	c := catalogOf(l)
	if c == nil {
		return s
	}
	return c.translate(s, 0)
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// separators - чем склеены части составного текста: части переводятся по отдельности.
var separators = []string{"; ", " · ", "|", ", ", ": ", ":"}

func (c *catalog) translate(s string, depth int) string {
	core := strings.TrimSpace(s)
	if !hasCyrillic(core) {
		return s
	}
	start := strings.Index(s, core)
	lead, trail := s[:start], s[start+len(core):]
	return lead + c.core(core, depth) + trail
}

func (c *catalog) core(s string, depth int) string {
	if v, ok := c.exact[s]; ok {
		return v
	}
	// Внутри строки подпись идёт со строчной буквы: «охлаждение» в каталоге - «Охлаждение».
	if v, ok := c.exact[upperFirst(s)]; ok {
		return lowerFirst(v)
	}
	if depth >= maxDepth {
		return s
	}
	if t, ok := c.byFormat(s, depth, true); ok {
		return t
	}
	// Подпись с именем в скобках и название в кавычках: значение внутри переводится отдельно.
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		head, inner := s[:i], s[i+2:len(s)-1]
		if h, n := c.translate(head, depth+1), c.translate(inner, depth+1); h != head || n != inner {
			return h + " (" + n + ")"
		}
	}
	if inner, ok := strings.CutPrefix(s, "«"); ok {
		if inner, ok = strings.CutSuffix(inner, "»"); ok && !strings.ContainsAny(inner, "«»") {
			// Кавычка внутри текста экранируется и не закрывает внешние кавычки раньше времени.
			return strconv.Quote(c.translate(inner, depth+1))
		}
	}
	for _, sep := range separators {
		if !strings.Contains(s, sep) {
			continue
		}
		parts := strings.Split(s, sep)
		changed := false
		for i, p := range parts {
			t := c.translate(p, depth+1)
			changed = changed || t != p
			parts[i] = t
		}
		if changed {
			return strings.Join(parts, sep)
		}
	}
	// «%s мин» подходит к любому тексту с таким концом, поэтому только после разбора на части.
	if t, ok := c.byFormat(s, depth, false); ok {
		return t
	}
	// Подпись и значение через пробел: «вода 65 %». Подпись - самые длинные первые слова из каталога.
	words := strings.Fields(s)
	for n := len(words) - 1; n > 0; n-- {
		head := strings.Join(words[:n], " ")
		tail := strings.TrimPrefix(s, head)
		if v, ok := c.exact[head]; ok {
			return v + c.translate(tail, depth+1)
		}
		if v, ok := c.exact[upperFirst(head)]; ok {
			return lowerFirst(v) + c.translate(tail, depth+1)
		}
	}
	return s
}

// byFormat ищет текст с местами для значений: с собственным началом или без него.
func (c *catalog) byFormat(s string, depth int, withPrefix bool) (string, bool) {
	for i := range c.formats {
		f := &c.formats[i]
		if (f.prefix != "") != withPrefix || !strings.HasPrefix(s, f.prefix) || !strings.HasSuffix(s, f.suffix) {
			continue
		}
		if m := f.re.FindStringSubmatch(s); m != nil {
			return fill(f.value, m[1:], func(a string) string { return c.translate(a, depth+1) }), true
		}
	}
	return "", false
}

// fill подставляет значения в перевод: %s - по порядку, %[2]s - по номеру.
func fill(value string, args []string, tr func(string) string) string {
	var b strings.Builder
	next := 0
	for i := 0; i < len(value); {
		if strings.HasPrefix(value[i:], "%s") {
			if next < len(args) {
				b.WriteString(tr(args[next]))
			}
			next++
			i += 2
			continue
		}
		if strings.HasPrefix(value[i:], "%[") && i+5 <= len(value) && value[i+3:i+5] == "]s" {
			if n := int(value[i+2] - '1'); n >= 0 && n < len(args) {
				b.WriteString(tr(args[n]))
			}
			i += 5
			continue
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String()
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

// JSON переводит все строки готового JSON: так идут потоки состояния и ответы скрипту.
func JSON(l Lang, data []byte) []byte {
	c := catalogOf(l)
	if c == nil || !hasCyrillic(string(data)) {
		return data
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return data
	}
	out, err := json.Marshal(c.walk(v))
	if err != nil {
		return data
	}
	return out
}

func (c *catalog) walk(v any) any {
	switch x := v.(type) {
	case string:
		return c.translate(x, 0)
	case []any:
		for i := range x {
			x[i] = c.walk(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = c.walk(x[k])
		}
	}
	return v
}

// translatedAttrs - атрибуты с текстом для человека. data-problems - причины для скрипта.
var translatedAttrs = regexp.MustCompile(`(\s(?:title|placeholder|aria-label|label|alt|data-problems)=")([^"]*)(")`)

// noText - элементы, в которых нет текста интерфейса: код, ввод и то, что помечено translate="no".
var noText = []string{"script", "style", "textarea", "code", "pre", "samp", "kbd"}

// visitor получает текст и значения атрибутов страницы и возвращает, чем их заменить.
type visitor func(text string) string

// walkPage проходит по разметке и отдаёт visitor текст между тегами и текст в атрибутах. Пропускает
// элементы из noText и элементы с translate="no". Разметку не меняет.
func walkPage(page []byte, visit visitor) []byte {
	var out bytes.Buffer
	out.Grow(len(page))
	skip, skipDepth := "", 0
	for i := 0; i < len(page); {
		next := bytes.IndexByte(page[i:], '<')
		if next < 0 {
			next = len(page) - i
		}
		if text := page[i : i+next]; len(text) > 0 {
			if skip == "" {
				out.WriteString(visit(string(text)))
			} else {
				out.Write(text)
			}
		}
		i += next
		if i >= len(page) {
			break
		}
		end := tagEnd(page, i)
		tag := string(page[i:end])
		i = end
		name, closing := tagName(tag)
		switch {
		case skip != "":
			if name == skip {
				if closing {
					skipDepth--
				} else if !strings.HasSuffix(tag, "/>") {
					skipDepth++
				}
				if skipDepth == 0 {
					skip = ""
				}
			}
			out.WriteString(tag)
			continue
		case !closing && name != "" && !voidElement(name) && (slices.Contains(noText, name) || strings.Contains(tag, ` translate="no"`)):
			skip, skipDepth = name, 1
			out.WriteString(tag)
			continue
		case closing || name == "":
			out.WriteString(tag)
			continue
		}
		if strings.Contains(tag, ` translate="no"`) {
			out.WriteString(tag)
			continue
		}
		out.WriteString(translatedAttrs.ReplaceAllStringFunc(tag, func(m string) string {
			p := translatedAttrs.FindStringSubmatch(m)
			return p[1] + visit(p[2]) + p[3]
		}))
	}
	return out.Bytes()
}

// tagEnd - конец тега, начатого в page[i]: > внутри кавычек тег не закрывает.
func tagEnd(page []byte, i int) int {
	var quote byte
	for j := i + 1; j < len(page); j++ {
		switch c := page[j]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return j + 1
		}
	}
	return len(page)
}

// tagName - имя тега строчными и закрывающий ли он. У комментария и <!doctype> имени нет.
func tagName(tag string) (string, bool) {
	s := strings.TrimPrefix(tag, "<")
	if strings.HasPrefix(s, "!") {
		return "", false
	}
	closing := strings.HasPrefix(s, "/")
	s = strings.TrimPrefix(s, "/")
	end := strings.IndexAny(s, " \t\r\n/>")
	if end < 0 {
		end = len(s)
	}
	return strings.ToLower(s[:end]), closing
}

func voidElement(name string) bool {
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr":
		return true
	}
	return false
}

// HTML переводит готовую страницу: текст между тегами и текст в атрибутах. Значения полей ввода не
// меняются: это данные владельца.
func HTML(l Lang, page []byte) []byte {
	c := catalogOf(l)
	if c == nil {
		return page
	}
	out := walkPage(page, func(text string) string {
		if !hasCyrillic(text) {
			return text
		}
		return html.EscapeString(c.translate(html.UnescapeString(text), 0))
	})
	return bytes.Replace(out, []byte(`<html lang="ru"`), []byte(`<html lang="`+string(l)+`"`), 1)
}

// Texts - текст страницы и атрибутов: для теста полноты каталога.
func Texts(page []byte) []string {
	var out []string
	walkPage(page, func(text string) string {
		if hasCyrillic(text) {
			out = append(out, html.UnescapeString(text))
		}
		return text
	})
	return out
}
