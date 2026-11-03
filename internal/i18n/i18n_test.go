package i18n

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestNegotiate(t *testing.T) {
	for header, want := range map[string]Lang{
		"":                                RU,
		"ru":                              RU,
		"ru-RU,ru;q=0.9,en-US;q=0.8":      RU,
		"en-US,en;q=0.9":                  EN,
		"en;q=0.5,ru;q=0.9":               RU,
		"EN-gb":                           EN,
		"de-DE,de;q=0.9":                  EN,
		"de,ru;q=0.3":                     RU,
		"ru;q=0,en;q=0.1":                 EN,
		"*":                               EN,
		"ru;q=плохо,en":                   EN,
		strings.Repeat("de,", 400) + "ru": EN,
	} {
		if got := Negotiate(header); got != want {
			t.Errorf("%.40q: %s, нужно %s", header, got, want)
		}
	}
}

func TestFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "en")
	if FromRequest(r) != EN || Chosen(r) != "" {
		t.Fatal("язык браузера")
	}
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "ru"})
	if FromRequest(r) != RU || Chosen(r) != "ru" {
		t.Fatal("выбранный язык главнее браузера")
	}
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "en")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "xx"})
	if FromRequest(r) != EN || Chosen(r) != "" {
		t.Fatal("чужое значение cookie")
	}
}

func TestTranslate(t *testing.T) {
	c := newCatalog(map[string]string{
		"Работает":              "Running",
		"работает":              "running",
		"Охлаждение":            "Cooling",
		"вода":                  "water",
		"осталось %s":           "%s left",
		"%s мин":                "%s min",
		"%s ч %s мин":           "%s h %s min",
		"В комнате «%s» нет %s": "No %[2]s in room «%[1]s»",
		"прибор":                "device",
		"Заряд батареи":         "Battery level",
		"С кавычкой":            `With "quote"`,
		"заряд %s %":            "battery %s%",
	})
	for in, want := range map[string]string{
		"Работает":                 "Running",
		"  работает ":              "  running ",
		"охлаждение":               "cooling",
		"Latin":                    "Latin",
		"осталось 5 мин":           "5 min left",
		"осталось 1 ч 25 мин":      "1 h 25 min left",
		"работает, осталось 5 мин": "running, 5 min left",
		"вода 65 %":                "water 65 %",
		"62 °C · вода 65 %":        "62 °C · water 65 %",
		"В комнате «Кухня» нет прибор": "No device in room «Кухня»",
		"заряд 12 %":      "battery 12%",
		"Чужой текст":     "Чужой текст",
		"«Заряд батареи»": `"Battery level"`,
		"«Кухня»":         `"Кухня"`,
		"«С кавычкой»":    `"With \"quote\""`,
	} {
		if got := c.translate(in, 0); got != want {
			t.Errorf("%q: %q, нужно %q", in, got, want)
		}
	}
}

func TestHTML(t *testing.T) {
	saved := english
	englishOnce.Do(func() {})
	english = newCatalog(map[string]string{"Сохранить": "Save", "Подсказка": "Hint", "Имя": "Name", "Кавычка «%s»": `Quote "%s"`})
	defer func() { english = saved }()
	page := `<!doctype html><html lang="ru"><head><title>Сохранить</title></head><body>
<button title="Подсказка">Сохранить</button><input value="Сохранить" placeholder="Имя">
<code>Сохранить</code><textarea>Сохранить</textarea><span translate="no">Сохранить <b>Сохранить</b></span>
<script>"Сохранить"</script><p>Кавычка «De&#39;Longhi»</p><a href="/x>y" title="Имя">Чужой</a></body></html>`
	want := `<!doctype html><html lang="en"><head><title>Save</title></head><body>
<button title="Hint">Save</button><input value="Сохранить" placeholder="Name">
<code>Сохранить</code><textarea>Сохранить</textarea><span translate="no">Сохранить <b>Сохранить</b></span>
<script>"Сохранить"</script><p>Quote &#34;De&#39;Longhi&#34;</p><a href="/x>y" title="Name">Чужой</a></body></html>`
	if got := string(HTML(EN, []byte(page))); got != want {
		t.Fatalf("перевод:\n%s\nнужно:\n%s", got, want)
	}
	if got := string(HTML(RU, []byte(page))); got != page {
		t.Fatalf("русская страница изменилась: %s", got)
	}
}

func TestJSON(t *testing.T) {
	saved := english
	englishOnce.Do(func() {})
	english = newCatalog(map[string]string{"Работает": "Running"})
	defer func() { english = saved }()
	in := `{"state":"Работает","n":1.50,"list":["Работает","x"],"ok":true}`
	if got := string(JSON(RU, []byte(in))); got != in {
		t.Fatalf("русский: %s", got)
	}
	want := `{"list":["Running","x"],"n":1.50,"ok":true,"state":"Running"}`
	if got := string(JSON(EN, []byte(in))); got != want {
		t.Fatalf("английский: %s", got)
	}
	if got := string(JSON(EN, []byte(`не json`))); got != `не json` {
		t.Fatalf("не JSON: %s", got)
	}
}

// Каталог записан парами, поэтому повтор ключа компилятор не заметит. Перевод не теряет мест для значений
// и не содержит русского текста.
func TestCatalogPairs(t *testing.T) {
	places := regexp.MustCompile(`%(\[\d\])?s`)
	seen := map[string]bool{}
	for _, p := range enPairs {
		key, value := p[0], p[1]
		if seen[key] {
			t.Errorf("ключ повторяется: %q", key)
		}
		seen[key] = true
		if value == "" || hasCyrillic(value) {
			t.Errorf("перевод %q: %q", key, value)
		}
		if strings.Count(key, "%s") != len(places.FindAllString(value, -1)) {
			t.Errorf("места для значений: %q -> %q", key, value)
		}
	}
	if len(enTexts) != len(enPairs) {
		t.Errorf("в словаре %d текстов, в каталоге %d", len(enTexts), len(enPairs))
	}
}
