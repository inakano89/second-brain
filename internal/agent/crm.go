package agent

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
)

// Personal CRM: who you have not talked to in a while, and a short brief of a person before
// a meeting. Interactions are the dated notes, e-mails and events linked to the person
// (imports and periodic reports do not count).

const (
	tagCRM   = "crm"     // opt in: remind me about this person
	tagNoCRM = "sem-crm" // never remind me about this person
	// metaContactEvery overrides CRM_STALE_DAYS for one person (days).
	metaContactEvery = "contact_every_days"
	// crmMinInteractions is how many interactions make someone "frequent" without the crm tag.
	crmMinInteractions = 3
)

// CRMStaleDays is CRM_STALE_DAYS (0 turns the reminders off).
func (a *Agent) CRMStaleDays() int { return max(a.cfg.GetInt("CRM_STALE_DAYS", 90), 0) }

// PersonBrief summarises what the brain knows about one person.
type PersonBrief struct {
	Person    database.Node
	Last      time.Time       // date of the latest past interaction (zero when none)
	LastTitle string          // what that interaction was
	Count     int             // dated interactions
	Recent    []database.Node // latest interactions, newest first
	OpenTasks []database.Node
	Upcoming  []database.Node // future events with the person
}

func isInteraction(n *database.Node) bool {
	return n.Type != database.TypePerson && n.Type != database.TypeTask && !n.DateUnknown() && !n.Imported() && !onThisDaySkip[n.Source]
}

// PersonBriefOf builds the brief of a person node.
func (a *Agent) PersonBriefOf(ctx context.Context, id int64, now time.Time) (*PersonBrief, error) {
	p, err := a.db.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Type != database.TypePerson {
		return nil, fmt.Errorf("o nó #%d não é uma pessoa", id)
	}
	links, err := a.db.Neighbors(ctx, id)
	if err != nil {
		return nil, err
	}
	b := &PersonBrief{Person: *p}
	seen := map[int64]bool{}
	for _, l := range links {
		n := l.Node
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		switch {
		case n.Type == database.TypeTask:
			if n.Status != database.StatusDone {
				b.OpenTasks = append(b.OpenTasks, n)
			}
		case n.Type == database.TypeEvent && n.DueAt != nil && n.DueAt.After(now):
			b.Upcoming = append(b.Upcoming, n)
		case isInteraction(&n) && !n.EffectiveAt().After(now):
			b.Recent = append(b.Recent, n)
		}
	}
	sort.Slice(b.Recent, func(i, j int) bool { return b.Recent[i].EffectiveAt().After(b.Recent[j].EffectiveAt()) })
	sort.Slice(b.Upcoming, func(i, j int) bool { return b.Upcoming[i].DueAt.Before(*b.Upcoming[j].DueAt) })
	b.Count = len(b.Recent)
	if b.Count > 0 {
		b.Last, b.LastTitle = b.Recent[0].EffectiveAt(), b.Recent[0].Title
		b.Recent = b.Recent[:min(5, len(b.Recent))]
	}
	return b, nil
}

// FindPerson resolves a name (or "#id") to a person node.
func (a *Agent) FindPerson(ctx context.Context, q string) (*database.Node, error) {
	q = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(q), "#"))
	if q == "" {
		return nil, fmt.Errorf("informe o nome da pessoa")
	}
	var id int64
	if _, err := fmt.Sscan(q, &id); err == nil && id > 0 {
		if n, err := a.db.GetNode(ctx, id); err == nil && n.Type == database.TypePerson {
			return n, nil
		}
	}
	if n, err := a.db.FindByTitle(ctx, database.TypePerson, q); err == nil {
		return n, nil
	}
	hits, err := a.db.SearchFTS(ctx, q, database.NodeFilter{Types: []string{database.TypePerson}}, 1)
	if err != nil || len(hits) == 0 {
		return nil, fmt.Errorf("não encontrei ninguém chamado “%s”", q)
	}
	return &hits[0].Node, nil
}

// ago says how long before now t was ("3 semanas", "2 meses").
func ago(t, now time.Time) string {
	days := int(now.Sub(t).Hours() / 24)
	switch {
	case days < 1:
		return "hoje"
	case days == 1:
		return "1 dia"
	case days < 14:
		return fmt.Sprintf("%d dias", days)
	case days < 60:
		return fmt.Sprintf("%d semanas", days/7)
	case days < 730:
		return fmt.Sprintf("%d meses", days/30)
	}
	return fmt.Sprintf("%d anos", days/365)
}

// FormatPersonBrief renders a brief as a short Telegram message.
func FormatPersonBrief(b *PersonBrief, loc *time.Location, now time.Time) string {
	var s strings.Builder
	p := b.Person
	fmt.Fprintf(&s, "👤 *%s*", oneLine(p.Title))
	if c, _ := p.Meta["company"].(string); c != "" {
		s.WriteString(" — " + oneLine(c))
	}
	s.WriteString("\n")
	if b.Count == 0 {
		s.WriteString("Sem interações registradas.\n")
	} else {
		fmt.Fprintf(&s, "Última interação há %s (%s): %s · %d no total\n", ago(b.Last, now), b.Last.In(loc).Format("02/01/2006"), oneLine(b.LastTitle), b.Count)
	}
	for i, n := range b.Recent {
		if i == 0 {
			continue // already shown as the last one
		}
		if i > 3 {
			break
		}
		fmt.Fprintf(&s, "  · %s: %s\n", n.EffectiveAt().In(loc).Format("02/01/2006"), oneLine(n.Title))
	}
	for _, t := range b.OpenTasks[:min(4, len(b.OpenTasks))] {
		fmt.Fprintf(&s, "☑️ #%d %s\n", t.ID, oneLine(t.Title))
	}
	for _, e := range b.Upcoming[:min(2, len(b.Upcoming))] {
		fmt.Fprintf(&s, "📅 %s: %s\n", e.DueAt.In(loc).Format("02/01 15:04"), oneLine(e.Title))
	}
	if bd, _ := p.Meta["birthday"].(string); bd != "" {
		fmt.Fprintf(&s, "🎂 %s\n", bd)
	}
	return strings.TrimRight(s.String(), "\n")
}

// StaleContact is someone you have not talked to for longer than expected.
type StaleContact struct {
	Person database.Node
	Last   time.Time
	Count  int
	Every  int // days the person is expected to be heard within
}

// StaleContacts lists people whose last interaction is older than their threshold: everyone
// tagged "crm" (or with contact_every_days) plus frequent contacts (3+ interactions), minus
// those tagged "sem-crm" or with a meeting already scheduled. Most overdue first.
func (a *Agent) StaleContacts(ctx context.Context, now time.Time, limit int) ([]StaleContact, error) {
	def := a.CRMStaleDays()
	if def == 0 {
		return nil, nil
	}
	people, err := a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypePerson}, Limit: 5000})
	if err != nil {
		return nil, err
	}
	var (
		out []StaleContact
		mu  sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(6)
	for i := range people {
		p := people[i]
		if slices.Contains(p.Tags, tagNoCRM) {
			continue
		}
		every := def
		opted := slices.Contains(p.Tags, tagCRM)
		if v, ok := p.Meta[metaContactEvery].(float64); ok && v > 0 {
			every, opted = int(v), true
		}
		g.Go(func() error {
			last, count, err := a.db.LastInteraction(gctx, p.ID, now)
			if err != nil || last.IsZero() || (!opted && count < crmMinInteractions) {
				return err
			}
			if now.Sub(last) < time.Duration(every)*24*time.Hour {
				return nil
			}
			if up, err := a.db.HasUpcoming(gctx, p.ID, now); err != nil || up {
				return err
			}
			mu.Lock()
			out = append(out, StaleContact{Person: p, Last: last, Count: count, Every: every})
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := now.Sub(out[i].Last).Hours()/float64(out[i].Every), now.Sub(out[j].Last).Hours()/float64(out[j].Every)
		if di != dj {
			return di > dj
		}
		return out[i].Person.ID < out[j].Person.ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FormatStaleContacts renders the reminder message ("" when the list is empty).
func FormatStaleContacts(list []StaleContact, loc *time.Location, now time.Time) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("👥 *Faz tempo que você não fala com…*\n")
	for _, c := range list {
		fmt.Fprintf(&b, "• *%s* — há %s (%s) · /pessoa %d\n", oneLine(c.Person.Title), ago(c.Last, now), c.Last.In(loc).Format("02/01/2006"), c.Person.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}

// CRMReminders returns the people to reach out to that were not reported for their current
// silence yet (each person is reported once per stretch without contact).
func (a *Agent) CRMReminders(ctx context.Context, now time.Time, limit int) ([]StaleContact, error) {
	list, err := a.StaleContacts(ctx, now, 0)
	if err != nil {
		return nil, err
	}
	var fresh []StaleContact
	for _, c := range list {
		if len(fresh) >= limit {
			break
		}
		newly, err := a.db.MarkSeen(ctx, "crm", fmt.Sprintf("%d:%s", c.Person.ID, c.Last.UTC().Format("2006-01-02")))
		if err != nil {
			return nil, err
		}
		if newly {
			fresh = append(fresh, c)
		}
	}
	return fresh, nil
}

// MeetingPrep returns a brief for each event starting within lead of now whose guests include
// people in the brain (e-mail match), reporting each event once. Events come from the local
// mirror of the calendar, so the Calendar sync routine must be active.
func (a *Agent) MeetingPrep(ctx context.Context, now time.Time, lead time.Duration) ([]string, error) {
	end := now.Add(lead)
	events, err := a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeEvent}, DueFrom: &now, DueTo: &end, Order: "due", Limit: 30})
	if err != nil {
		return nil, err
	}
	loc := a.cfg.Location()
	var out []string
	for _, ev := range events {
		if allDay, _ := ev.Meta["all_day"].(bool); allDay {
			continue
		}
		emails := metaStringList(ev.Meta["attendees"])
		var briefs []string
		seenPerson := map[int64]bool{}
		for _, e := range emails {
			p, err := a.db.FindPersonByEmail(ctx, e)
			if err != nil || seenPerson[p.ID] {
				continue
			}
			seenPerson[p.ID] = true
			b, err := a.PersonBriefOf(ctx, p.ID, now)
			if err != nil {
				continue
			}
			briefs = append(briefs, FormatPersonBrief(b, loc, now))
		}
		if len(briefs) == 0 {
			continue
		}
		if newly, err := a.db.MarkSeen(ctx, "prep", fmt.Sprintf("%s|%s", ev.SourceRef, ev.DueAt.UTC().Format(time.RFC3339))); err != nil || !newly {
			continue
		}
		head := fmt.Sprintf("📅 *Em %s: %s* (%s)\n", ago2(ev.DueAt.Sub(now)), oneLine(ev.Title), ev.DueAt.In(loc).Format("15:04"))
		out = append(out, head+strings.Join(briefs, "\n\n"))
	}
	return out, nil
}

func ago2(d time.Duration) string {
	m := int(d.Minutes())
	if m < 1 {
		return "instantes"
	}
	return fmt.Sprintf("%d min", m)
}

func metaStringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// briefTool is the read-only chat tool result for a person brief.
func (a *Agent) briefTool(ctx context.Context, q string) (any, error) {
	p, err := a.FindPerson(ctx, q)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	b, err := a.PersonBriefOf(ctx, p.ID, now)
	if err != nil {
		return nil, err
	}
	loc := a.cfg.Location()
	nodes := func(list []database.Node) []nodeBrief {
		out := make([]nodeBrief, 0, len(list))
		for i := range list {
			out = append(out, brief(&list[i], loc))
		}
		return out
	}
	out := map[string]any{
		"person": brief(&b.Person, loc), "interactions": b.Count, "recent": nodes(b.Recent),
		"open_tasks": nodes(b.OpenTasks), "upcoming_events": nodes(b.Upcoming), "details": personDetails(&b.Person),
		"summary": extract.Truncate(b.Person.Content, 1500),
	}
	if b.Count > 0 {
		out["last_interaction"] = b.Last.In(loc).Format("2006-01-02")
		out["days_since_last_interaction"] = int(now.Sub(b.Last).Hours() / 24)
	}
	return out, nil
}
