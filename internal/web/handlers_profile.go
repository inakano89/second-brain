package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/profile"
)

type profileField struct {
	profile.Field
	Value   string
	Display string          // formatted for reading
	Checked map[string]bool // weekday checkboxes
}

type profileItemView struct {
	Item    profile.Item
	Kind    *profile.Kind
	Fields  []profileField // every field, for the edit form
	Shown   []profileField // filled fields, for reading
	Masked  bool           // sensitive: details load on demand
	Section string
	Stock   string // "~12 dias de estoque"
}

type profileKindView struct {
	Kind  *profile.Kind
	New   profileItemView // empty form
	Items []profileItemView
}

type profileView struct {
	Tab      string
	Sections []profile.Section
	Section  *profile.Section
	Counts   map[string]int
	O        *profile.Overview
	Kinds    []profileKindView
	Day      string
	Tomorrow string
	Access   string
	Local    bool
	AllEnc   bool
	Weekdays []string
}

var accessLabels = map[string]string{
	profile.AccessBasic: "Itens comuns liberados; sensíveis só para modelo local",
	profile.AccessFull:  "Tudo liberado, inclusive para modelos na nuvem",
	profile.AccessNone:  "A IA não lê o Perfil",
}

func fmtProfileValue(f profile.Field, v string) string {
	switch f.Type {
	case profile.FDate:
		if t, err := time.Parse("2006-01-02", v); err == nil {
			return t.Format("02/01/2006")
		}
	case profile.FWeekdays:
		return strings.ReplaceAll(v, ",", ", ")
	case profile.FDay:
		return "dia " + v
	}
	return v
}

func newItemView(it profile.Item) profileItemView {
	k := it.Def()
	v := profileItemView{Item: it, Kind: k, Section: k.Section, Masked: it.Sensitive && it.ID != 0}
	for _, f := range k.Fields {
		pf := profileField{Field: f, Value: it.Values[f.Key]}
		if f.Type == profile.FWeekdays {
			pf.Checked = map[string]bool{}
			for _, d := range strings.Split(pf.Value, ",") {
				pf.Checked[strings.TrimSpace(d)] = true
			}
		}
		if pf.Value != "" {
			pf.Display = fmtProfileValue(f, pf.Value)
			v.Shown = append(v.Shown, pf)
		}
		v.Fields = append(v.Fields, pf)
	}
	if left, ok := profile.DaysLeft(it); ok {
		v.Stock = fmt.Sprintf("~%d dias de estoque", int(left))
	}
	return v
}

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request) {
	store := s.Agent.Profile()
	tab := r.URL.Query().Get("tab")
	v := profileView{Tab: "overview", Sections: profile.Sections, Access: accessLabels[store.Access()], Weekdays: profile.Weekdays,
		Local: s.LLM.IsLocal("chat") && s.LLM.IsLocal("telegram"), AllEnc: store.EncryptAll()}
	for i := range profile.Sections {
		if profile.Sections[i].Key == tab {
			v.Tab, v.Section = tab, &profile.Sections[i]
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	now := time.Now().In(s.Cfg.Location())
	v.Day, v.Tomorrow = profile.DayKey(now), profile.DayKey(now.AddDate(0, 0, 1))
	if v.Section == nil { // the overview also reads the calendar
		o, err := s.Agent.ProfileOverview(ctx, now, 30)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		v.O, v.Counts = o, o.Counts
	} else {
		items, err := store.List(ctx, false)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		v.Counts = map[string]int{}
		byKind := map[string][]profileItemView{}
		for _, it := range items {
			v.Counts[it.Def().Section]++
			byKind[it.Kind] = append(byKind[it.Kind], newItemView(it))
		}
		for _, k := range profile.SectionKinds(v.Tab) {
			blank := newItemView(profile.Item{Kind: k.Key, Sensitive: k.Sensitive, Values: map[string]string{}})
			v.Kinds = append(v.Kinds, profileKindView{Kind: k, New: blank, Items: byKind[k.Key]})
		}
	}
	title := "Perfil"
	if v.Section != nil {
		title += " · " + v.Section.Label
	}
	s.render(w, "profile", s.page(r, title, "profile", v))
}

// profileItem reveals a (sensitive) item: details and edit form. Every reveal is logged.
func (s *Server) profileItem(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := s.Agent.Profile().Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if it.Sensitive {
		user, _ := s.currentUser(r)
		s.log.Info("dado sensível do perfil exibido", "item", id, "kind", it.Kind, "user", user, "ip", clientIP(r))
	}
	v := newItemView(it)
	v.Masked = false
	s.fragment(w, "profile_item", v)
}

func profileFromForm(r *http.Request, it *profile.Item) {
	it.Title = r.PostFormValue("title")
	it.Sensitive = r.PostFormValue("sensitive") == "on"
	it.Values = map[string]string{}
	for _, f := range it.Def().Fields {
		if f.Type == profile.FWeekdays {
			var days []string
			for _, d := range profile.Weekdays { // keep week order
				for _, sel := range r.PostForm[f.Key] {
					if sel == d {
						days = append(days, d)
					}
				}
			}
			if len(days) < 7 {
				it.Values[f.Key] = strings.Join(days, ",")
			}
			continue
		}
		it.Values[f.Key] = r.PostFormValue(f.Key)
	}
}

func sectionOf(kind string) string {
	if k := profile.KindOf(kind); k != nil {
		return k.Section
	}
	return ""
}

func (s *Server) profileSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/profile", err.Error(), true)
		return
	}
	store := s.Agent.Profile()
	it := profile.Item{Kind: r.PostFormValue("kind")}
	if id := r.PathValue("id"); id != "" {
		n, _ := strconv.ParseInt(id, 10, 64)
		cur, err := store.Get(r.Context(), n)
		if err != nil {
			redirectFlash(w, r, "/profile", "item não encontrado", true)
			return
		}
		it = cur
	}
	back := "/profile?tab=" + sectionOf(it.Kind)
	if profile.KindOf(it.Kind) == nil {
		redirectFlash(w, r, "/profile", "tipo inválido", true)
		return
	}
	profileFromForm(r, &it)
	if err := store.Save(r.Context(), &it); err != nil {
		redirectFlash(w, r, back, err.Error(), true)
		return
	}
	redirectFlash(w, r, back+fmt.Sprintf("#item-%d", it.ID), "Salvo: "+it.Title, false)
}

func (s *Server) profileDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	store := s.Agent.Profile()
	back := "/profile"
	if row, err := s.DB.GetProfile(r.Context(), id); err == nil {
		back += "?tab=" + sectionOf(row.Kind)
	}
	if err := store.Delete(r.Context(), id); err != nil {
		redirectFlash(w, r, back, err.Error(), true)
		return
	}
	redirectFlash(w, r, back, "Item apagado.", false)
}

// profileCheck marks (or unmarks) a dose, habit or class of a day.
func (s *Server) profileCheck(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	day, err := time.ParseInLocation("2006-01-02", r.FormValue("day"), s.Cfg.Location())
	if err != nil {
		redirectFlash(w, r, "/profile", "dia inválido", true)
		return
	}
	if _, _, err := s.Agent.Profile().Check(r.Context(), id, day, r.FormValue("slot"), r.FormValue("undo") == "1"); err != nil {
		redirectFlash(w, r, "/profile", err.Error(), true)
		return
	}
	http.Redirect(w, r, "/profile#today", http.StatusSeeOther)
}

// profileSuggestions lists calendar events that look like profile items (lazy: the
// calendar can be slow).
func (s *Server) profileSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	list, err := s.Agent.CalendarSuggestions(ctx, time.Now())
	data := map[string]any{"List": list}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.fragment(w, "profile_suggestions", data)
}

func (s *Server) profileImport(w http.ResponseWriter, r *http.Request) {
	loc := s.Cfg.Location()
	title := strings.TrimSpace(r.FormValue("title"))
	start, err := time.Parse(time.RFC3339, r.FormValue("start"))
	c, ok := profile.Classify(title)
	if err != nil || !ok {
		redirectFlash(w, r, "/profile?tab=datas", "evento inválido", true)
		return
	}
	sug := profile.Suggestion{Event: profile.Event{ID: r.FormValue("event_id"), Title: title, Start: start.In(loc), AllDay: r.FormValue("all_day") == "1"}, Category: c}
	it := sug.Item()
	store := s.Agent.Profile()
	if err := store.Save(r.Context(), &it); err != nil {
		redirectFlash(w, r, "/profile?tab=datas", err.Error(), true)
		return
	}
	_ = store.MarkImported(r.Context(), sug.Event.ID)
	redirectFlash(w, r, fmt.Sprintf("/profile?tab=%s#item-%d", sectionOf(it.Kind), it.ID), "Adicionado ao perfil: "+it.Title+". Complete os detalhes.", false)
}

func (s *Server) profileDismiss(w http.ResponseWriter, r *http.Request) {
	_ = s.Agent.Profile().MarkImported(r.Context(), r.FormValue("event_id"))
	if isHTMX(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/profile?tab=datas", http.StatusSeeOther)
}
