package hccloud

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hcgateway/internal/hcapi"
)

// fakeCloud - облако с прибором good и прибором bad, которому ключей не выдают.
func fakeCloud(t *testing.T, appliances string) Region {
	t.Helper()
	const mapping = `<m><featureDescription><feature refUID="0201">BSH.Common.Status.DoorState</feature></featureDescription></m>`
	const device = `<device><statusList uid="0010"><status uid="0201" access="read"/></statusList></device>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/security/oauth/token":
			if r.FormValue("grant_type") == "refresh_token" {
				if r.FormValue("client_id") != appClientID || r.Form.Has("client_secret") {
					t.Errorf("продление: %v", r.Form)
				}
				w.Write([]byte(`{"access_token":"` + fakeJWT(`{"sub":"user-1","n":2}`) + `","refresh_token":"r2","expires_in":86400}`))
				return
			}
			if r.FormValue("code") != "CODE" || r.FormValue("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"access_token":"` + fakeJWT(`{"sub":"user-1"}`) + `","refresh_token":"r1","expires_in":86400}`))
		case r.Header.Get("Authorization") == "Bearer "+rejected:
			w.WriteHeader(http.StatusUnauthorized)
		case r.Header.Get("Authorization") == "":
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, "/paired-appliances"):
			w.Write([]byte(appliances))
		case r.URL.Path == "/api/appliance/v2/appliances/good/encryption-information":
			w.Write([]byte(`{"tls":{"key":"AAAA"}}`))
		case r.URL.Path == "/api/iddf/v1/iddf/good":
			var buf bytes.Buffer
			z := zip.NewWriter(&buf)
			for name, text := range map[string]string{"good_FeatureMapping.xml": mapping, "good_DeviceDescription.xml": device} {
				f, _ := z.Create(name)
				io.WriteString(f, text)
			}
			z.Close()
			w.Write(buf.Bytes())
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	return Region{Name: "cn", Base: srv.URL, Hosts: []string{srv.URL}}
}

func finished(t *testing.T, f *Flow) FlowStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := f.Status(); !st.Running {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("загрузка не закончилась")
	return FlowStatus{}
}

func redirectFor(t *testing.T, link, code string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return "hcauth://auth/prod?code=" + code + "&state=" + u.Query().Get("state")
}

// rejected - токен, который облако не принимает, хотя срок его не истёк.
var rejected = fakeJWT(`{"sub":"user-1","n":1}`)

func newFlow(t *testing.T, dir string, r Region) (*Flow, *hcapi.Client) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	api, err := hcapi.New(filepath.Join(dir, "tokens.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	return NewFlow(dir, api, nil, log, r), api
}

const twoAppliances = `{"appliances":[{"haId":"good","brand":"Bosch","haType":"Oven"},{"haId":"bad","brand":"Bosch","haType":"Hood"}]}`

func TestFlow(t *testing.T) {
	dir := t.TempDir()
	f, api := newFlow(t, dir, fakeCloud(t, twoAppliances))
	if err := f.Finish("hcauth://auth/prod?code=CODE&state=x"); err != ErrNoLogin {
		t.Fatalf("без входа: %v", err)
	}
	if err := f.Start("mars"); err != ErrRegion {
		t.Fatalf("чужой регион: %v", err)
	}
	if err := f.Start("cn"); err != nil {
		t.Fatal(err)
	}
	st := f.Status()
	if !st.Waiting || !strings.Contains(st.Link, "scope=Control+DeleteAppliance") {
		t.Fatalf("после начала входа: %+v", st)
	}
	// Чужой адрес не обрывает вход.
	if err := f.Finish("hcauth://auth/prod?code=CODE&state=x"); err != ErrBadRedirect {
		t.Fatalf("чужой state: %v", err)
	}
	if !f.Status().Waiting {
		t.Fatal("вход оборвался на чужом адресе")
	}
	if err := f.Finish(redirectFor(t, st.Link, "CODE")); err != nil {
		t.Fatal(err)
	}
	st = finished(t, f)
	if !st.Done || st.Failed || len(st.Results) != 2 || st.Results[0].Err != nil || st.Results[0].Mode != "tls" || st.Results[1].Err == nil {
		t.Fatalf("итог: %+v", st)
	}
	for _, name := range []string{"appliance.json", "description.json", "FeatureMapping.xml", "DeviceDescription.xml"} {
		if _, err := os.Stat(filepath.Join(dir, "appliances", "good", name)); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "appliances", "bad")); err == nil {
		t.Error("каталог прибора без ключей создан")
	}
	if err := f.Finish("hcauth://auth/prod?code=CODE&state=z"); err != ErrNoLogin {
		t.Fatalf("вход использован дважды: %v", err)
	}
	if tok, ok := api.AppTokens(); !ok || tok.Region != "cn" || tok.ClientID != appClientID {
		t.Fatalf("токен входа не сохранён: %+v %v", tok, ok)
	}
}

func TestFlowToken(t *testing.T) {
	dir := t.TempDir()
	f, api := newFlow(t, dir, fakeCloud(t, twoAppliances))
	jwt := fakeJWT(`{"sub":"user-1","exp":4102444800}`)
	if err := f.Update(); err != ErrNoTokens {
		t.Fatalf("без токена: %v", err)
	}
	if err := f.FinishToken("mars", jwt); err != ErrRegion {
		t.Fatalf("чужой регион: %v", err)
	}
	if err := f.FinishToken("cn", "{ access_token: '<token>' }"); err != ErrBadToken {
		t.Fatalf("заглушка: %v", err)
	}
	// Без refresh token токен работает один раз и не сохраняется.
	if err := f.FinishToken("cn", jwt); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); !st.NoRefresh || st.Failed || st.Results[0].Err != nil {
		t.Fatalf("итог: %+v", st)
	}
	if _, ok := api.AppTokens(); ok {
		t.Fatal("токен без refresh token сохранён")
	}
	if err := f.FinishToken("cn", "{ access_token: '"+jwt+"', refresh_token: 'r1', scope: 'ReadOrigApi Control' }"); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.NoRefresh || st.Failed || len(st.Results) != 2 || st.Results[0].Err != nil {
		t.Fatalf("итог: %+v", st)
	}
	tok, ok := api.AppTokens()
	if !ok || tok.Region != "cn" || tok.Scope != "ReadOrigApi Control" || tok.Access != "" || tok.Expires.Year() != 2100 {
		t.Fatalf("сведения о токене: %+v %v", tok, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "appliances", "good", "appliance.json")); err != nil {
		t.Error(err)
	}
	// Обновление ключей - без нового входа.
	if err := os.RemoveAll(filepath.Join(dir, "appliances")); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.Failed || st.Results[0].Err != nil {
		t.Fatalf("обновление: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "appliances", "good", "appliance.json")); err != nil {
		t.Error(err)
	}
}

// Сохранённый токен отозван раньше срока: шлюз продлевает его и повторяет запрос один раз.
func TestFlowRejectedToken(t *testing.T) {
	r := fakeCloud(t, twoAppliances)
	f, api := newFlow(t, t.TempDir(), r)
	if err := api.UseTokens(hcapi.Tokens{Access: rejected, Refresh: "r1", ClientID: appClientID, BaseURL: r.Base, Region: r.Name,
		Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.Failed || len(st.Results) != 2 {
		t.Fatalf("итог: %+v", st)
	}
	if tok, ok := api.AppTokens(); !ok || tok.Refreshed.IsZero() {
		t.Fatalf("токен не продлён: %+v", tok)
	}
}

// Токен, который облако не приняло, например из другого региона, не заменяет рабочий вход.
func TestFlowKeepsLoginOnBadToken(t *testing.T) {
	var refreshed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshed.Store(refreshed.Load() || r.URL.Path == "/security/oauth/token")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	dir := t.TempDir()
	f, api := newFlow(t, dir, Region{Name: "eu", Base: srv.URL, Hosts: []string{srv.URL}})
	if err := api.UseTokens(hcapi.Tokens{Access: "working", Refresh: "r0", ClientID: appClientID, BaseURL: "http://127.0.0.1:9", Region: "cn",
		Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	jwt := fakeJWT(`{"sub":"user-1","exp":4102444800}`)
	if err := f.FinishToken("eu", "{ access_token: '"+jwt+"', refresh_token: 'cn-refresh' }"); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); !st.Failed {
		t.Fatalf("итог: %+v", st)
	}
	tok, ok := api.AppTokens()
	if !ok || tok.Region != "cn" {
		t.Fatalf("прежний вход потерян: %+v %v", tok, ok)
	}
	if token, err := api.AppAccess(context.Background()); err != nil || token != "working" {
		t.Fatalf("токен заменён: %q %v", token, err)
	}
	if refreshed.Load() {
		t.Fatal("refresh token непроверенного токена ушёл в облако")
	}
}

// Новые файлы прибора заменяют старые целиком; старые остаются, если загрузка не удалась.
func TestDownloadReplacesDir(t *testing.T) {
	dir := t.TempDir()
	f, _ := newFlow(t, dir, fakeCloud(t, twoAppliances))
	good := filepath.Join(dir, "appliances", "good")
	if err := os.MkdirAll(good, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "stale.xml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "appliances", "bad")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "appliance.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "appliances", tmpPrefix+"left")
	fresh := filepath.Join(dir, "appliances", tmpPrefix+"fresh")
	for _, p := range []string{old, fresh} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-2 * leftoverAge)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	if err := f.FinishToken("cn", fakeJWT(`{"sub":"user-1"}`)); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.Failed || len(st.Results) != 2 || st.Results[1].Err == nil {
		t.Fatalf("итог: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(good, "stale.xml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("старый файл остался: %v", err)
	}
	if _, err := os.Stat(filepath.Join(good, "appliance.json")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(bad, "appliance.json")); err != nil {
		t.Errorf("файл прибора без ключей пропал: %v", err)
	}
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("брошенный каталог остался: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("свежий каталог другой загрузки удалён: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "appliances"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".old") || (strings.HasPrefix(e.Name(), tmpPrefix) && e.Name() != tmpPrefix+"fresh") {
			t.Errorf("остался временный каталог %s", e.Name())
		}
	}
}

// replaceDir возвращает старый каталог, если новый не встал на место.
func TestReplaceDirRestores(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceDir(filepath.Join(root, "нет-такого"), dir); err == nil {
		t.Fatal("нет ошибки")
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Fatalf("старый каталог не вернулся: %v", err)
	}
}

// Остановка шлюза обрывает идущую загрузку и ждёт её конца; новых загрузок нет.
func TestFlowRun(t *testing.T) {
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	f, _ := newFlow(t, t.TempDir(), Region{Name: "cn", Base: srv.URL, Hosts: []string{srv.URL}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	if err := f.FinishToken("cn", fakeJWT(`{"sub":"user-1"}`)); err != nil {
		t.Fatal(err)
	}
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("загрузка не оборвалась")
	}
	if st := f.Status(); st.Running || !st.Failed {
		t.Fatalf("итог: %+v", st)
	}
	if err := f.Start("cn"); err != ErrBusy {
		t.Fatalf("после остановки: %v", err)
	}
}

// Приложение и регион токена проверяются до запросов к облаку: такой токен туда не уходит.
func TestFlowTokenChecks(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	api, err := hcapi.New(filepath.Join(dir, "tokens.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	region := func(name string) Region { return Region{Name: name, Base: srv.URL, Hosts: []string{srv.URL}} }
	f := NewFlow(dir, api, nil, log, region("eu"), region("na"), region("cn"))
	token := func(claims string) string { return fakeJWT(`{"sub":"user-1","exp":4102444800` + claims + `}`) }
	app := `,"azp":"` + appClientID + `"`

	if err := f.FinishToken("cn", token(`,"azp":"portal-app"`)); !errors.Is(err, ErrForeignToken) {
		t.Errorf("чужое приложение: %v", err)
	}
	var mismatch *RegionMismatchError
	if err := f.FinishToken("eu", token(app+`,"region":"CN"`)); !errors.As(err, &mismatch) || mismatch.Token != "cn" || mismatch.Selected != "eu" {
		t.Errorf("чужой регион: %v", err)
	}
	for name, claims := range map[string]string{"нет региона": app, "незнакомый регион": app + `,"region":"XX"`} {
		if err := f.FinishToken("", token(claims)); !errors.Is(err, ErrTokenRegion) {
			t.Errorf("%s без выбора: %v", name, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("отвергнутый токен ушёл в облако: %d запросов", n)
	}

	// Регион из токена, без учёта регистра.
	if err := f.FinishToken("", token(app+`,"region":"cn"`)); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.Region != "cn" || st.UnknownRegion != "" {
		t.Errorf("регион по токену: %+v", st)
	}
	// Тот же регион выбран явно.
	if err := f.FinishToken("cn", token(app+`,"region":"CN"`)); err != nil {
		t.Fatal(err)
	}
	finished(t, f)
	// Незнакомый регион при явном выборе не повод отказать, но страница о нём скажет.
	if err := f.FinishToken("na", token(app+`,"region":"RNA"`)); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.Region != "na" || st.UnknownRegion != "RNA" {
		t.Errorf("незнакомый регион: %+v", st)
	}
	// Странное значение на страницу не попадает.
	if err := f.FinishToken("na", token(app+`,"region":"<b>x</b>"`)); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.UnknownRegion != "?" {
		t.Errorf("странное значение: %+v", st)
	}
	// Токен без полей azp и region при явном выборе проходит: шлюз не может о нём судить.
	if err := f.FinishToken("eu", token("")); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); st.Region != "eu" || st.UnknownRegion != "" {
		t.Errorf("без полей: %+v", st)
	}
}

func TestFlowFailures(t *testing.T) {
	region := fakeCloud(t, `{"appliances":[]}`)
	dir := t.TempDir()
	f, _ := newFlow(t, dir, region)
	f.Start("cn")
	if err := f.Finish(redirectFor(t, f.Status().Link, "CODE")); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); !st.Failed || !st.Empty {
		t.Fatalf("пустой аккаунт: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "paired-appliances.json")); err != nil {
		t.Error(err)
	}
	f.Start("cn")
	if err := f.Finish(redirectFor(t, f.Status().Link, "WRONG")); err != nil {
		t.Fatal(err)
	}
	if st := finished(t, f); !st.Failed || st.Empty {
		t.Fatalf("неверный код: %+v", st)
	}
	f.Start("cn")
	f.now = func() time.Time { return time.Now().Add(flowLoginTTL + time.Second) }
	if st := f.Status(); st.Waiting || !st.Expired || st.Link != "" {
		t.Fatalf("срок входа: %+v", st)
	}
}
