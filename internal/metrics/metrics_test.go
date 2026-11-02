package metrics

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fakeState struct{}

func (fakeState) Linked() bool { return true }
func (fakeState) Banned() int  { return 2 }
func (fakeState) TokenExpires() map[string]time.Time {
	return map[string]time.Time{"cloud": time.Now().Add(time.Hour)}
}
func (fakeState) Devices() []Device {
	return []Device{
		{ID: "oven", Source: "demo", Online: true, Operation: "Run"},
		{ID: "dish", Source: "demo", Online: false, Operation: "Ready"},
	}
}

func scrape(t *testing.T, h http.Handler, auth string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

func TestHandler(t *testing.T) {
	token := testToken
	h := Handler(func() string { return token })
	for name, auth := range map[string]string{"без заголовка": "", "чужой токен": "Bearer wrong", "не Bearer": "Basic " + testToken} {
		if status, body := scrape(t, h, auth); status != http.StatusUnauthorized || body != "" {
			t.Errorf("%s: %d %q", name, status, body)
		}
	}

	Watch(fakeState{})
	SetBuildInfo("test")
	Login(true, LoginWrong)
	TokenRefreshSource("cloud")
	TokenRefresh("cloud", RefreshOK)
	status, body := scrape(t, h, "Bearer "+testToken)
	if status != http.StatusOK {
		t.Fatalf("с токеном: %d", status)
	}
	for _, want := range []string{
		`hcgateway_build_info{version="test"} 1`,
		`hcgateway_yandex_linked 1`,
		`hcgateway_auth_banned 2`,
		`hcgateway_device_online{device="oven",source="demo"} 1`,
		`hcgateway_device_online{device="dish",source="demo"} 0`,
		`hcgateway_device_operation_state{device="oven",state="Run"} 1`,
		`hcgateway_linking_logins_total{result="banned"} 0`,
		`hcgateway_source_token_expires_seconds{source="cloud"} 3`,
		`hcgateway_source_token_refreshes_total{result="ok",source="cloud"} 1`,
		`hcgateway_source_token_refreshes_total{result="rejected",source="cloud"} 0`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("нет %s", want)
		}
	}
	// У прибора без связи состояния программы нет.
	if strings.Contains(body, `device="dish",state=`) {
		t.Error("состояние программы прибора без связи")
	}
	if Value("hcgateway_web_logins_total", "result", LoginWrong) < 1 {
		t.Error("Value не видит счётчик")
	}

	// Токен убрали на ходу - метрики больше не отдаются.
	token = ""
	if status, _ := scrape(t, h, "Bearer "); status != http.StatusUnauthorized {
		t.Fatalf("без токена в настройках: %d", status)
	}
}

func TestYandexHandler(t *testing.T) {
	h := YandexHandler("test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	before := Value("hcgateway_yandex_requests_total", "handler", "test", "code", "418")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := Value("hcgateway_yandex_requests_total", "handler", "test", "code", "418"); got != before+1 {
		t.Fatalf("запросы: %v", got)
	}
}

// names - имена своих метрик шлюза. Имя есть только в описании метрики: у счётчика
// с метками, который ещё не считал, рядов нет.
func names(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`fqName: "([^"]+)"`)
	out := map[string]bool{}
	for _, c := range own {
		ch := make(chan *prometheus.Desc, 16)
		c.Describe(ch)
		close(ch)
		for d := range ch {
			m := re.FindStringSubmatch(d.String())
			if m == nil {
				t.Fatalf("имя метрики не найдено в %s", d)
			}
			out[m[1]] = true
		}
	}
	return out
}

// TestDashboard проверяет, что каждое выражение дашборда ссылается на метрику шлюза:
// иначе после переименования метрики панель молча опустеет.
func TestDashboard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "grafana", "hcgateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dash any
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatal(err)
	}
	known := names(t)
	metricRe := regexp.MustCompile(`hcgateway_[a-z0-9_]+`)
	exprs := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if s, ok := v.(string); ok && (k == "expr" || k == "definition") {
					exprs++
					for _, name := range metricRe.FindAllString(s, -1) {
						base := name
						for _, suffix := range []string{"_bucket", "_sum", "_count"} {
							if b, ok := strings.CutSuffix(name, suffix); ok && known[b] {
								base = b
							}
						}
						if !known[base] {
							t.Errorf("выражение %q: метрики %s нет у шлюза", s, name)
						}
					}
					continue
				}
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(dash)
	if exprs < 10 {
		t.Fatalf("выражений в дашборде: %d", exprs)
	}
}
