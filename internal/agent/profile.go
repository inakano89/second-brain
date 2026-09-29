package agent

import (
	"context"
	"time"

	"github.com/inakano89/second-brain/internal/profile"
)

// ProfileOverview gathers the personal overview with the calendar events of the next
// `horizon` days (fetched concurrently with the profile data).
func (a *Agent) ProfileOverview(ctx context.Context, now time.Time, horizon int) (*profile.Overview, error) {
	loc := a.cfg.Location()
	start := now.In(loc)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	evCh := make(chan []profile.Event, 1)
	go func() {
		var out []profile.Event
		evs, err := a.EventsBetween(ctx, start, start.AddDate(0, 0, horizon+1))
		if err != nil {
			a.log.Warn("perfil: agenda indisponível", "err", err)
		}
		for _, e := range evs {
			out = append(out, profile.Event{ID: e.ID, Title: e.Summary, Start: e.Start, AllDay: e.AllDay})
		}
		evCh <- out
	}()
	o, err := a.profile.Overview(ctx, now, horizon, nil)
	events := <-evCh
	if err != nil {
		return nil, err
	}
	imported := a.profile.Imported(ctx)
	o.Alerts = profile.Merge(o.Alerts, profile.EventAlerts(events, now.In(loc), imported))
	return o, nil
}

// CalendarSuggestions lists calendar events of the next year that look like profile items
// (surgeries, courses, trips, birthdays…) and were not added yet.
func (a *Agent) CalendarSuggestions(ctx context.Context, now time.Time) ([]profile.Suggestion, error) {
	loc := a.cfg.Location()
	start := now.In(loc).AddDate(0, 0, -30)
	evs, err := a.EventsBetween(ctx, start, now.In(loc).AddDate(1, 0, 0))
	if err != nil {
		return nil, err
	}
	list := make([]profile.Event, 0, len(evs))
	for _, e := range evs {
		list = append(list, profile.Event{ID: e.ID, Title: e.Summary, Start: e.Start, AllDay: e.AllDay})
	}
	return profile.Suggest(list, a.profile.Imported(ctx), loc), nil
}

func (a *Agent) profileForAI(ctx context.Context, query, kind string) (any, error) {
	local := a.llm.IsLocal("chat") && a.llm.IsLocal("telegram")
	return a.profile.ForAI(ctx, query, kind, local)
}
