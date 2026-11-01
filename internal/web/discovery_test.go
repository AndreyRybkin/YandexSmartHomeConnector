package web

import (
	"context"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"hcgateway/internal/config"
	"hcgateway/internal/discovery"
)

const discoveryConfig = `devices:
  strip:
    name: Лента
    source: wled
    host: 192.168.1.40
  tv:
    name: Телевизор
    source: samsung-tv
    host: 192.168.1.80
    mac: AA:BB:CC:DD:EE:01
`

// Поиск в домашней сети: прибор с тем же адресом - «уже добавлен», новый - «Добавить» с
// заполненной формой, прибор с тем же MAC по другому адресу - «Обновить адрес».
func TestDiscovery(t *testing.T) {
	f := newFixture(t, discoveryConfig)
	f.writeUser(testPassword)
	f.login()
	gate := make(chan struct{})
	f.gw.Discovery = discovery.New(func(ctx context.Context, ip netip.Addr) []discovery.Found {
		<-gate
		switch ip.String() {
		case "192.168.1.40":
			return []discovery.Found{{Source: config.SourceWLED, Host: "192.168.1.40", Name: "Лента"}}
		case "192.168.1.57":
			return []discovery.Found{{Source: config.SourceWLED, Host: "192.168.1.57", Name: "Кухня", MAC: "AA:BB:CC:DD:EE:02"}}
		case "192.168.1.81":
			return []discovery.Found{{Source: config.SourceSamsungTV, Host: "192.168.1.81", Model: "UE43TU7100", MAC: "AA:BB:CC:DD:EE:01"}}
		}
		return nil
	})
	_, body := f.get("/connections")
	if !strings.Contains(body, `id="discovery"`) || !strings.Contains(body, `name="network" value="192.168.1.0/24"`) {
		t.Fatal("нет раздела поиска или сети по адресам приборов")
	}
	if _, _, to := f.post("/connections/discovery", url.Values{"network": {"8.8.8.0/24"}}); to != "/connections#discovery" {
		t.Fatalf("чужая сеть: переход %s", to)
	}
	if _, body = f.get("/connections"); !strings.Contains(body, "искать можно только в частной сети") {
		t.Fatal("чужая сеть не отвергнута")
	}
	if _, _, to := f.post("/connections/discovery", url.Values{"network": {"192.168.1.0/24"}}); to != "/connections#discovery" {
		t.Fatalf("поиск: переход %s", to)
	}
	if f.config().Discovery.Network != "192.168.1.0/24" {
		t.Fatal("сеть поиска не сохранена")
	}
	// Пока идёт поиск, страница обновляется сама по адресу с новой меткой: иначе браузер
	// счёл бы обновление переходом к разделу #discovery.
	if _, body = f.get("/connections"); !regexp.MustCompile(`<meta http-equiv="refresh" content="2; url=/connections\?r=[0-9]+#discovery">`).MatchString(body) ||
		!strings.Contains(body, "Идёт поиск в сети 192.168.1.0/24") {
		t.Fatal("страница поиска не обновляется сама")
	}
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for f.gw.Discovery.Status().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_, body = f.get("/connections")
	for _, want := range []string{"проверено 254 из 254 адресов", `уже добавлен: <a href="/devices/strip">Лента</a>`,
		`<a href="/?found=1#add">Добавить</a>`, `адрес изменился у <a href="/devices/tv">Телевизор</a>: был <code>192.168.1.80</code>`,
		"найдено: 3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("на странице нет %s", want)
		}
	}
	_, body = f.get("/?found=1")
	if !strings.Contains(body, `name="host" value="192.168.1.57"`) || !strings.Contains(body, `name="name" value="Кухня"`) ||
		!strings.Contains(body, `<option value="wled" selected>`) {
		t.Fatal("форма не заполнена находкой")
	}
	f.get("/connections")
	if _, _, to := f.post("/connections/discovery/move", url.Values{"found": {"2"}}); to != "/connections#discovery" {
		t.Fatalf("обновление адреса: переход %s", to)
	}
	if d := f.config().Devices["tv"]; d.Host != "192.168.1.81" {
		t.Fatalf("адрес телевизора: %s", d.Host)
	}
	f.get("/connections")
	if _, _, to := f.post("/connections/discovery/move", url.Values{"found": {"9"}}); to != "/connections#discovery" {
		t.Fatalf("чужая находка: переход %s", to)
	}
}
