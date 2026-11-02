package web

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"hcgateway/internal/appliance"
	"hcgateway/internal/hcapi/hcfake"
	"hcgateway/internal/hclocal"
)

const moveCloudID = "385000000000000000-001"
const moveLocalID = "385000000000000000"

// moveFixture - прибор из облака и ключи того же прибора, у них разный суффикс haId.
func moveFixture(t *testing.T, connected bool, extra string) *fixture {
	t.Helper()
	fake := hcfake.New(t, &hcfake.Appliance{HaID: moveCloudID, Name: "Посудомойка", Type: "Dishwasher", Brand: "Bosch",
		Connected: connected, Status: map[string]any{}, Settings: map[string]any{}})
	cfg := "homeconnect:\n  client_id: " + hcfake.ClientID + "\n  client_secret: " + hcfake.ClientSecret + "\n  api_url: " + fake.URL + "\n" +
		"devices:\n  dish:\n    name: Посудомойка\n    room: Кухня\n    icon: dishwasher\n    source: homeconnect-cloud\n    ha_id: " + moveCloudID + "\n" + extra
	f := newFixture(t, cfg, func(dir string) {
		fake.TokenFile(t, filepath.Join(dir, "homeconnect-tokens.json"))
		writeLocalKeys(t, dir, moveLocalID)
	})
	f.writeUser(testPassword)
	f.login()
	f.get("/connections")
	return f
}

// Перевод туда и обратно: источник меняется, идентификатор, имя, комната и значок остаются.
func TestMoveDevice(t *testing.T) {
	f := moveFixture(t, true, "")
	var checked []string
	f.gw.CheckLocal = func(_ context.Context, haID, host string) error {
		checked = append(checked, haID+"@"+host)
		return nil
	}
	_, body := f.get("/devices/dish")
	if !strings.Contains(body, `href="/devices/dish/move">Перевести на локальную связь</a>`) {
		t.Fatal("нет кнопки перевода")
	}
	_, body = f.get("/devices/dish/move")
	if !strings.Contains(body, moveLocalID) || !strings.Contains(body, `name="host"`) {
		t.Fatal("форма не предлагает ключи прибора")
	}
	status, _, to := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}})
	if status != 303 || to != "/devices/dish#move" {
		t.Fatalf("перевод: %d %s", status, to)
	}
	if len(checked) != 1 || checked[0] != moveLocalID+"@192.168.1.60" {
		t.Fatalf("проверка связи: %v", checked)
	}
	d := f.config().Devices["dish"]
	if d.Source != "homeconnect-local" || d.HaID != moveLocalID || d.Host != "192.168.1.60" || d.Name != "Посудомойка" ||
		d.Room != "Кухня" || d.Icon != "dishwasher" {
		t.Fatalf("настройки: %+v", d)
	}
	if _, ok := f.gw.Device("dish"); !ok {
		t.Fatal("прибор не собран")
	}
	_, body = f.get("/devices/dish")
	if !strings.Contains(body, "Прибор переведён на локальную связь.") || !strings.Contains(body, `href="/devices/dish/move">Перевести в облако</a>`) {
		t.Fatal("нет итога или кнопки обратного перевода")
	}
	_, _, to = f.post("/devices/dish/move", url.Values{})
	if to != "/devices/dish#move" {
		t.Fatalf("перевод в облако: %s", to)
	}
	d = f.config().Devices["dish"]
	if d.Source != "homeconnect-cloud" || d.HaID != moveCloudID || d.Host != "" || d.Name != "Посудомойка" {
		t.Fatalf("настройки после обратного перевода: %+v", d)
	}
	if _, ok := f.gw.Device("dish"); !ok {
		t.Fatal("прибор облака пропал из Алисы")
	}
	if _, body = f.get("/devices/dish"); !strings.Contains(body, "Прибор переведён в облако Home Connect.") {
		t.Fatal("нет итога обратного перевода")
	}
}

// Проверка не прошла: настройки не меняются, причина названа, адрес остаётся в форме.
func TestMoveRefused(t *testing.T) {
	f := moveFixture(t, true, "")
	for _, c := range []struct {
		err  error
		want string
	}{
		{hclocal.ErrWrongKey, "не принял ключ"},
		{errors.New("connection refused"), "Нет связи с прибором по этому адресу"},
	} {
		f.gw.CheckLocal = func(context.Context, string, string) error { return c.err }
		status, body, _ := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}})
		if status != 400 || !strings.Contains(body, c.want) || !strings.Contains(body, `value="192.168.1.60"`) || strings.Contains(body, "connection refused") {
			t.Fatalf("%v: %d %s", c.err, status, body)
		}
		if d := f.config().Devices["dish"]; d.Source != "homeconnect-cloud" || d.HaID != moveCloudID {
			t.Fatalf("настройки изменились: %+v", d)
		}
	}
	f.gw.CheckLocal = func(context.Context, string, string) error {
		t.Error("плохой адрес дошёл до проверки связи")
		return nil
	}
	if status, body, _ := f.post("/devices/dish/move", url.Values{"host": {"a b/c"}}); status != 400 || !strings.Contains(body, "Нужен адрес прибора") {
		t.Fatalf("плохой адрес: %d", status)
	}
	// Настройки изменились, пока открыта страница: прибор остаётся как был.
	f.gw.CheckLocal = func(context.Context, string, string) error { return nil }
	if status, body, _ := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}, "version": {"чужая"}}); status != 400 || !strings.Contains(body, "Настройки изменились") {
		t.Fatalf("устаревшая страница: %d", status)
	}
	if d := f.config().Devices["dish"]; d.Source != "homeconnect-cloud" {
		t.Fatalf("настройки изменились: %+v", d)
	}
}

// Перевод в облако требует прибор на связи в аккаунте.
func TestMoveToCloudOffline(t *testing.T) {
	f := moveFixture(t, false, "")
	f.gw.CheckLocal = func(context.Context, string, string) error { return nil }
	if _, _, to := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}}); to != "/devices/dish#move" {
		t.Fatalf("на локальную связь: %s", to)
	}
	f.get("/connections")
	status, body, _ := f.post("/devices/dish/move", url.Values{})
	if status != 400 || !strings.Contains(body, "Облако сообщает, что прибор не на связи") {
		t.Fatalf("прибор не на связи: %d", status)
	}
	if d := f.config().Devices["dish"]; d.Source != "homeconnect-local" || d.Host != "192.168.1.60" {
		t.Fatalf("настройки изменились: %+v", d)
	}
}

// Перевод невозможен: страница называет причину и ведёт туда, где её убрать.
func TestMoveBlocked(t *testing.T) {
	f := moveFixture(t, true, "  other:\n    name: Другая\n    source: homeconnect-local\n    ha_id: "+moveLocalID+"\n    host: 192.168.1.61\n")
	if status, body, _ := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}}); status != 400 || !strings.Contains(body, `<a href="/devices/other">other</a>`) {
		t.Fatalf("дубль: %d", status)
	}
	f = newFixture(t, "devices:\n  dish:\n    name: П\n    source: homeconnect-cloud\n    ha_id: "+moveCloudID+"\n")
	f.writeUser(testPassword)
	f.login()
	if _, body := f.get("/devices/dish/move"); !strings.Contains(body, `<a href="/connections#local-login">Загрузите ключи</a>`) {
		t.Fatal("нет ссылки на загрузку ключей")
	}
	f = newFixture(t, "devices:\n  dish:\n    name: П\n    source: homeconnect-local\n    ha_id: "+moveLocalID+"\n    host: 192.168.1.60\n",
		func(dir string) { writeLocalKeys(t, dir, moveLocalID) })
	f.writeUser(testPassword)
	f.login()
	if _, body := f.get("/devices/dish/move"); !strings.Contains(body, `<a href="/connections#homeconnect">Войдите</a>`) {
		t.Fatal("нет ссылки на вход")
	}
	if status, _ := f.get("/devices/нет/move"); status != 404 {
		t.Fatalf("чужой прибор: %d", status)
	}
	f = newFixture(t, demoConfig)
	f.writeUser(testPassword)
	f.login()
	if status, _ := f.get("/devices/oven/move"); status != 404 {
		t.Fatalf("демо-прибор: %d", status)
	}
}

// Несколько подходящих ключей: перевод только после выбора.
func TestMoveChoice(t *testing.T) {
	f := moveFixture(t, true, "")
	writeLocalKeys(t, f.dir, moveLocalID+"-002")
	f.gw.CheckLocal = func(context.Context, string, string) error { return nil }
	status, body, _ := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}})
	if status != 400 || !strings.Contains(body, "Выберите прибор.") || !strings.Contains(body, `<select name="ha_id">`) {
		t.Fatalf("без выбора: %d", status)
	}
	if status, _, _ := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}, "ha_id": {"чужой"}}); status != 400 {
		t.Fatalf("чужой прибор: %d", status)
	}
	if _, _, to := f.post("/devices/dish/move", url.Values{"host": {"192.168.1.60"}, "ha_id": {moveLocalID + "-002"}}); to != "/devices/dish#move" {
		t.Fatalf("с выбором: %s", to)
	}
	if d := f.config().Devices["dish"]; d.HaID != moveLocalID+"-002" {
		t.Fatalf("настройки: %+v", d)
	}
}

func TestMoveChanges(t *testing.T) {
	opt := func(keys ...string) map[string]appliance.Option {
		m := map[string]appliance.Option{}
		for _, k := range keys {
			m[k] = appliance.Option{Key: k}
		}
		return m
	}
	same := appliance.Description{Type: "Oven", Programs: []string{"A.P1"}, Options: opt("A.O1")}
	if got := moveChanges(same, same); got != "Программы и параметры те же." {
		t.Errorf("те же: %q", got)
	}
	if got := moveChanges(same, appliance.Description{Type: "Oven"}); !strings.Contains(got, "загрузит из облака") {
		t.Errorf("описание не загружено: %q", got)
	}
	got := moveChanges(appliance.Description{Type: "Oven", Programs: []string{"A.P1", "A.P2"}, Options: opt("A.O1")},
		appliance.Description{Type: "Dishwasher", Programs: []string{"A.P2", "A.P3"}, Options: opt("A.O2")})
	for _, want := range []string{"Пропали программы: P1.", "Появились программы: P3.", "Пропали параметры: O1.", "Появились параметры: O2.", "вместо прежнего"} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q в %q", want, got)
		}
	}
	long := moveList("Пропали", []string{"a", "b", "c", "d", "e", "f", "g", "h"}, func(s string) string { return s })
	if long != "Пропали: a, b, c, d, e, f и ещё 2." {
		t.Errorf("длинный список: %q", long)
	}
}
