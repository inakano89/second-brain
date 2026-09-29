package scheduler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/finance"
)

// Review sends the day's spaced-repetition items (Kindle highlights, insights and learnings).
func (d *Deps) Review(ctx context.Context, notify bool) (string, error) {
	per := d.Cfg.GetInt("REVIEW_PER_DAY", 2)
	if per <= 0 {
		return "", nil
	}
	items, err := d.Agent.ReviewDue(ctx, time.Now(), min(per, 10))
	if err != nil {
		return "", err
	}
	text := agent.FormatReviews(items)
	if text != "" && notify {
		d.notify(ctx, text)
	}
	return text, nil
}

// Diary opens tonight's guided diary and sends its questions (once per day).
func (d *Deps) Diary(ctx context.Context, notify bool) (string, error) {
	if d.Notifier == nil {
		return "", nil
	}
	now := time.Now()
	day := now.In(d.Cfg.Location()).Format("2006-01-02")
	if _, err := d.DB.GetNodeBySource(ctx, "diary", "diary:"+day); err == nil {
		return "", nil // already written today
	}
	text, err := d.Agent.StartDiary(ctx, now)
	if err != nil {
		return "", err
	}
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}

// YearReview writes the retrospective of the previous year (once) and sends it.
func (d *Deps) YearReview(ctx context.Context, notify bool) (string, error) {
	year := time.Now().In(d.Cfg.Location()).Year() - 1
	if _, err := d.DB.GetNodeBySource(ctx, "routine", fmt.Sprintf("year:%d", year)); err == nil {
		return "", nil
	}
	n, text, err := d.Agent.YearReview(ctx, year)
	if err != nil {
		if n == nil && strings.Contains(err.Error(), "não há conteúdo") {
			d.Log.Info("retrospectiva: nada a resumir", "year", year)
			return "", nil
		}
		return "", err
	}
	msg := fmt.Sprintf("🎆 *Retrospectiva %d*\n\n%s\n\nCompleta em: %s", year, extract.Truncate(text, 3200), d.link(fmt.Sprintf("/?focus=%d", n.ID)))
	if notify {
		d.notify(ctx, msg)
	}
	return msg, nil
}

// goalsBlock lists the goals of the profile with their progress and the tasks that mention them.
// Like every personal block it goes only to the user's chat, never to a model or a note.
func (d *Deps) goalsBlock(ctx context.Context) string {
	items, err := d.Agent.Profile().List(ctx, false)
	if err != nil {
		return ""
	}
	loc := d.Cfg.Location()
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	weekAgo := now.AddDate(0, 0, -7)
	var lines []string
	for _, it := range items {
		if it.Kind != "goal" || it.Locked {
			continue
		}
		line := "• *" + strings.NewReplacer("*", "", "_", " ").Replace(it.Title) + "*"
		progress, hasProgress := it.Num("progress")
		if hasProgress {
			line += fmt.Sprintf(" — %.0f%%", progress)
			if progress >= 100 {
				lines = append(lines, line+" ✅")
				continue
			}
		}
		if dl, ok := it.Date("deadline", loc); ok {
			days := int(dl.Sub(today).Hours() / 24)
			switch {
			case days < 0:
				line += fmt.Sprintf(" · ⚠️ prazo venceu há %d dias", -days)
			case days <= 30 && (!hasProgress || progress < 50):
				line += fmt.Sprintf(" · ⚠️ faltam %d dias e o progresso está baixo", days)
			default:
				line += fmt.Sprintf(" · prazo em %d dias", days)
			}
		}
		open, done, doneWeek := d.goalTasks(ctx, it.Title, weekAgo)
		switch {
		case open+done == 0:
			line += " · sem tarefas ligadas"
		default:
			line += fmt.Sprintf(" · tarefas: %d abertas, %d concluídas (%d na semana)", open, done, doneWeek)
		}
		if next := it.Get("next"); next != "" {
			line += "\n   ↳ próximo passo: " + extract.Truncate(next, 100)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "🎯 *Metas*\n" + strings.Join(lines, "\n")
}

// goalTasks counts the tasks that share at least two of the goal's main words (one, for a goal
// with a single main word): open, done and done since weekAgo.
func (d *Deps) goalTasks(ctx context.Context, title string, weekAgo time.Time) (open, done, doneWeek int) {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var keys []string
	for _, w := range words {
		if len([]rune(w)) >= 4 {
			keys = append(keys, w)
		}
	}
	if len(keys) == 0 {
		return
	}
	need := min(2, len(keys))
	hits, err := d.DB.SearchFTS(ctx, strings.Join(keys, " "), database.NodeFilter{Types: []string{database.TypeTask}}, 300)
	if err != nil {
		return
	}
	for _, h := range hits {
		text := strings.ToLower(h.Title + " " + h.Content)
		n := 0
		for _, k := range keys {
			if strings.Contains(text, k) {
				n++
			}
		}
		if n < need {
			continue
		}
		switch h.Status {
		case database.StatusDone:
			done++
			if h.UpdatedAt.After(weekAgo) {
				doneWeek++
			}
		default:
			open++
		}
	}
	return
}

// CRM tells you who you have not talked to in a while (each person once per stretch of silence).
func (d *Deps) CRM(ctx context.Context, notify bool) (string, error) {
	now := time.Now()
	list, err := d.Agent.CRMReminders(ctx, now, 6)
	if err != nil {
		return "", err
	}
	text := agent.FormatStaleContacts(list, d.Cfg.Location(), now)
	if text != "" && notify {
		d.notify(ctx, text)
	}
	return text, nil
}

// MeetingPrep sends, shortly before each meeting with people you know, a brief of each of them.
func (d *Deps) MeetingPrep(ctx context.Context) error {
	lead := d.Cfg.GetInt("MEETING_PREP_MINUTES", 45)
	if lead <= 0 {
		return nil
	}
	briefs, err := d.Agent.MeetingPrep(ctx, time.Now(), time.Duration(lead)*time.Minute)
	if err != nil {
		return err
	}
	for _, b := range briefs {
		d.notify(ctx, b)
	}
	return nil
}

// Travel sends the dossier of each trip of the profile that starts within a week (once per trip).
// It goes only to the user's chat: it carries documents and medication from the profile.
func (d *Deps) Travel(ctx context.Context, notify bool) (string, error) {
	now := time.Now()
	var sent []string
	for _, it := range d.Agent.UpcomingTrips(ctx, now, 7) {
		start, _ := it.Date("start", d.Cfg.Location())
		if fresh, err := d.DB.MarkSeen(ctx, "travel", fmt.Sprintf("%d:%s", it.ID, start.Format("2006-01-02"))); err != nil || !fresh {
			continue
		}
		text, err := d.Agent.TravelDossier(ctx, agent.TravelOptions{Destination: it.Title, Personal: true, SensitiveOK: true})
		if err != nil {
			d.Log.Warn("dossiê de viagem falhou", "trip", it.ID, "err", err)
			continue
		}
		if notify {
			d.notify(ctx, text)
		}
		sent = append(sent, text)
	}
	return strings.Join(sent, "\n\n"), nil
}

// Finance sends the summary of the previous month (once per month) from the imported statements.
func (d *Deps) Finance(ctx context.Context, notify bool) (string, error) {
	prev := finance.ShiftMonth(time.Now().In(d.Cfg.Location()).Format("2006-01"), -1)
	if months, _ := d.DB.TransactionMonths(ctx); !slices.Contains(months, prev) {
		return "", nil // no statement for that month (yet)
	}
	if fresh, err := d.DB.MarkSeen(ctx, "finance", prev); err != nil || !fresh {
		return "", err
	}
	text, err := d.Agent.FinanceDigest(ctx, prev)
	if err != nil || text == "" {
		return "", err
	}
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}
