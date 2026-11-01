package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Шапка страницы прибора: значок, имя и живая метка на любой вкладке; имя для голоса;
// свой значок без пересборки прибора; раздел «Удаление и сброс».
func TestDevicePageHead(t *testing.T) {
	f := newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/devices/oven")
	for _, want := range []string{`<div class="device-head"><svg class="icon" aria-hidden="true"><use href="#i-oven"/></svg>`,
		`<p class="state" data-online aria-live="polite"><span class="chip idle">готов</span></p>`,
		`<span class="hint" data-voice>В Алисе: «Духовка», комната «Кухня»</span>`, `<option value="" selected>по типу прибора</option>`,
		`<section class="field danger" id="danger">`, `<a class="button" href="/devices/oven/delete">Удалить прибор</a>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице прибора нет %s", want)
		}
	}
	before, _ := f.gw.HomeConnect("oven")
	if _, _, to := f.post("/devices/oven", url.Values{"name": {"Духовка"}, "room": {"Кухня"}, "icon": {"light"}, "demo": {"Oven"}}); to != "/devices/oven#basic" {
		t.Fatalf("сохранение значка: переход %s", to)
	}
	if after, _ := f.gw.HomeConnect("oven"); after != before || f.config().Devices["oven"].Icon != "light" {
		t.Fatal("значок не сохранён или прибор пересобран")
	}
	if _, body = f.get("/"); !strings.Contains(body, `<use href="#i-light"/>`) {
		t.Fatal("в списке не свой значок")
	}
	f.get("/devices/oven")
	if status, body, _ := f.post("/devices/oven", url.Values{"name": {"Духовка"}, "icon": {"<svg>"}, "demo": {"Oven"}}); status != http.StatusBadRequest ||
		!strings.Contains(body, "icon") {
		t.Fatalf("чужой значок: %d", status)
	}
	// Второй прибор с тем же именем в той же комнате - предупреждение.
	f.get("/")
	f.post("/devices", url.Values{"name": {"духовка "}, "room": {"Кухня"}, "source": {"demo"}, "demo": {"Oven"}, "id": {"oven2"}})
	if _, body = f.get("/devices/oven2"); !strings.Contains(body, "В комнате «Кухня» уже есть прибор «Духовка»: голосом их не различить.") {
		t.Fatal("нет предупреждения о таком же имени")
	}
}

// Забыть ключ телевизора - только после подтверждения на отдельной странице.
func TestTVForgetConfirm(t *testing.T) {
	f := newFixture(t, "devices:\n  tv:\n    name: Телевизор\n    source: webos\n    host: 127.0.0.1\n")
	f.writeUser(testPassword)
	f.login()
	status, body := f.get("/devices/tv/tv/forget")
	if status != http.StatusOK || !strings.Contains(body, "Забыть ключ телевизора «Телевизор»?") || !strings.Contains(body, `action="/devices/tv/tv/forget"`) {
		t.Fatalf("подтверждение: %d", status)
	}
	if _, _, to := f.post("/devices/tv/tv/forget", url.Values{}); to != "/devices/tv#danger" {
		t.Fatalf("забыть ключ: переход %s", to)
	}
	if status, _ := f.get("/devices/nothing/tv/forget"); status != http.StatusNotFound {
		t.Fatalf("чужой прибор: %d", status)
	}
}
