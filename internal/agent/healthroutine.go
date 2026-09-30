package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Health × routine: how busy days (hours of meetings) relate to the sleep and recovery that
// follow. Sleep and recovery of the morning after are recorded by the wearable under the next day.

const (
	busyHours  = 4.0 // a day with at least this many hours of meetings is "cheio"
	lightHours = 1.5 // and one with at most this many is "leve"
	minGroup   = 3   // days needed in each group to compare
	minCorr    = 8   // days with data needed for a correlation
)

// routineMetrics are the next-morning measures compared with the day's load.
var routineMetrics = []struct{ Kind, Label string }{
	{"sleep_minutes", "sono"},
	{"sleep_score", "pontuação de sono"},
	{"recovery_score", "recuperação"},
	{"resting_hr", "FC em repouso"},
}

// DayLoad is the time spent in meetings on one day.
type DayLoad struct {
	Date     string // YYYY-MM-DD
	Hours    float64
	Meetings int
}

// GroupStats are the averages of the measures for a group of days.
type GroupStats struct {
	Days int
	Avg  map[string]float64 // kind → average of the following morning
}

// RoutineReport compares busy and light days.
type RoutineReport struct {
	Days        int // days with meeting data
	Busy, Light GroupStats
	Corr        map[string]float64 // Pearson r between the day's hours and the measure
	CorrN       map[string]int
}

// AnalyzeRoutine relates each day's meeting load to the measures dated the day after.
// metrics maps kind → date → value.
func AnalyzeRoutine(loads []DayLoad, metrics map[string]map[string]float64, loc *time.Location) RoutineReport {
	rep := RoutineReport{Days: len(loads), Corr: map[string]float64{}, CorrN: map[string]int{},
		Busy: GroupStats{Avg: map[string]float64{}}, Light: GroupStats{Avg: map[string]float64{}}}
	type acc struct{ sum, n float64 }
	busy, light := map[string]*acc{}, map[string]*acc{}
	for _, m := range routineMetrics {
		busy[m.Kind], light[m.Kind] = &acc{}, &acc{}
	}
	xs, ys := map[string][]float64{}, map[string][]float64{}
	for _, d := range loads {
		day, err := time.ParseInLocation("2006-01-02", d.Date, loc)
		if err != nil {
			continue
		}
		next := day.AddDate(0, 0, 1).Format("2006-01-02")
		isBusy, isLight := d.Hours >= busyHours, d.Hours <= lightHours
		if isBusy {
			rep.Busy.Days++
		}
		if isLight {
			rep.Light.Days++
		}
		for _, m := range routineMetrics {
			v, ok := metrics[m.Kind][next]
			if !ok {
				continue
			}
			xs[m.Kind], ys[m.Kind] = append(xs[m.Kind], d.Hours), append(ys[m.Kind], v)
			if isBusy {
				busy[m.Kind].sum, busy[m.Kind].n = busy[m.Kind].sum+v, busy[m.Kind].n+1
			}
			if isLight {
				light[m.Kind].sum, light[m.Kind].n = light[m.Kind].sum+v, light[m.Kind].n+1
			}
		}
	}
	for _, m := range routineMetrics {
		if a := busy[m.Kind]; a.n > 0 {
			rep.Busy.Avg[m.Kind] = a.sum / a.n
		}
		if a := light[m.Kind]; a.n > 0 {
			rep.Light.Avg[m.Kind] = a.sum / a.n
		}
		if len(xs[m.Kind]) >= minCorr {
			if r, ok := pearson(xs[m.Kind], ys[m.Kind]); ok {
				rep.Corr[m.Kind], rep.CorrN[m.Kind] = r, len(xs[m.Kind])
			}
		}
	}
	return rep
}

func pearson(x, y []float64) (float64, bool) {
	n := float64(len(x))
	var sx, sy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
	}
	mx, my := sx/n, sy/n
	var num, dx, dy float64
	for i := range x {
		num += (x[i] - mx) * (y[i] - my)
		dx += (x[i] - mx) * (x[i] - mx)
		dy += (y[i] - my) * (y[i] - my)
	}
	if dx == 0 || dy == 0 {
		return 0, false
	}
	return num / math.Sqrt(dx*dy), true
}

func corrWord(r float64) string {
	switch a := math.Abs(r); {
	case a >= 0.6:
		return "forte"
	case a >= 0.35:
		return "moderada"
	case a >= 0.15:
		return "fraca"
	}
	return "praticamente nenhuma"
}

func fmtMeasure(kind string, v float64) string {
	switch kind {
	case "sleep_minutes":
		return fmt.Sprintf("%dh%02d", int(v)/60, int(v)%60)
	case "resting_hr":
		return fmt.Sprintf("%.0f bpm", v)
	}
	return fmt.Sprintf("%.0f", v)
}

// Format renders the report in Portuguese; it is empty when there is nothing to say.
func (r RoutineReport) Format(days int) string {
	if r.Days == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🩺 *Saúde × rotina* (últimos %d dias, %d com agenda)\n", days, r.Days)
	if r.Busy.Days >= minGroup && r.Light.Days >= minGroup {
		row := func(name string, g GroupStats, cond string) {
			var parts []string
			for _, m := range routineMetrics {
				if v, ok := g.Avg[m.Kind]; ok {
					parts = append(parts, m.Label+" "+fmtMeasure(m.Kind, v))
				}
			}
			if len(parts) > 0 {
				fmt.Fprintf(&b, "• %s (%s, %d dias): %s\n", name, cond, g.Days, strings.Join(parts, " · "))
			}
		}
		row("Dias cheios", r.Busy, fmt.Sprintf("≥ %.0fh de reuniões", busyHours))
		row("Dias leves", r.Light, fmt.Sprintf("≤ %.1fh", lightHours))
		if bs, ok1 := r.Busy.Avg["sleep_minutes"]; ok1 {
			if ls, ok2 := r.Light.Avg["sleep_minutes"]; ok2 {
				diff := ls - bs
				switch {
				case diff >= 20:
					fmt.Fprintf(&b, "→ Depois de dias cheios você dorme em média %d min a menos.", int(diff))
					if br, ok := r.Busy.Avg["recovery_score"]; ok {
						if lr, ok := r.Light.Avg["recovery_score"]; ok && lr-br >= 5 {
							fmt.Fprintf(&b, " A recuperação cai %.0f pontos.", lr-br)
						}
					}
					b.WriteString("\n")
				case diff <= -20:
					fmt.Fprintf(&b, "→ Curiosamente você dorme %d min a mais depois de dias cheios.\n", int(-diff))
				default:
					b.WriteString("→ O sono quase não muda entre dias cheios e leves.\n")
				}
			}
		}
	} else {
		fmt.Fprintf(&b, "Poucos dias cheios (%d) ou leves (%d) para comparar; precisa de pelo menos %d de cada.\n", r.Busy.Days, r.Light.Days, minGroup)
	}
	kinds := make([]string, 0, len(r.Corr))
	for k := range r.Corr {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		for _, m := range routineMetrics {
			if m.Kind == k && (k == "sleep_minutes" || k == "recovery_score") {
				fmt.Fprintf(&b, "Correlação horas de reunião × %s: %+.2f (%s, %d dias)\n", m.Label, r.Corr[k], corrWord(r.Corr[k]), r.CorrN[k])
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// meetingLoads sums the timed events of each day in [from, to).
func meetingLoads(events []CalendarEvent, from, to time.Time, loc *time.Location) []DayLoad {
	byDay := map[string]*DayLoad{}
	for _, e := range events {
		if e.AllDay || !e.End.After(e.Start) {
			continue
		}
		// Split events that cross midnight.
		for cur := e.Start; cur.Before(e.End); {
			dayEnd := time.Date(cur.In(loc).Year(), cur.In(loc).Month(), cur.In(loc).Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			end := e.End
			if end.After(dayEnd) {
				end = dayEnd
			}
			key := cur.In(loc).Format("2006-01-02")
			if cur.Before(from) || !cur.Before(to) {
				cur = end
				continue
			}
			d := byDay[key]
			if d == nil {
				d = &DayLoad{Date: key}
				byDay[key] = d
			}
			d.Hours += math.Min(end.Sub(cur).Hours(), 12)
			if cur.Equal(e.Start) {
				d.Meetings++
			}
			cur = end
		}
	}
	var out []DayLoad
	// Days without events count as free days (0 hours) when the calendar had any data at all.
	if len(byDay) > 0 {
		for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
			key := d.In(loc).Format("2006-01-02")
			if l, ok := byDay[key]; ok {
				out = append(out, *l)
			} else {
				out = append(out, DayLoad{Date: key})
			}
		}
	}
	return out
}

// HealthRoutine analyses the last days of agenda against the wearable's sleep and recovery.
func (a *Agent) HealthRoutine(ctx context.Context, days int) (RoutineReport, string, error) {
	if days <= 0 || days > 180 {
		days = 30
	}
	loc := a.cfg.Location()
	now := time.Now().In(loc)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from := to.AddDate(0, 0, -days)
	events, err := a.EventsBetween(ctx, from, to)
	if err != nil {
		return RoutineReport{}, "", err
	}
	ms, err := a.db.MetricsRange(ctx, from.Format("2006-01-02"), to.AddDate(0, 0, 1).Format("2006-01-02"))
	if err != nil {
		return RoutineReport{}, "", err
	}
	metrics := map[string]map[string]float64{}
	for _, m := range ms {
		if metrics[m.Kind] == nil {
			metrics[m.Kind] = map[string]float64{}
		}
		metrics[m.Kind][m.Date] = m.Value
	}
	rep := AnalyzeRoutine(meetingLoads(events, from, to, loc), metrics, loc)
	return rep, rep.Format(days), nil
}
