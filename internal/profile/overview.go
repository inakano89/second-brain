package profile

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"
)

// Overview is the whole personal picture for a moment in time.
type Overview struct {
	Now      time.Time
	Items    []Item
	Today    []Slot
	Tomorrow []Slot
	Alerts   []Alert
	Counts   map[string]int // items per section
	Access   string
	Locked   int // items that could not be decrypted
}

// Overview gathers items, today's and tomorrow's schedule and the alerts of the next
// `horizon` days, merging contact birthdays and the classified calendar events.
func (s *Store) Overview(ctx context.Context, now time.Time, horizon int, events []Event) (*Overview, error) {
	now = now.In(s.Location())
	today := midnight(now)
	tomorrow := today.AddDate(0, 0, 1)
	o := &Overview{Now: now, Counts: map[string]int{}, Access: s.Access()}
	var (
		people   []Person
		done     map[string]bool
		imported map[string]bool
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { o.Items, err = s.List(gctx, false); return })
	g.Go(func() (err error) { people, err = s.People(gctx); return })
	g.Go(func() (err error) { done, err = s.Checks(gctx, today, tomorrow); return })
	g.Go(func() error { imported = s.Imported(gctx); return nil })
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for _, it := range o.Items {
		o.Counts[it.Def().Section]++
		if it.Locked {
			o.Locked++
		}
	}
	o.Today = Today(o.Items, today, done)
	o.Tomorrow = Today(o.Items, tomorrow, done)
	o.Alerts = Merge(Upcoming(o.Items, now, horizon), Birthdays(people, now, horizon), EventAlerts(events, now, imported))
	return o, nil
}

// Within returns the alerts up to `days` ahead (overdue ones included).
func (o *Overview) Within(days int) []Alert {
	var out []Alert
	for _, a := range o.Alerts {
		if a.Days <= days {
			out = append(out, a)
		}
	}
	return out
}

// Pending returns today's slots not yet checked off.
func (o *Overview) Pending() []Slot {
	var out []Slot
	for _, s := range o.Today {
		if s.Check && !s.Done {
			out = append(out, s)
		}
	}
	return out
}
