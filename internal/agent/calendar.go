package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

// CalendarEvent is a provider-neutral calendar entry.
type CalendarEvent struct {
	ID          string    `json:"id"`
	Summary     string    `json:"summary"`
	Description string    `json:"description,omitempty"`
	Location    string    `json:"location,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day"`
	Link        string    `json:"link,omitempty"`
}

// CalendarAPI is implemented by the Google Calendar integration.
type CalendarAPI interface {
	Connected() bool
	ListEvents(ctx context.Context, from, to time.Time) ([]CalendarEvent, error)
	CreateEvent(ctx context.Context, ev CalendarEvent) (*CalendarEvent, error)
}

// ErrNoCalendar means Google Calendar is not connected.
var ErrNoCalendar = errors.New("google calendar não conectado")

// ParseEvent turns natural language ("amanhã 15h reunião com Ana") into an event.
func (a *Agent) ParseEvent(ctx context.Context, text string) (*CalendarEvent, error) {
	loc := a.cfg.Location()
	nowLocal := time.Now().In(loc)
	var out struct {
		Summary     string `json:"summary"`
		Start       string `json:"start"`
		End         string `json:"end"`
		AllDay      bool   `json:"all_day"`
		Location    string `json:"location"`
		Description string `json:"description"`
	}
	err := a.llm.CompleteJSON(ctx, "", llm.Request{
		Purpose: "event-parse", MaxTokens: 1500, Effort: "low",
		System: fmt.Sprintf(`Extraia um evento de agenda do texto. Agora é %s (timezone %s).
Responda SOMENTE JSON: {"summary":"...","start":"YYYY-MM-DDTHH:MM:SS","end":"YYYY-MM-DDTHH:MM:SS","all_day":false,"location":"","description":""}
Se não houver horário de término, use 1 hora de duração. Para dia inteiro use all_day=true e horários 00:00:00.`, nowLocal.Format("2006-01-02 15:04 Monday"), loc.String()),
		Messages: []llm.Message{{Role: llm.RoleUser, Content: text}},
	}, &out)
	if err != nil {
		return nil, err
	}
	start, err := parseLocal(out.Start, loc)
	if err != nil {
		return nil, fmt.Errorf("data de início inválida: %w", err)
	}
	end, err := parseLocal(out.End, loc)
	if err != nil || !end.After(start) {
		end = start.Add(time.Hour)
		if out.AllDay {
			end = start.AddDate(0, 0, 1)
		}
	}
	return &CalendarEvent{Summary: strings.TrimSpace(out.Summary), Start: start, End: end, AllDay: out.AllDay, Location: out.Location, Description: out.Description}, nil
}

func parseLocal(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if l == time.RFC3339 {
			if t, err := time.Parse(l, s); err == nil {
				return t.In(loc), nil
			}
			continue
		}
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("formato de data não reconhecido: %q", s)
}

// CreateEvent creates the event in Google Calendar and mirrors it as an event node.
func (a *Agent) CreateEvent(ctx context.Context, ev CalendarEvent) (*CalendarEvent, *database.Node, error) {
	cal := a.calendar()
	if cal == nil {
		return nil, nil, ErrNoCalendar
	}
	created, err := cal.CreateEvent(ctx, ev)
	if err != nil {
		return nil, nil, err
	}
	node, err := a.UpsertEventNode(ctx, *created)
	return created, node, err
}

// CreateEventNL parses natural language and creates the event.
func (a *Agent) CreateEventNL(ctx context.Context, text string) (*CalendarEvent, *database.Node, error) {
	if a.calendar() == nil {
		return nil, nil, ErrNoCalendar
	}
	ev, err := a.ParseEvent(ctx, text)
	if err != nil {
		return nil, nil, err
	}
	return a.CreateEvent(ctx, *ev)
}

// UpsertEventNode mirrors a calendar event into the graph.
func (a *Agent) UpsertEventNode(ctx context.Context, ev CalendarEvent) (*database.Node, error) {
	loc := a.cfg.Location()
	var b strings.Builder
	fmt.Fprintf(&b, "**Início:** %s\n**Fim:** %s\n", ev.Start.In(loc).Format("02/01/2006 15:04"), ev.End.In(loc).Format("02/01/2006 15:04"))
	if ev.Location != "" {
		fmt.Fprintf(&b, "**Local:** %s\n", ev.Location)
	}
	if ev.Description != "" {
		b.WriteString("\n" + ev.Description + "\n")
	}
	start := ev.Start
	n, created, err := a.Ingest(ctx, IngestInput{
		Type: database.TypeEvent, Title: ev.Summary, Content: b.String(), Source: "calendar", SourceRef: ev.ID,
		DueAt: &start, Meta: map[string]any{"start": ev.Start.Format(time.RFC3339), "end": ev.End.Format(time.RFC3339), "link": ev.Link, "all_day": ev.AllDay},
	})
	if err != nil {
		return nil, err
	}
	if created {
		_ = a.QueueEnrich(ctx, n.ID)
	}
	return n, nil
}

// EventsBetween lists calendar events (live API, falling back to event nodes).
func (a *Agent) EventsBetween(ctx context.Context, from, to time.Time) ([]CalendarEvent, error) {
	if cal := a.calendar(); cal != nil {
		evs, err := cal.ListEvents(ctx, from, to)
		if err == nil {
			return evs, nil
		}
		a.log.Warn("calendar indisponível, usando cache local", "err", err)
	}
	nodes, err := a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeEvent}, Order: "due", Limit: 500})
	if err != nil {
		return nil, err
	}
	var out []CalendarEvent
	for _, n := range nodes {
		if n.DueAt == nil || n.DueAt.Before(from) || !n.DueAt.Before(to) {
			continue
		}
		ev := CalendarEvent{ID: n.SourceRef, Summary: n.Title, Start: *n.DueAt, End: n.DueAt.Add(time.Hour)}
		if s, ok := n.Meta["end"].(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				ev.End = t
			}
		}
		out = append(out, ev)
	}
	return out, nil
}
