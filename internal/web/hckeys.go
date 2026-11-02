package web

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"hcgateway/internal/config"
	"hcgateway/internal/hcapi"
	"hcgateway/internal/hccloud"
)

// Загрузка ключей приборов Home Connect на странице «Подключения»: по данным токена или по
// ссылке на вход.

// keysRefresh - как часто страница обновляется сама, пока идёт загрузка ключей, в секундах.
const keysRefresh = 2

// keysSection - раздел страницы, куда возвращаются формы загрузки ключей.
const keysSection = "/connections#local"

type regionOption struct {
	Name, Label string
	Selected    bool
}

type keyResult struct {
	Brand, HaType, HaID string
	OK                  bool
}

type keysView struct {
	// Regions - регионы для входа по ссылке; TokenRegions - для токена: первый пункт - «по токену».
	Regions, TokenRegions []regionOption
	// UnknownRegion - незнакомый регион из токена: шлюз использовал выбранный.
	UnknownRegion string
	// Failed - загрузка оборвалась; Empty - в аккаунте нет приборов; Expired - шлюз не дождался адреса.
	Waiting, Running, Done, Failed, Empty, Expired, NoRefresh bool
	// Rejected - облако отвергло сохранённый токен: нужен новый вход или новый токен.
	Rejected bool
	Link     string
	// Saved - сохранён токен приложения Home Connect.
	Saved   *savedToken
	Results []keyResult
}

// Active - загрузка требует внимания: ждёт адрес, идёт или закончилась с ошибкой.
func (v keysView) Active() bool {
	return v.Waiting || v.Running || v.Failed || v.Expired || v.Rejected ||
		slices.ContainsFunc(v.Results, func(r keyResult) bool { return !r.OK })
}

// savedToken - сведения о сохранённом токене, без его значений. Время - сколько прошло и сколько
// осталось, а не часы: часовой пояс контейнера может быть не домашним.
type savedToken struct {
	Region string
	// Expired - срок истёк; Left - сколько срока осталось; Refreshed - когда токен получен или продлён.
	Expired         bool
	Left, Refreshed string
	// RefreshFailed - когда не удалось последнее продление; NextRefresh - через сколько шлюз повторит.
	RefreshFailed, NextRefresh string
}

// regionLabels - регионы в форме в порядке списка.
var regionLabels = []regionOption{{Name: hccloud.EU.Name, Label: "Европа"}, {Name: hccloud.NA.Name, Label: "Северная Америка"},
	{Name: hccloud.China.Name, Label: "Китай"}}

func regionLabel(name string) string {
	for _, r := range regionLabels {
		if r.Name == name {
			return r.Label
		}
	}
	return name
}

// agoText - сколько времени назад: «5 мин назад», «только что».
func agoText(d time.Duration) string {
	if t := strings.TrimSpace(ago(d)); t != "" {
		return t + " назад"
	}
	return "только что"
}

// leftText - сколько осталось: «20 ч», «меньше минуты».
func leftText(d time.Duration) string {
	if t := strings.TrimSpace(ago(d)); t != "" {
		return t
	}
	return "меньше минуты"
}

// savedView - сведения о сохранённом токене для страницы.
func (s *Server) savedView() *savedToken {
	api := s.gw.Cloud.API
	t, ok := api.AppTokens()
	if !ok {
		return nil
	}
	v := &savedToken{Region: regionLabel(t.Region), Expired: !time.Now().Before(t.Expires), Left: leftText(time.Until(t.Expires))}
	if !t.Refreshed.IsZero() {
		v.Refreshed = agoText(time.Since(t.Refreshed))
	}
	if st := api.RefreshState(); !st.Failed.IsZero() {
		v.RefreshFailed = agoText(time.Since(st.Failed))
		if st.Next.After(time.Now()) {
			v.NextRefresh = leftText(time.Until(st.Next))
		}
	}
	return v
}

// suffixRe - суффикс номера прибора в haId API приборов: в API аккаунта и в каталоге ключей его нет.
var suffixRe = regexp.MustCompile(`-[0-9]{3}$`)

// twinDevice ищет прибор шлюза с тем же haId до суффикса, но с другим источником: похоже, это та же
// техника, добавленная второй раз. Правило выведено из одного прибора, поэтому только предупреждение.
func twinDevice(cfg *config.Config, haID, source string) string {
	base := baseHaID(haID)
	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		if d := cfg.Devices[id]; d.Source != source && hcSource(d.Source) && baseHaID(d.HaID) == base {
			return id
		}
	}
	return ""
}

func (s *Server) keysView() keysView {
	st := s.gw.Keys.Status()
	v := keysView{Waiting: st.Waiting, Running: st.Running, Done: st.Done, Failed: st.Failed, Empty: st.Empty,
		Expired: st.Expired, NoRefresh: st.NoRefresh, UnknownRegion: st.UnknownRegion, Link: st.Link, Saved: s.savedView(),
		Rejected:     s.gw.Cloud.API.RefreshState().Rejected,
		TokenRegions: append([]regionOption{{Label: "Определить по токену"}}, regionLabels...)}
	region := st.Region
	if t, ok := s.gw.Cloud.API.AppTokens(); ok && region == "" {
		region = t.Region
	}
	for _, r := range regionLabels {
		r.Selected = r.Name == region
		v.Regions = append(v.Regions, r)
	}
	for _, r := range st.Results {
		a := r.Appliance
		v.Results = append(v.Results, keyResult{Brand: a.Brand, HaType: a.HaType, HaID: a.HaID, OK: r.Err == nil})
	}
	return v
}

func (s *Server) keysLogin(w http.ResponseWriter, r *http.Request, sess *session) {
	switch err := s.gw.Keys.Start(r.PostForm.Get("region")); {
	case err == nil:
		s.done(w, r, sess, keysSection, "Ссылка на вход готова.")
	case s.keysFailed(w, r, sess, err):
	default:
		s.log.Warn("вход для загрузки ключей не начат", "err", err)
		s.failed(w, r, sess, keysSection, "Не удалось начать вход, подробности в журнале шлюза.")
	}
}

func (s *Server) keysFinish(w http.ResponseWriter, r *http.Request, sess *session) {
	switch err := s.gw.Keys.Finish(r.PostForm.Get("redirect")); {
	case err == nil:
		s.log.Info("загрузка ключей начата")
		s.done(w, r, sess, keysSection, "Вход принят, шлюз загружает ключи.")
	case s.keysFailed(w, r, sess, err):
	default:
		s.failed(w, r, sess, keysSection, "Адрес не подошёл: нужен адрес hcauth:// из последней ссылки.")
	}
}

func (s *Server) keysToken(w http.ResponseWriter, r *http.Request, sess *session) {
	var mismatch *hccloud.RegionMismatchError
	switch err := s.gw.Keys.FinishToken(r.PostForm.Get("region"), r.PostForm.Get("token")); {
	case err == nil:
		s.log.Info("загрузка ключей по токену начата")
		s.done(w, r, sess, keysSection, "Токен принят: регион "+regionLabel(s.gw.Keys.Status().Region)+", шлюз загружает ключи.")
	case s.keysFailed(w, r, sess, err):
	case errors.As(err, &mismatch):
		s.failed(w, r, sess, keysSection, "Токен выдан для региона «"+regionLabel(mismatch.Token)+"», а выбран «"+regionLabel(mismatch.Selected)+
			"». Вставьте токен ещё раз и выберите его регион или «Определить по токену».")
	case errors.Is(err, hccloud.ErrForeignToken):
		s.failed(w, r, sess, keysSection, "Токен выдан другому приложению: шлюзу нужен токен приложения Home Connect, другой он не сможет продлить.")
	case errors.Is(err, hccloud.ErrTokenRegion):
		s.failed(w, r, sess, keysSection, "Шлюз не смог определить регион по токену: выберите регион из списка и вставьте токен ещё раз.")
	case errors.Is(err, hccloud.ErrTokenExpired):
		s.failed(w, r, sess, keysSection, "Срок токена истёк: получите новый.")
	default:
		s.failed(w, r, sess, keysSection, "В данных нет токена: нужен JSON с полем access_token или сам токен.")
	}
}

// keysUpdate загружает ключи заново по сохранённому токену.
func (s *Server) keysUpdate(w http.ResponseWriter, r *http.Request, sess *session) {
	switch err := s.gw.Keys.Update(); {
	case err == nil:
		s.log.Info("загрузка ключей по сохранённому токену начата")
		s.done(w, r, sess, keysSection, "Шлюз загружает ключи.")
	case s.keysFailed(w, r, sess, err):
	default:
		s.failed(w, r, sess, keysSection, "Сохранённого токена нет: загрузите ключи по токену или по ссылке.")
	}
}

// keysRefreshNow продлевает сохранённый токен сейчас.
func (s *Server) keysRefreshNow(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	switch err := s.gw.Cloud.API.RefreshNow(ctx); {
	case err == nil:
		s.log.Info("токен Home Connect продлён из интерфейса")
		s.done(w, r, sess, keysSection, "Токен продлён.")
	case errors.Is(err, hcapi.ErrNotLoggedIn):
		s.failed(w, r, sess, keysSection, "Облако не продлило токен: нужен новый вход или новый токен.")
	default:
		s.log.Warn("токен Home Connect не продлён", "err", err)
		s.failed(w, r, sess, keysSection, "Токен не продлён, шлюз повторит позже. Подробности - в журнале шлюза.")
	}
}

// keysLogout удаляет токены и возвращает к разделу ключей.
func (s *Server) keysLogout(w http.ResponseWriter, r *http.Request, sess *session) {
	s.logoutHomeConnect(w, r, sess, keysSection)
}

// keysFailed отвечает на общие для форм отказы и сообщает, был ли отказ одним из них.
func (s *Server) keysFailed(w http.ResponseWriter, r *http.Request, sess *session, err error) bool {
	switch {
	case errors.Is(err, hccloud.ErrRegion):
		s.failed(w, r, sess, keysSection, "Неизвестный регион.")
	case errors.Is(err, hccloud.ErrBusy):
		s.failed(w, r, sess, keysSection, "Загрузка ключей уже идёт.")
	case errors.Is(err, hccloud.ErrNoLogin):
		s.failed(w, r, sess, keysSection, "Вход не начат или его срок истёк: получите новую ссылку.")
	default:
		return false
	}
	return true
}
