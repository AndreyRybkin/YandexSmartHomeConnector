package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"hcgateway/internal/config"
	"hcgateway/internal/delonghi"
	"hcgateway/internal/profile"
)

// Кофемашины De'Longhi: вход в аккаунт и список машин на странице «Подключения»,
// поля и отпечаток сертификата на странице прибора.

const (
	// maxEmailLen и maxPasswordLen - пределы почты и пароля аккаунта De'Longhi.
	maxEmailLen    = 254
	maxPasswordLen = 256
)

// drinkLabels - напитки Алисы по-русски.
var drinkLabels = map[string]string{"espresso": "эспрессо", "double_espresso": "двойной эспрессо",
	"americano": "американо", "cappuccino": "капучино", "latte": "латте"}

// coffeeAccount - аккаунт De'Longhi на странице «Подключения».
type coffeeAccount struct {
	LoggedIn      bool
	Pool          string
	Expires       string
	LoginRequired bool
	Machines      []coffeeRow
}

// coffeeRow - машина аккаунта и прибор настроек с её серийным номером, если он есть.
type coffeeRow struct {
	delonghi.DeviceInfo
	Device string
}

func (s *Server) coffeeAccount(cfg *config.Config) coffeeAccount {
	st := s.gw.DeLonghi.Status()
	v := coffeeAccount{LoggedIn: st.LoggedIn, Pool: st.Pool, LoginRequired: st.LoginRequired}
	if !st.Expires.IsZero() {
		v.Expires = st.Expires.Local().Format(time.DateTime)
	}
	devices := map[string]string{}
	for id, d := range cfg.Devices {
		if d.Source == config.SourceDeLonghi {
			devices[d.Serial] = id
		}
	}
	for _, m := range s.gw.DeLonghi.Devices() {
		v.Machines = append(v.Machines, coffeeRow{DeviceInfo: m, Device: devices[m.Serial]})
	}
	return v
}

// coffeeLogin входит в аккаунт De'Longhi. Пароль не сохраняется и не пишется в журнал.
func (s *Server) coffeeLogin(w http.ResponseWriter, r *http.Request, sess *session) {
	const to = "/connections#delonghi"
	email, password := strings.TrimSpace(r.PostForm.Get("email")), r.PostForm.Get("password")
	if email == "" || password == "" || len(email) > maxEmailLen || len(password) > maxPasswordLen ||
		!utf8.ValidString(email) || !utf8.ValidString(password) {
		s.failed(w, r, sess, to, "Нужны почта и пароль аккаунта De'Longhi.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.gw.DeLonghi.Login(ctx, email, password); err != nil {
		switch {
		case errors.Is(err, delonghi.ErrBadCredentials):
			s.failed(w, r, sess, to, "De'Longhi не принял почту или пароль.")
		case errors.Is(err, delonghi.ErrNoAccount):
			s.failed(w, r, sess, to, "Аккаунт De'Longhi с такой почтой не найден.")
		case errors.Is(err, delonghi.ErrRateLimited):
			s.failed(w, r, sess, to, "De'Longhi просит повторить вход позже.")
		default:
			s.log.Warn("вход в De'Longhi не выполнен", "err", err)
			s.failed(w, r, sess, to, "Вход в De'Longhi не выполнен, подробности в журнале шлюза.")
		}
		return
	}
	if err := s.gw.DeLonghi.RefreshDevices(ctx); err != nil {
		s.log.Warn("список кофемашин не загружен", "err", err)
	}
	s.done(w, r, sess, to, "Вход в De'Longhi выполнен.")
}

func (s *Server) coffeeLogout(w http.ResponseWriter, r *http.Request, sess *session) {
	if err := s.gw.DeLonghi.Logout(); err != nil {
		s.log.Error("выход из De'Longhi", "err", err)
		s.failed(w, r, sess, "/connections#delonghi", "Файл сессии De'Longhi не удалён, подробности в журнале шлюза.")
		return
	}
	s.done(w, r, sess, "/connections#delonghi", "Выход из De'Longhi выполнен.")
}

func (s *Server) coffeeDevices(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.gw.DeLonghi.RefreshDevices(ctx); err != nil {
		s.log.Warn("список кофемашин не загружен", "err", err)
		s.failed(w, r, sess, "/connections#delonghi", "Список кофемашин не загружен, подробности в журнале шлюза.")
		return
	}
	s.done(w, r, sess, "/connections#delonghi", "Список кофемашин обновлён.")
}

// coffeeView - поля кофемашины на странице прибора.
type coffeeView struct {
	ProfileID string
	Recipes   []recipeRow
	Drinks    []choice
	// Pin - подтверждённый отпечаток; Observed - снятый шлюзом и ещё не подтверждённый.
	Pin      string
	Observed string
	// Stats - рецепты машины из облака с числом варок и напитком Алисы, если сопоставлен.
	Stats []statRow
}

type statRow struct {
	ID, Brew int
	Drink    string
}

type recipeRow struct {
	Drink, Label, Value string
}

func (s *Server) coffeeView(id string, d config.Device) *coffeeView {
	v := &coffeeView{ProfileID: "1", Pin: groups(d.Pin)}
	if d.ProfileID != nil {
		v.ProfileID = strconv.Itoa(*d.ProfileID)
	}
	recipes := d.Recipes
	if len(recipes) == 0 {
		recipes = profile.DeLonghiRecipes
	}
	for _, drink := range config.CoffeeDrinks {
		row := recipeRow{Drink: drink, Label: drinkLabels[drink]}
		if n, ok := recipes[drink]; ok {
			row.Value = strconv.Itoa(n)
		}
		v.Recipes = append(v.Recipes, row)
	}
	byNumber := map[int]string{}
	for name, n := range recipes {
		byNumber[n] = drinkLabels[name]
	}
	s.statsMu.Lock()
	for _, st := range s.stats[id] {
		v.Stats = append(v.Stats, statRow{ID: st.ID, Brew: st.Brew, Drink: byNumber[st.ID]})
	}
	s.statsMu.Unlock()
	drink := d.DefaultDrink
	if drink == "" {
		drink = profile.DeLonghiDrink
	}
	v.Drinks = plain(config.CoffeeDrinks, drink, "", drinkLabels)
	if hc, ok := s.gw.HomeConnect(id); ok {
		if ap, ok := hc.Appliance.(*delonghi.Appliance); ok && d.Pin == "" {
			v.Observed = ap.Observed()
		}
	}
	return v
}

// groups - отпечаток по два знака через двоеточие, как его показывает браузер.
func groups(hex string) string {
	var parts []string
	for i := 0; i+2 <= len(hex); i += 2 {
		parts = append(parts, hex[i:i+2])
	}
	return strings.Join(parts, ":")
}

// coffeeForm переносит поля кофемашины из формы. Пустой рецепт - напитка нет в Алисе.
func coffeeForm(d *config.Device, f url.Values) error {
	d.Host, d.Serial = strings.TrimSpace(f.Get("host")), strings.TrimSpace(f.Get("serial"))
	if s := strings.TrimSpace(f.Get("profile_id")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return invalid("Профиль пользователя машины: нужно число")
		}
		d.ProfileID = &n
	} else {
		d.ProfileID = nil
	}
	recipes := map[string]int{}
	for _, drink := range config.CoffeeDrinks {
		s := strings.TrimSpace(f.Get("recipe." + drink))
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return invalid("Рецепт для %s: нужно число", drinkLabels[drink])
		}
		recipes[drink] = n
	}
	if len(recipes) == 0 {
		return invalid("Нужен номер рецепта хотя бы для одного напитка")
	}
	d.Recipes = recipes
	d.DefaultDrink = f.Get("default_drink")
	if _, ok := recipes[d.DefaultDrink]; !ok {
		return invalid("Напиток для «включи» должен быть среди напитков с рецептом")
	}
	return nil
}

// confirmPin подтверждает отпечаток сертификата, который снял шлюз: после этого шлюз
// отправляет машине токен аккаунта. Подтверждается только тот отпечаток, что показан на странице.
func (s *Server) confirmPin(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	to := "/devices/" + id
	pin := strings.ToUpper(strings.ReplaceAll(r.PostForm.Get("pin"), ":", ""))
	hc, ok := s.gw.HomeConnect(id)
	ap, isCoffee := (*delonghi.Appliance)(nil), false
	if ok {
		ap, isCoffee = hc.Appliance.(*delonghi.Appliance)
	}
	if !isCoffee || pin == "" || ap.Observed() != pin {
		s.failed(w, r, sess, to, "Отпечаток изменился или ещё не снят: откройте страницу заново и сверьте его.")
		return
	}
	err := s.change(r.PostForm.Get("version"), "devices."+id+".pin", func(c *config.Config) error {
		d, ok := c.Devices[id]
		if !ok || d.Source != config.SourceDeLonghi {
			return invalid("Кофемашины %s нет в настройках", id)
		}
		d.Pin = pin
		c.Devices[id] = d
		return nil
	})
	if err != nil {
		s.failed(w, r, sess, to, s.explain("devices."+id, err))
		return
	}
	s.done(w, r, sess, to, "Отпечаток подтверждён: шлюз подключается к кофемашине.")
}

// coffeeChoices - кофемашины аккаунта, которых нет в настройках, и текущая cur.
func (s *Server) coffeeChoices(cfg *config.Config, cur string) []choice {
	used := map[string]bool{}
	for _, d := range cfg.Devices {
		if d.Source == config.SourceDeLonghi && d.Serial != cur {
			used[d.Serial] = true
		}
	}
	var list []string
	labels := map[string]string{}
	for _, m := range s.gw.DeLonghi.Devices() {
		if !used[m.Serial] {
			list = append(list, m.Serial)
			labels[m.Serial] = "Кофемашина " + m.Model + ", " + m.Serial
		}
	}
	if len(list) == 0 {
		return nil
	}
	return choices(list, cur, blankIf(cur == "", "-"), labels)
}

// loadRecipes читает из облака номера рецептов машины и сколько раз каждый сварили: по ним
// владелец находит номер напитка. Список живёт в памяти до перезапуска шлюза.
func (s *Server) loadRecipes(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	to := "/devices/" + id + "#coffee"
	cfg, _ := s.file.Current()
	dc, ok := cfg.Devices[id]
	if !ok || dc.Source != config.SourceDeLonghi {
		s.notFound(w, sess)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stats, err := s.gw.DeLonghi.RecipeStats(ctx, dc.Serial)
	if err != nil {
		s.log.Warn("рецепты кофемашины не загружены", "device", id, "err", err)
		s.failed(w, r, sess, to, "Рецепты не загружены из облака, подробности в журнале шлюза.")
		return
	}
	s.statsMu.Lock()
	if s.stats == nil {
		s.stats = map[string][]delonghi.RecipeStat{}
	}
	s.stats[id] = stats
	s.statsMu.Unlock()
	s.done(w, r, sess, to, "Рецепты загружены из облака.")
}
