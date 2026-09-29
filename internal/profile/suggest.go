package profile

import (
	"sort"
	"strings"
	"time"
)

// Suggestion is a calendar event that can become a profile item.
type Suggestion struct {
	Event    Event
	Category Category
}

// Suggest classifies events and keeps the first occurrence of each title (weekly classes
// and yearly birthdays show up once), skipping those already imported.
func Suggest(evs []Event, imported map[string]bool, loc *time.Location) []Suggestion {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Start.Before(evs[j].Start) })
	seen := map[string]bool{}
	var out []Suggestion
	for _, e := range evs {
		key := strings.ToLower(strings.TrimSpace(e.Title))
		if imported[e.ID] || seen[key] {
			continue
		}
		c, ok := Classify(e.Title)
		if !ok {
			continue
		}
		seen[key] = true
		e.Start = e.Start.In(loc)
		out = append(out, Suggestion{Event: e, Category: c})
	}
	return out
}

// dateField is where the event date goes for each kind.
var dateField = map[string]string{
	"surgery": "date", "vaccine": "next", "exam": "date", "doctor": "next",
	"date": "date", "trip": "start", "course": "start", "document": "expires",
}

// Item builds the profile item for the suggestion.
func (s Suggestion) Item() Item {
	it := Item{Kind: s.Category.Kind, Title: s.Event.Title, Values: map[string]string{}}
	if k := KindOf(it.Kind); k != nil {
		it.Sensitive = k.Sensitive
	}
	it.Values[dateField[it.Kind]] = s.Event.Start.Format("2006-01-02")
	switch it.Kind {
	case "date":
		it.Values["yearly"] = "não"
		if s.Category.Label == "Aniversário" {
			it.Values["yearly"], it.Values["category"] = "sim", "aniversário"
		}
	case "course":
		if !s.Event.AllDay {
			it.Values["time"] = s.Event.Start.Format("15:04")
		}
	}
	return it
}
