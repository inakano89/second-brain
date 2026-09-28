package google

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
)

type gTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type gAttendee struct {
	Email          string `json:"email,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	Self           bool   `json:"self,omitempty"`
	Resource       bool   `json:"resource,omitempty"`
	ResponseStatus string `json:"responseStatus,omitempty"`
}

type gEvent struct {
	ID          string      `json:"id,omitempty"`
	Status      string      `json:"status,omitempty"`
	Summary     string      `json:"summary"`
	Description string      `json:"description,omitempty"`
	Location    string      `json:"location,omitempty"`
	HTMLLink    string      `json:"htmlLink,omitempty"`
	Start       gTime       `json:"start"`
	End         gTime       `json:"end"`
	Attendees   []gAttendee `json:"attendees,omitempty"`
}

func (c *Client) toEvent(e gEvent) agent.CalendarEvent {
	loc := c.cfg.Location()
	parse := func(t gTime) (time.Time, bool) {
		if t.DateTime != "" {
			v, _ := time.Parse(time.RFC3339, t.DateTime)
			return v, false
		}
		v, _ := time.ParseInLocation("2006-01-02", t.Date, loc)
		return v, true
	}
	s, allDay := parse(e.Start)
	en, _ := parse(e.End)
	ev := agent.CalendarEvent{ID: e.ID, Summary: e.Summary, Description: e.Description, Location: e.Location, Start: s, End: en, AllDay: allDay, Link: e.HTMLLink}
	for _, a := range e.Attendees {
		if a.Self || a.Resource || a.Email == "" {
			continue
		}
		ev.Attendees = append(ev.Attendees, agent.Attendee{Name: a.DisplayName, Email: strings.ToLower(a.Email), Status: a.ResponseStatus})
	}
	return ev
}

func (c *Client) calendarID() string { return url.PathEscape(c.cfg.Get("GOOGLE_CALENDAR_ID")) }

type calRef struct{ ID, Name string }

// readCalendars resolves GOOGLE_CALENDARS ("all" = every calendar shown in Google Calendar).
func (c *Client) readCalendars(ctx context.Context) []calRef {
	ids := c.cfg.GetList("GOOGLE_CALENDARS")
	if len(ids) > 0 && !strings.EqualFold(ids[0], "all") {
		out := make([]calRef, len(ids))
		for i, id := range ids {
			out[i] = calRef{ID: id}
		}
		return out
	}
	primary := []calRef{{ID: c.cfg.Get("GOOGLE_CALENDAR_ID")}}
	if !c.hasScope(scopeCalendarRead) {
		return primary
	}
	var resp struct {
		Items []struct {
			ID              string `json:"id"`
			Summary         string `json:"summary"`
			SummaryOverride string `json:"summaryOverride"`
			Selected        bool   `json:"selected"`
			Hidden          bool   `json:"hidden"`
			Primary         bool   `json:"primary"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "https://www.googleapis.com/calendar/v3/users/me/calendarList?maxResults=250", nil, &resp); err != nil {
		c.log.Warn("lista de agendas indisponível; usando a principal", "err", err)
		return primary
	}
	var out []calRef
	for _, it := range resp.Items {
		if it.Hidden || !(it.Selected || it.Primary) {
			continue
		}
		name := it.SummaryOverride
		if name == "" {
			name = it.Summary
		}
		if it.Primary {
			name = "Principal"
		}
		out = append(out, calRef{ID: it.ID, Name: name})
	}
	if len(out) == 0 {
		return primary
	}
	return out
}

func (c *Client) calendarEvents(ctx context.Context, cal calRef, from, to time.Time) ([]agent.CalendarEvent, error) {
	q := url.Values{}
	q.Set("timeMin", from.Format(time.RFC3339))
	q.Set("timeMax", to.Format(time.RFC3339))
	q.Set("singleEvents", "true")
	q.Set("orderBy", "startTime")
	q.Set("maxResults", "250")
	var out []agent.CalendarEvent
	for page := 0; page < 40; page++ {
		var resp struct {
			Summary       string   `json:"summary"`
			Items         []gEvent `json:"items"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := c.do(ctx, http.MethodGet, "https://www.googleapis.com/calendar/v3/calendars/"+url.PathEscape(cal.ID)+"/events?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		name := cal.Name
		if name == "" {
			name = resp.Summary
		}
		for _, e := range resp.Items {
			if e.Status == "cancelled" {
				continue
			}
			ev := c.toEvent(e)
			ev.Calendar = name
			out = append(out, ev)
		}
		if resp.NextPageToken == "" {
			break
		}
		q.Set("pageToken", resp.NextPageToken)
	}
	return out, nil
}

// ListEvents implements agent.GoogleAPI, merging every read calendar concurrently.
func (c *Client) ListEvents(ctx context.Context, from, to time.Time) ([]agent.CalendarEvent, error) {
	cals := c.readCalendars(ctx)
	results := make([][]agent.CalendarEvent, len(cals))
	errs := make([]error, len(cals))
	var g errgroup.Group
	g.SetLimit(4)
	for i, cal := range cals {
		g.Go(func() error {
			results[i], errs[i] = c.calendarEvents(ctx, cal, from, to)
			return nil
		})
	}
	_ = g.Wait()
	seen := map[string]bool{}
	var out []agent.CalendarEvent
	failed := 0
	for i, evs := range results {
		if errs[i] != nil {
			failed++
			c.log.Warn("agenda indisponível", "calendar", cals[i].ID, "err", errs[i])
			continue
		}
		for _, ev := range evs {
			if !seen[ev.ID] {
				seen[ev.ID] = true
				out = append(out, ev)
			}
		}
	}
	if failed == len(cals) {
		return nil, errs[0]
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// CreateEvent implements agent.GoogleAPI.
func (c *Client) CreateEvent(ctx context.Context, ev agent.CalendarEvent) (*agent.CalendarEvent, error) {
	loc := c.cfg.Location()
	ge := gEvent{Summary: ev.Summary, Description: ev.Description, Location: ev.Location}
	if ev.AllDay {
		ge.Start = gTime{Date: ev.Start.In(loc).Format("2006-01-02")}
		ge.End = gTime{Date: ev.End.In(loc).Format("2006-01-02")}
	} else {
		ge.Start = gTime{DateTime: ev.Start.Format(time.RFC3339), TimeZone: loc.String()}
		ge.End = gTime{DateTime: ev.End.Format(time.RFC3339), TimeZone: loc.String()}
	}
	var created gEvent
	if err := c.do(ctx, http.MethodPost, "https://www.googleapis.com/calendar/v3/calendars/"+c.calendarID()+"/events", ge, &created); err != nil {
		return nil, err
	}
	e := c.toEvent(created)
	c.log.Info("evento criado no Google Calendar", "summary", e.Summary, "start", e.Start)
	return &e, nil
}

// SyncCalendar mirrors events from yesterday to +14 days as event nodes, plus a
// one-time backfill of GOOGLE_CALENDAR_PAST_DAYS in monthly windows.
func (s *Syncer) SyncCalendar(ctx context.Context) (int, error) {
	if !s.g.Can(agent.GoogleCalendar) {
		return 0, nil
	}
	now := time.Now()
	windows := [][2]time.Time{{now.AddDate(0, 0, -1), now.AddDate(0, 0, 14)}}
	past := s.g.cfg.GetInt("GOOGLE_CALENDAR_PAST_DAYS", 365)
	const doneKey = "google.calendar.backfill_days"
	done := 0
	if v, ok, _ := s.ag.DB().KVGet(ctx, doneKey); ok {
		done = atoiDefault(v, 0)
	}
	if past > done {
		start := now.AddDate(0, 0, -past)
		for from := start; from.Before(now.AddDate(0, 0, -1)); from = from.AddDate(0, 1, 0) {
			to := from.AddDate(0, 1, 0)
			if limit := now.AddDate(0, 0, -1); to.After(limit) {
				to = limit
			}
			windows = append(windows, [2]time.Time{from, to})
		}
	}
	var mu sync.Mutex
	total := 0
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	for _, w := range windows {
		g.Go(func() error {
			evs, err := s.g.ListEvents(gctx, w[0], w[1])
			if err != nil {
				return err
			}
			for _, ev := range evs {
				if _, err := s.ag.UpsertEventNode(gctx, ev); err != nil && !errors.Is(err, database.ErrDeleted) {
					return err
				}
			}
			mu.Lock()
			total += len(evs)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return total, err
	}
	if past > done {
		_ = s.ag.DB().KVSet(ctx, doneKey, itoa(past))
		s.g.log.Info("histórico da agenda importado", "days", past)
	}
	return total, nil
}
