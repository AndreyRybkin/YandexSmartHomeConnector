package web

import (
	"net/url"
	"strings"
	"testing"

	"hcgateway/internal/smartthings/stfake"
)

func TestFlow(t *testing.T) {
	f := flow(true, false, false)
	if !f[0].Done || f[0].Open || !f[1].Open || f[2].Open {
		t.Fatalf("шаги: %+v", f)
	}
	if f := flow(true, true); f[0].Open || f[1].Open {
		t.Fatalf("все сделаны, а шаг раскрыт: %+v", f)
	}
	if done, note := added(2, 5); done || note != "в шлюзе 2 из 5" {
		t.Fatalf("пометка: %v %q", done, note)
	}
	if done, note := added(0, 0); done || note != "" {
		t.Fatal("пустой список сделан")
	}
}

// Карточки подключений с одной главной кнопкой и шаги подключения в разделах: раскрыт
// первый несделанный шаг, сделанный свёрнут и помечен «готово».
func TestConnectionCards(t *testing.T) {
	cloud := stfake.New()
	defer cloud.Close()
	f := newFixture(t, "smartthings:\n  api_url: "+cloud.URL()+"\n")
	f.writeUser(testPassword)
	f.login()
	_, body := f.get("/connections")
	for _, want := range []string{`<div class="conn-grid">`, `<article class="card warn">`, `<a class="button" href="#yandex">Настроить</a>`,
		`<details class="step" id="yandex-app" open>`, `<details class="step" id="yandex-link">`,
		`<details class="step" id="smartthings-app" open>`, `<span class="step-num">2</span>Вход в SmartThings`} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	f.post("/connections/smartthings", url.Values{"client_id": {stfake.ClientID}, "client_secret": {stfake.ClientSecret}})
	_, body = f.get("/connections")
	for _, want := range []string{`<details class="step done" id="smartthings-app">`, `<span class="chip run">готово</span><span class="muted">Изменить</span>`,
		`<details class="step" id="smartthings-login" open>`, `<form method="post" action="/connections/smartthings/login"><input type="hidden" name="csrf"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("после настройки SmartThings нет %s", want)
		}
	}
}
