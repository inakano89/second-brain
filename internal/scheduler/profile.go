package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/profile"
)

// personalBlock is appended to the morning and evening reports. It is built here, from
// the profile, and never sent to an AI model: only to the user's own Telegram chat.
func (d *Deps) personalBlock(ctx context.Context, tomorrow bool) string {
	o, err := d.Agent.ProfileOverview(ctx, time.Now(), 7)
	if err != nil {
		d.Log.Warn("perfil indisponível para o relatório", "err", err)
		return ""
	}
	if tomorrow {
		alerts := o.Within(7)
		var soon []profile.Alert
		for _, a := range alerts {
			if a.Days >= 1 || a.Level == profile.LevelLate {
				soon = append(soon, a)
			}
		}
		return profile.Digest("📋 *Amanhã e próximos 7 dias*", o.Tomorrow, soon)
	}
	return profile.Digest("📋 *Hoje na sua rotina*", o.Today, o.Within(7))
}

func withBlock(text, block string) string {
	if block == "" {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n\n" + block
}

// Reminders notifies doses, habits and classes whose time arrived since the last run.
func (d *Deps) Reminders(ctx context.Context) error {
	if !d.Cfg.GetBool("REMINDERS_ENABLED") || d.Notifier == nil {
		return nil
	}
	loc := d.Cfg.Location()
	now := time.Now().In(loc)
	last := d.cursor(ctx, "reminders.cursor", now)
	if now.Sub(last) > 2*time.Hour { // after downtime, only recent reminders still matter
		last = now.Add(-2 * time.Hour)
	}
	store := d.Agent.Profile()
	items, err := store.List(ctx, false)
	if err != nil {
		return err
	}
	type due struct {
		at   time.Time
		line string
	}
	var list []due
	before := time.Duration(d.Cfg.GetInt("REMINDERS_CLASS_BEFORE", 60)) * time.Minute
	for _, day := range []time.Time{last, now} { // a window can cross midnight
		start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
		done, _ := store.Checks(ctx, start, start)
		for _, s := range profile.Today(items, start, done) {
			if s.Time == "" || s.Done {
				continue
			}
			t, err := time.ParseInLocation("2006-01-02 15:04", profile.DayKey(start)+" "+s.Time, loc)
			if err != nil {
				continue
			}
			at := t
			line := fmt.Sprintf("%s %s", s.Icon, s.Title)
			if s.Detail != "" {
				line += " · " + s.Detail
			}
			if s.Check {
				line += fmt.Sprintf(" → /tomei %d", s.ItemID)
			} else {
				at = t.Add(-before)
				line += " às " + s.Time
			}
			if at.After(last) && !at.After(now) {
				list = append(list, due{at, line})
			}
		}
		if profile.DayKey(last) == profile.DayKey(now) {
			break
		}
	}
	_ = d.DB.KVSet(ctx, "reminders.cursor", now.UTC().Format(time.RFC3339Nano))
	if len(list) == 0 {
		return nil
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	var b strings.Builder
	b.WriteString("⏰ *Lembrete*\n")
	for _, x := range list {
		b.WriteString(x.line + "\n")
	}
	d.notify(ctx, strings.TrimRight(b.String(), "\n"))
	return nil
}
