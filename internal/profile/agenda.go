package profile

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Slot is something scheduled for a given day: a dose, a class, a habit.
type Slot struct {
	ItemID int64
	Kind   string
	Icon   string
	Time   string // HH:MM or "" (any time of the day)
	Title  string
	Detail string
	Done   bool
	Check  bool // can be checked off (doses and habits)
}

// Key identifies the slot's check-in.
func (s Slot) Key(day time.Time) string { return checkKey(s.ItemID, DayKey(day), s.Time) }

// Alert levels.
const (
	LevelInfo = "info"
	LevelWarn = "warn"
	LevelLate = "late"
)

// Alert is an upcoming date, due item or warning.
type Alert struct {
	Date    time.Time
	Days    int // days from today (negative = overdue)
	Icon    string
	Label   string
	Title   string
	ItemID  int64  // profile item, when it comes from one
	NodeID  int64  // person node, for birthdays from contacts
	Source  string // perfil, contatos, agenda
	Level   string
	Section string // profile section of the item
}

// When returns a friendly relative day.
func (a Alert) When() string {
	switch {
	case a.Days < -1:
		return fmt.Sprintf("há %d dias", -a.Days)
	case a.Days == -1:
		return "ontem"
	case a.Days == 0:
		return "hoje"
	case a.Days == 1:
		return "amanhã"
	case a.Days < 7:
		return fmt.Sprintf("em %d dias (%s)", a.Days, weekdayPT[a.Date.Weekday()])
	}
	return fmt.Sprintf("em %d dias (%s)", a.Days, a.Date.Format("02/01"))
}

var weekdayPT = []string{"domingo", "segunda", "terça", "quarta", "quinta", "sexta", "sábado"}

// Person is a contact with a birthday ("MM-DD" or "YYYY-MM-DD").
type Person struct {
	ID       int64
	Name     string
	Birthday string
}

// Event is a calendar event.
type Event struct {
	ID     string
	Title  string
	Start  time.Time
	AllDay bool
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func daysBetween(from, to time.Time) int {
	return int(math.Round(midnight(to).Sub(midnight(from)).Hours() / 24))
}

// within reports whether the item is active on day (start/end fields).
func active(it Item, day time.Time) bool {
	loc := day.Location()
	if s, ok := it.Date("start", loc); ok && day.Before(s) {
		return false
	}
	if e, ok := it.Date("end", loc); ok && midnight(day).After(e) {
		return false
	}
	return true
}

// Today lists what is scheduled for day, sorted by time.
func Today(items []Item, day time.Time, done map[string]bool) []Slot {
	var out []Slot
	add := func(s Slot) {
		s.Done = done[s.Key(day)]
		out = append(out, s)
	}
	for _, it := range items {
		if it.Archived || it.Locked || !it.OnWeekday(day.Weekday()) || !active(it, day) {
			continue
		}
		d := it.Def()
		switch it.Kind {
		case "medication", "supplement", "habit":
			detail := it.Get("dose")
			if it.Kind == "habit" {
				detail = it.Get("target")
			}
			times := it.Times("times")
			if len(times) == 0 {
				times = []string{""}
			}
			for _, t := range times {
				add(Slot{ItemID: it.ID, Kind: it.Kind, Icon: d.Icon, Time: t, Title: it.Title, Detail: detail, Check: true})
			}
		case "enrollment", "course":
			if it.Get("weekdays") == "" && it.Get("time") == "" {
				continue // no schedule: not a daily item
			}
			detail := it.Get("activity")
			if it.Kind == "course" {
				detail = it.Get("provider")
			}
			if u := it.Get("until"); u != "" && it.Get("time") != "" {
				detail = strings.TrimSpace(detail + " · até " + u)
			}
			add(Slot{ItemID: it.ID, Kind: it.Kind, Icon: d.Icon, Time: it.Get("time"), Title: it.Title, Detail: detail})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Time == "") != (out[j].Time == "") {
			return out[i].Time != "" // timed first
		}
		return out[i].Time < out[j].Time
	})
	return out
}

// noLate are date fields that describe an event, not something to renew: once past they
// simply leave the list.
var noLate = map[string]bool{"surgery.date": true, "trip.start": true, "course.start": true, "course.end": true, "medication.end": true}

// nextYearly returns the next occurrence (today included) of a month/day.
func nextYearly(month time.Month, day int, today time.Time) time.Time {
	t := time.Date(today.Year(), month, day, 0, 0, 0, 0, today.Location())
	if t.Before(today) {
		t = t.AddDate(1, 0, 0)
	}
	return t
}

// nextMonthly returns the next occurrence of a day of the month (clamped to month length).
func nextMonthly(dom int, today time.Time) time.Time {
	for i := 0; i < 2; i++ {
		first := time.Date(today.Year(), today.Month()+time.Month(i), 1, 0, 0, 0, 0, today.Location())
		last := first.AddDate(0, 1, -1).Day()
		t := first.AddDate(0, 0, min(dom, last)-1)
		if !t.Before(today) {
			return t
		}
	}
	return today
}

// DaysLeft estimates for how many days the stock of a medication or supplement lasts.
func DaysLeft(it Item) (float64, bool) {
	stock, ok := it.Num("stock")
	if !ok {
		return 0, false
	}
	per, ok := it.Num("per_dose")
	if !ok || per <= 0 {
		per = 1
	}
	doses := float64(max(1, len(it.Times("times"))))
	week := 7.0
	if w := it.Get("weekdays"); w != "" {
		week = float64(len(strings.Split(w, ",")))
	}
	daily := per * doses * week / 7
	return stock / daily, true
}

// Upcoming lists dates, renewals and warnings of the profile from `horizon` days ahead
// (overdue items up to 30 days back).
func Upcoming(items []Item, now time.Time, horizon int) []Alert {
	today := midnight(now)
	var out []Alert
	add := func(a Alert) {
		a.Date = midnight(a.Date)
		a.Days = daysBetween(today, a.Date)
		if a.Source == "" {
			a.Source = "perfil"
		}
		if a.Level == "" {
			a.Level = LevelInfo
			if a.Days < 0 {
				a.Level = LevelLate
			} else if a.Days <= 3 {
				a.Level = LevelWarn
			}
		}
		out = append(out, a)
	}
	for _, it := range items {
		if it.Archived || it.Locked {
			continue
		}
		def := it.Def()
		first := len(out)
		upcomingItem(it, def, today, now, horizon, add)
		for i := first; i < len(out); i++ {
			out[i].Section = def.Section
		}
	}
	return out
}

func upcomingItem(it Item, def *Kind, today, now time.Time, horizon int, add func(Alert)) {
	inRange := func(d int, h int) bool { return d >= -30 && d <= h }
	loc := now.Location()
	// Explicit date fields with an alert label.
	for _, f := range def.Fields {
		if f.Type != FDate || f.Alert == "" {
			continue
		}
		t, ok := it.Date(f.Key, loc)
		if !ok {
			continue
		}
		d := daysBetween(today, t)
		if d < 0 && noLate[it.Kind+"."+f.Key] {
			continue
		}
		if inRange(d, horizon) {
			add(Alert{Date: t, Icon: def.Icon, Label: f.Alert, Title: it.Title, ItemID: it.ID})
		}
	}
	switch it.Kind {
	case "date":
		t, ok := it.Date("date", loc)
		if !ok {
			return
		}
		h := horizon
		if r, ok := it.Num("remind"); ok && int(r) > h {
			h = int(r)
		}
		label := it.Get("category")
		if label == "" {
			label = "Data importante"
		}
		if it.Get("yearly") == "não" {
			if d := daysBetween(today, t); d >= 0 && d <= h {
				add(Alert{Date: t, Icon: def.Icon, Label: capitalize(label), Title: it.Title, ItemID: it.ID})
			}
			return
		}
		next := nextYearly(t.Month(), t.Day(), today)
		if d := daysBetween(today, next); d <= h {
			title := it.Title
			if n := next.Year() - t.Year(); n > 0 {
				title += fmt.Sprintf(" (%d anos)", n)
			}
			add(Alert{Date: next, Icon: def.Icon, Label: capitalize(label), Title: title, ItemID: it.ID})
		}
	case "identity", "pet":
		if t, ok := it.Date("birth", loc); ok {
			next := nextYearly(t.Month(), t.Day(), today)
			if daysBetween(today, next) <= horizon {
				label := "Seu aniversário"
				if it.Kind == "pet" {
					label = "Aniversário do pet"
				}
				add(Alert{Date: next, Icon: "🎂", Label: label, Title: fmt.Sprintf("%s (%d anos)", it.Title, next.Year()-t.Year()), ItemID: it.ID})
			}
		}
	case "doctor", "home":
		last, ok1 := it.Date("last", loc)
		every, ok2 := it.Num("every")
		if !ok1 || !ok2 || every <= 0 || it.Get("next") != "" {
			return
		}
		due := last.AddDate(0, int(every), 0)
		if d := daysBetween(today, due); d <= horizon {
			label := "Consulta de rotina"
			if it.Kind == "home" {
				label = "Manutenção"
			}
			if d < 0 {
				label += " atrasada"
			}
			a := Alert{Date: due, Icon: def.Icon, Label: label, Title: it.Title, ItemID: it.ID}
			if d < -30 { // keep showing long-overdue routine items, pinned to today
				a.Date, a.Level = today, LevelLate
			}
			add(a)
		}
	case "medication", "supplement":
		if days, ok := DaysLeft(it); ok && days <= 10 && active(it, today) {
			a := Alert{Date: today.AddDate(0, 0, int(days)), Icon: def.Icon, Title: it.Title, ItemID: it.ID, Level: LevelWarn}
			a.Label = fmt.Sprintf("Estoque para ~%d dias: comprar", int(days))
			if days < 1 {
				a.Label, a.Level = "Sem estoque: comprar", LevelLate
			}
			add(a)
		}
	}
	// Monthly payments show up a few days before.
	if dom, ok := it.Num("due_day"); ok && dom >= 1 {
		next := nextMonthly(int(dom), today)
		if daysBetween(today, next) <= min(5, horizon) {
			label := "Pagamento"
			if v := firstNonEmpty(it.Get("fee"), it.Get("price"), it.Get("amount")); v != "" {
				label += " R$ " + v
			}
			add(Alert{Date: next, Icon: "💳", Label: label, Title: it.Title, ItemID: it.ID})
		}
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// Birthdays lists contact birthdays in the horizon.
func Birthdays(people []Person, now time.Time, horizon int) []Alert {
	today := midnight(now)
	var out []Alert
	for _, p := range people {
		b := p.Birthday
		year := 0
		if len(b) == 10 {
			year, _ = strconv.Atoi(b[:4])
			b = b[5:]
		}
		m, d, ok := strings.Cut(b, "-")
		mm, err1 := strconv.Atoi(m)
		dd, err2 := strconv.Atoi(d)
		if !ok || err1 != nil || err2 != nil || mm < 1 || mm > 12 || dd < 1 || dd > 31 {
			continue
		}
		next := nextYearly(time.Month(mm), dd, today)
		days := daysBetween(today, next)
		if days > horizon {
			continue
		}
		title := p.Name
		if year > 1900 {
			title += fmt.Sprintf(" (%d anos)", next.Year()-year)
		}
		level := LevelInfo
		if days <= 1 {
			level = LevelWarn
		}
		out = append(out, Alert{Date: next, Days: days, Icon: "🎂", Label: "Aniversário", Title: title, NodeID: p.ID, Source: "contatos", Level: level})
	}
	return out
}

// Category is how a calendar event is classified.
type Category struct {
	Kind  string // profile kind it can become
	Icon  string
	Label string
}

var categories = []struct {
	re  *regexp.Regexp
	cat Category
}{
	{regexp.MustCompile(`(?i)cirurgi|opera[çc][ãa]o|internaç|procedimento`), Category{"surgery", "🏥", "Cirurgia"}},
	{regexp.MustCompile(`(?i)vacina`), Category{"vaccine", "💉", "Vacina"}},
	{regexp.MustCompile(`(?i)\bexame|laborat[óo]rio|ultrassom|resson[âa]ncia|tomografia|check.?up`), Category{"exam", "🧾", "Exame"}},
	{regexp.MustCompile(`(?i)consulta|m[ée]dic[oa]|dentista|dr\.|dra\.|terapia|psic[óo]log|nutricionista|fisioterap`), Category{"doctor", "👩‍⚕️", "Consulta"}},
	{regexp.MustCompile(`(?i)anivers[áa]rio|birthday`), Category{"date", "🎂", "Aniversário"}},
	{regexp.MustCompile(`(?i)casamento|bodas|formatura`), Category{"date", "💍", "Data importante"}},
	{regexp.MustCompile(`(?i)\bvoo\b|viagem|embarque|hotel|check.?in|aeroporto|flight`), Category{"trip", "✈️", "Viagem"}},
	{regexp.MustCompile(`(?i)\bcurso|aula|workshop|palestra|confer[êe]ncia|prova\b|vestibular|matr[íi]cula`), Category{"course", "🎓", "Curso/estudo"}},
	{regexp.MustCompile(`(?i)vencimento|renova[çc][ãa]o|ipva|licenciamento|passaporte|\bcnh\b`), Category{"document", "📄", "Vencimento"}},
}

// Classify maps a calendar event title to a profile category (ok=false for ordinary events).
func Classify(title string) (Category, bool) {
	for _, c := range categories {
		if c.re.MatchString(title) {
			return c.cat, true
		}
	}
	return Category{}, false
}

// EventAlerts turns classified calendar events into alerts; imported events are skipped.
func EventAlerts(evs []Event, now time.Time, imported map[string]bool) []Alert {
	today := midnight(now)
	var out []Alert
	for _, e := range evs {
		if imported[e.ID] {
			continue
		}
		c, ok := Classify(e.Title)
		if !ok {
			continue
		}
		start := e.Start.In(now.Location())
		title := e.Title
		if !e.AllDay {
			title += " · " + start.Format("15:04")
		}
		days := daysBetween(today, start)
		level := LevelInfo
		if days <= 1 {
			level = LevelWarn
		}
		out = append(out, Alert{Date: midnight(start), Days: days, Icon: c.Icon, Label: c.Label, Title: title, Source: "agenda", Level: level})
	}
	return out
}

// Merge sorts alerts by date and drops calendar birthdays already known from the profile
// or contacts (Google Calendar mirrors contact birthdays).
func Merge(lists ...[]Alert) []Alert {
	var all []Alert
	bday := map[string]bool{}
	for _, l := range lists {
		for _, a := range l {
			if a.Source != "agenda" && a.Icon == "🎂" {
				bday[DayKey(a.Date)] = true
			}
		}
	}
	for _, l := range lists {
		for _, a := range l {
			if a.Source == "agenda" && a.Icon == "🎂" && bday[DayKey(a.Date)] {
				continue
			}
			all = append(all, a)
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Date.Equal(all[j].Date) {
			return all[i].Date.Before(all[j].Date)
		}
		return all[i].Level == LevelLate && all[j].Level != LevelLate
	})
	return all
}

// Digest renders today's slots and the alerts as Telegram Markdown.
func Digest(heading string, slots []Slot, alerts []Alert) string {
	if len(slots) == 0 && len(alerts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(heading + "\n")
	for _, s := range slots {
		mark := "▫️"
		if s.Done {
			mark = "✔️"
		}
		when := s.Time
		if when == "" {
			when = "—"
		}
		fmt.Fprintf(&b, "%s %s %s %s", mark, when, s.Icon, s.Title)
		if s.Detail != "" {
			b.WriteString(" · " + s.Detail)
		}
		if s.Check && !s.Done {
			fmt.Fprintf(&b, " (/tomei %d)", s.ItemID)
		}
		b.WriteString("\n")
	}
	if len(alerts) > 0 {
		if len(slots) > 0 {
			b.WriteString("\n")
		}
		for _, a := range alerts {
			flag := ""
			if a.Level == LevelLate {
				flag = "⚠️ "
			}
			fmt.Fprintf(&b, "%s%s %s: %s — %s\n", flag, a.Icon, a.Label, a.Title, a.When())
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
