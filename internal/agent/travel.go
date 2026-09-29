package agent

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/profile"
)

// Travel dossier: everything the brain knows about a trip in one message: agenda around the
// dates, bookings found in Gmail, related notes and files, open tasks and, from the personal
// profile, documents that expire, vaccines and medication stock.

const toolTravel = "travel_dossier"

const bookingQuery = `(reserva OR voo OR passagem OR hotel OR hospedagem OR booking OR airbnb OR "e-ticket" OR localizador OR "check-in" OR itinerário)`

// TravelOptions selects the trip.
type TravelOptions struct {
	Destination string
	From, To    time.Time // trip dates; taken from the profile's trip item when empty
	// Personal adds the sections built from the personal profile. Sensitive items (documents,
	// medications, vaccines) also need SensitiveOK.
	Personal    bool
	SensitiveOK bool
}

var travelTaskRe = regexp.MustCompile(`(?i)viag|passagem|hotel|reserva|passaporte|mala|check-?in|visto|seguro`)

// tripItems finds profile trips matching destination (or the next trip when it is empty).
func (a *Agent) tripItems(ctx context.Context, destination string, now time.Time) []profile.Item {
	items, err := a.profile.List(ctx, false)
	if err != nil {
		return nil
	}
	loc := a.cfg.Location()
	var out []profile.Item
	for _, it := range items {
		if it.Kind != "trip" || it.Locked {
			continue
		}
		start, ok := it.Date("start", loc)
		if destination != "" {
			if !strings.Contains(strings.ToLower(it.Title), strings.ToLower(destination)) {
				continue
			}
		} else if !ok || start.Before(now.AddDate(0, 0, -1)) {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i].Date("start", loc)
		b, _ := out[j].Date("start", loc)
		return a.Before(b)
	})
	return out
}

// UpcomingTrips lists profile trips starting within the next days.
func (a *Agent) UpcomingTrips(ctx context.Context, now time.Time, days int) []profile.Item {
	loc := a.cfg.Location()
	today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	var out []profile.Item
	for _, it := range a.tripItems(ctx, "", today) {
		if start, ok := it.Date("start", loc); ok && !start.Before(today) && start.Before(today.AddDate(0, 0, days+1)) {
			out = append(out, it)
		}
	}
	return out
}

// TravelDossier assembles the dossier text (Telegram Markdown).
func (a *Agent) TravelDossier(ctx context.Context, opts TravelOptions) (string, error) {
	loc := a.cfg.Location()
	now := time.Now().In(loc)
	dest := strings.TrimSpace(opts.Destination)
	var trip *profile.Item
	if opts.Personal || opts.From.IsZero() {
		if trips := a.tripItems(ctx, dest, now); len(trips) > 0 && (opts.SensitiveOK || opts.Personal || !trips[0].Sensitive) {
			trip = &trips[0]
			if dest == "" {
				dest = trip.Title
			}
			if opts.From.IsZero() {
				opts.From, _ = trip.Date("start", loc)
			}
			if opts.To.IsZero() {
				opts.To, _ = trip.Date("end", loc)
			}
		}
	}
	if dest == "" && opts.From.IsZero() {
		return "", fmt.Errorf("informe o destino ou as datas da viagem")
	}
	if opts.To.IsZero() && !opts.From.IsZero() {
		opts.To = opts.From.AddDate(0, 0, 7)
	}
	if opts.From.IsZero() {
		opts.From = now
		opts.To = now.AddDate(0, 0, 30)
	}

	// Each variable is written by a single goroutine.
	var (
		events               []CalendarEvent
		mails                []EmailInfo
		files                []DriveFile
		notes, tasks         []database.Node
		evErr, mailErr       error
		bookingNodes         []database.Node
		wg                   errgroup.Group
		windowFrom, windowTo = opts.From.AddDate(0, 0, -1), opts.To.AddDate(0, 0, 2)
	)
	wg.Go(func() error {
		events, evErr = a.EventsBetween(ctx, windowFrom, windowTo)
		return nil
	})
	if g := a.google(GoogleGmail); g != nil {
		wg.Go(func() error {
			q := bookingQuery + " newer_than:2y"
			if dest != "" {
				q = fmt.Sprintf("%s %q", bookingQuery, dest)
			}
			mails, mailErr = g.SearchEmail(ctx, q, 8)
			return nil
		})
	}
	if g := a.google(GoogleDrive); g != nil && dest != "" {
		wg.Go(func() error { files, _ = g.SearchDrive(ctx, dest, 6); return nil })
	}
	wg.Go(func() error {
		if dest != "" {
			hits, _ := a.db.SearchFTS(ctx, dest, database.NodeFilter{Types: []string{database.TypeNote, database.TypeArticle, database.TypeInsight}}, 12)
			for _, h := range hits {
				if h.Source == "gmail" {
					bookingNodes = append(bookingNodes, h.Node)
				} else if !onThisDaySkip[h.Source] && len(notes) < 6 {
					notes = append(notes, h.Node)
				}
			}
		}
		for _, tag := range []string{"viagem", "travel"} {
			list, _ := a.db.ListNodes(ctx, database.NodeFilter{Tag: tag, Limit: 6})
			for _, n := range list {
				if len(notes) < 8 && !hasNode(notes, n.ID) {
					notes = append(notes, n)
				}
			}
		}
		open, _ := a.db.OpenTasks(ctx, 200)
		for _, t := range open {
			text := t.Title + " " + t.Content
			if (dest != "" && strings.Contains(strings.ToLower(text), strings.ToLower(dest))) || travelTaskRe.MatchString(t.Title) || containsTag(t.Tags, "viagem") {
				tasks = append(tasks, t)
			}
		}
		return nil
	})
	_ = wg.Wait()

	var b strings.Builder
	days := int(opts.To.Sub(opts.From).Hours()/24) + 1
	title := "Viagem"
	if dest != "" {
		title = oneLine(dest)
	}
	fmt.Fprintf(&b, "✈️ *Dossiê: %s* (%s – %s, %d dias)\n", title, opts.From.In(loc).Format("02/01"), opts.To.In(loc).Format("02/01"), days)
	if d := int(opts.From.Sub(now).Hours() / 24); d > 0 {
		fmt.Fprintf(&b, "Faltam %d dias.\n", d)
	}

	// Agenda: what is on the calendar during the trip conflicts with it.
	b.WriteString("\n📅 *Agenda*\n")
	var during, around []CalendarEvent
	for _, e := range events {
		if e.Start.Before(opts.To.AddDate(0, 0, 1)) && !e.End.Before(opts.From) {
			during = append(during, e)
		} else {
			around = append(around, e)
		}
	}
	switch {
	case evErr != nil:
		b.WriteString("(agenda indisponível)\n")
	case len(during)+len(around) == 0:
		b.WriteString("Nada marcado durante a viagem. ✅\n")
	}
	for _, e := range during[:min(10, len(during))] {
		fmt.Fprintf(&b, "⚠️ %s — %s\n", e.Start.In(loc).Format("02/01 15:04"), oneLine(e.Summary))
	}
	for _, e := range around[:min(4, len(around))] {
		fmt.Fprintf(&b, "• %s — %s\n", e.Start.In(loc).Format("02/01 15:04"), oneLine(e.Summary))
	}

	b.WriteString("\n🎫 *Reservas e comprovantes*\n")
	found := false
	for _, m := range mails {
		fmt.Fprintf(&b, "• %s — %s (%s)\n", m.Date.In(loc).Format("02/01/2006"), oneLine(m.Subject), oneLine(m.From))
		found = true
	}
	for _, n := range bookingNodes[:min(5, len(bookingNodes))] {
		fmt.Fprintf(&b, "• %s — %s (#%d)\n", n.EffectiveAt().In(loc).Format("02/01/2006"), oneLine(n.Title), n.ID)
		found = true
	}
	if mailErr != nil {
		b.WriteString("(Gmail indisponível)\n")
	} else if !found {
		b.WriteString("Nenhum e-mail de reserva encontrado.\n")
	}

	if len(files)+len(notes) > 0 {
		b.WriteString("\n📎 *Arquivos e notas*\n")
		for _, f := range files {
			fmt.Fprintf(&b, "• 📄 %s\n", oneLine(f.Name))
		}
		for _, n := range notes {
			fmt.Fprintf(&b, "• %s (#%d)\n", oneLine(n.Title), n.ID)
		}
	}
	if len(tasks) > 0 {
		b.WriteString("\n☑️ *Pendências*\n")
		for _, t := range tasks[:min(8, len(tasks))] {
			fmt.Fprintf(&b, "• #%d %s\n", t.ID, oneLine(t.Title))
		}
	}
	if opts.Personal {
		if p := a.travelPersonal(ctx, trip, opts, now, days); p != "" {
			b.WriteString("\n" + p)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// travelPersonal builds the profile-based checks: documents, vaccines, medication and checklist.
func (a *Agent) travelPersonal(ctx context.Context, trip *profile.Item, opts TravelOptions, now time.Time, tripDays int) string {
	items, err := a.profile.List(ctx, false)
	if err != nil {
		return ""
	}
	loc := a.cfg.Location()
	readable := func(it profile.Item) bool { return !it.Locked && (!it.Sensitive || opts.SensitiveOK) }
	var lines []string
	sixMonths := opts.To.AddDate(0, 6, 0)
	for _, it := range items {
		if !readable(it) {
			continue
		}
		switch it.Kind {
		case "document":
			if exp, ok := it.Date("expires", loc); ok {
				switch {
				case exp.Before(opts.To):
					lines = append(lines, fmt.Sprintf("🛑 *%s* vence em %s, antes de a viagem acabar", oneLine(it.Title), exp.Format("02/01/2006")))
				case exp.Before(sixMonths):
					lines = append(lines, fmt.Sprintf("⚠️ *%s* vence em %s: menos de 6 meses após a volta (muitos países exigem 6 meses)", oneLine(it.Title), exp.Format("02/01/2006")))
				}
			}
		case "vaccine":
			if next, ok := it.Date("next", loc); ok && !next.After(opts.To) {
				lines = append(lines, fmt.Sprintf("💉 *%s*: próxima dose em %s", oneLine(it.Title), next.Format("02/01/2006")))
			}
		case "medication", "supplement":
			if left, ok := profile.DaysLeft(it); ok && left < float64(tripDays)+3 {
				lines = append(lines, fmt.Sprintf("💊 *%s*: estoque para ~%d dias e a viagem dura %d — leve o suficiente", oneLine(it.Title), int(left), tripDays))
			}
		}
	}
	if trip != nil && readable(*trip) {
		if bk := trip.Get("bookings"); bk != "" {
			lines = append(lines, "🎫 Reservas anotadas no Perfil:\n"+indent(bk))
		}
		if ck := trip.Get("checklist"); ck != "" {
			lines = append(lines, "🧳 Checklist do Perfil:\n"+indent(ck))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "🪪 *Do seu Perfil*\n" + strings.Join(lines, "\n")
}

func indent(s string) string {
	var out []string
	for _, ln := range strings.Split(strings.TrimSpace(s), "\n") {
		out = append(out, "   "+strings.TrimSpace(ln))
	}
	return strings.Join(out, "\n")
}

func hasNode(list []database.Node, id int64) bool {
	for _, n := range list {
		if n.ID == id {
			return true
		}
	}
	return false
}

func containsTag(tags []string, t string) bool {
	for _, x := range tags {
		if x == t {
			return true
		}
	}
	return false
}

// travelForChat runs the dossier for the chat tool, honouring PROFILE_AI_ACCESS: the personal
// sections are added only when the policy lets this model read them.
func (a *Agent) travelForChat(ctx context.Context, args toolArgs) (any, error) {
	loc := a.cfg.Location()
	opts := TravelOptions{Destination: args.str("destination")}
	if s := args.str("from"); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			return nil, fmt.Errorf("data inválida em from: %s", s)
		}
		opts.From = t
	}
	if s := args.str("to"); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			return nil, fmt.Errorf("data inválida em to: %s", s)
		}
		opts.To = t
	}
	access := a.profile.Access()
	local := a.llm.IsLocal("chat") && a.llm.IsLocal("telegram")
	opts.Personal = access != profile.AccessNone
	opts.SensitiveOK = access == profile.AccessFull || (access == profile.AccessBasic && local)
	text, err := a.TravelDossier(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"dossier": text}
	if opts.Personal && !opts.SensitiveOK {
		out["note"] = "Documentos, vacinas e medicações do Perfil são sensíveis e não foram incluídos (use um modelo local ou PROFILE_AI_ACCESS=full)."
	}
	return out, nil
}
