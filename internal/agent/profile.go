package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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

// isProfileTool reports whether a tool call touches profile data (its turn is stored encrypted).
func isProfileTool(name string) bool { return name == toolProfile || name == toolProfileSave }

// profileSaveDoc lists, per kind, the field keys the model may fill in.
func profileSaveDoc() string {
	hint := map[string]string{
		profile.FDate: "AAAA-MM-DD", profile.FTime: "HH:MM", profile.FTimes: "HH:MM, HH:MM",
		profile.FWeekdays: "seg,qua… (vazio = todos os dias)", profile.FNumber: "número", profile.FDay: "1-31",
	}
	var b strings.Builder
	for _, k := range profile.Kinds {
		fmt.Fprintf(&b, "- %s (título: %s):", k.Key, k.TitleLabel)
		for i, f := range k.Fields {
			if i > 0 {
				b.WriteString(";")
			}
			fmt.Fprintf(&b, " %s", f.Key)
			switch {
			case f.Type == profile.FSelect:
				var opts []string
				for _, o := range f.Options {
					if o != "" {
						opts = append(opts, o)
					}
				}
				fmt.Fprintf(&b, "[%s]", strings.Join(opts, "|"))
			case hint[f.Type] != "":
				fmt.Fprintf(&b, "[%s]", hint[f.Type])
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// profileFields reads the "fields" argument: a list of {key, value} (or a plain object).
func profileFields(raw any) map[string]string {
	out := map[string]string{}
	set := func(k string, v any) {
		k = strings.TrimSpace(k)
		if k == "" || v == nil {
			return
		}
		switch x := v.(type) {
		case string:
			out[k] = x
		case float64:
			out[k] = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			out[k] = strconv.FormatBool(x)
		}
	}
	switch x := raw.(type) {
	case []any:
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				k, _ := m["key"].(string)
				set(k, m["value"])
			}
		}
	case map[string]any:
		for k, v := range x {
			set(k, v)
		}
	}
	return out
}

// profileSave creates or updates a profile item for the model. The reply repeats only what
// the model sent, never the stored item, so nothing hidden by PROFILE_AI_ACCESS leaks.
func (a *Agent) profileSave(ctx context.Context, args toolArgs) (any, error) {
	fields := profileFields(args["fields"])
	it, created, err := a.profile.UpsertFromAI(ctx, args.str("kind"), args.str("title"), fields)
	if err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "created": created, "id": it.ID, "kind": it.Def().Label, "title": it.Title, "written": fields}, nil
}
