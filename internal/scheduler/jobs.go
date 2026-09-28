package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/integrations/google"
	"github.com/inakano89/second-brain/internal/integrations/rss"
	"github.com/inakano89/second-brain/internal/integrations/zepp"
)

// Notifier delivers messages (Telegram).
type Notifier interface {
	Notify(ctx context.Context, text string) error
	SendDocument(ctx context.Context, chatID int64, name string, r io.Reader, caption string) error
	AllowedIDs() []int64
}

// Deps bundles what the routines need.
type Deps struct {
	Cfg       *config.Config
	DB        *database.DB
	Agent     *agent.Agent
	Notifier  Notifier
	PurgeTemp func(age time.Duration) int
	Update    JobFunc // self-update check/install (optional)
	Models    JobFunc // curated model catalogue sync (optional)
	Log       *slog.Logger
}

// Register adds every routine according to CRON_* settings.
func Register(s *Scheduler, d *Deps) error {
	enqueue := func(kind string) JobFunc {
		return func(ctx context.Context) error {
			_, err := d.DB.Enqueue(ctx, kind, nil, database.EnqueueOpts{DedupeKey: kind, MaxAttempts: 4})
			return err
		}
	}
	jobs := []struct {
		name, key string
		fn        JobFunc
	}{
		{"morning", "CRON_MORNING", func(ctx context.Context) error { _, err := d.MorningBriefing(ctx, true); return err }},
		{"evening", "CRON_EVENING", func(ctx context.Context) error { _, err := d.EveningReview(ctx, true); return err }},
		{"weekly", "CRON_WEEKLY", func(ctx context.Context) error { _, err := d.WeeklyReview(ctx, true); return err }},
		{"maintenance", "CRON_MAINTENANCE", d.Maintenance},
		{"backup", "CRON_BACKUP", func(ctx context.Context) error { _, err := d.Backup(ctx); return err }},
		{"rss", "CRON_RSS", enqueue(rss.TaskPoll)},
		{"gmail", "CRON_GMAIL", enqueue(google.TaskGmailSync)},
		{"calendar", "CRON_CALENDAR", enqueue(google.TaskCalendarSync)},
		{"drive", "CRON_DRIVE", enqueue(google.TaskDriveSync)},
		{"contacts", "CRON_CONTACTS", enqueue(google.TaskContactsSync)},
		{"google-tasks", "CRON_GOOGLE_TASKS", enqueue(google.TaskTasksSync)},
		{"youtube", "CRON_YOUTUBE", enqueue(google.TaskYouTubeSync)},
		{"takeout", "CRON_TAKEOUT", enqueue(google.TaskTakeoutSync)},
		{"zepp", "CRON_ZEPP", enqueue(zepp.TaskSync)},
	}
	if d.Update != nil {
		jobs = append(jobs, struct {
			name, key string
			fn        JobFunc
		}{"update", "CRON_UPDATE", d.Update})
	}
	if d.Models != nil {
		jobs = append(jobs, struct {
			name, key string
			fn        JobFunc
		}{"models", "CRON_MODELS", d.Models})
	}
	var errs []error
	for _, j := range jobs {
		if err := s.Add(j.name, d.Cfg.Get(j.key), j.fn); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func dayBounds(loc *time.Location, t time.Time) (time.Time, time.Time) {
	t = t.In(loc)
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

func fmtMinutes(v float64) string { return fmt.Sprintf("%dh%02d", int(v)/60, int(v)%60) }

func (d *Deps) saveInsight(ctx context.Context, title, ref, content string, tags []string) (*database.Node, error) {
	n, _, err := d.Agent.Ingest(ctx, agent.IngestInput{
		Type: database.TypeInsight, Title: title, Content: content, Summary: extract.Truncate(content, 280),
		Source: "routine", SourceRef: ref, Tags: tags, Meta: map[string]any{"enriched": true}, Enrich: true,
	})
	if err == nil {
		_ = d.DB.KVSet(ctx, "routine.latest."+strings.SplitN(ref, ":", 2)[0], fmt.Sprint(n.ID))
	}
	return n, err
}

func (d *Deps) compose(ctx context.Context, purpose, system, data, fallback string) string {
	if !d.Agent.LLM().Enabled() {
		return fallback
	}
	text, err := d.Agent.Ask(ctx, purpose, system, data)
	if err != nil || strings.TrimSpace(text) == "" {
		d.Log.Warn("LLM indisponível para rotina, usando template", "routine", purpose, "err", err)
		return fallback + "\n\n_(gerado sem LLM)_"
	}
	return text
}

func (d *Deps) notify(ctx context.Context, text string) {
	if d.Notifier == nil {
		return
	}
	if err := d.Notifier.Notify(ctx, text); err != nil {
		d.Log.Warn("notificação falhou (reenfileirada)", "err", err)
	}
}

func taskLine(n database.Node, loc *time.Location) string {
	due := ""
	if n.DueAt != nil {
		due = " (prazo " + n.DueAt.In(loc).Format("02/01") + ")"
	}
	return fmt.Sprintf("- #%d %s%s", n.ID, n.Title, due)
}

// MorningBriefing crosses sleep quality, today's agenda and pending tasks.
func (d *Deps) MorningBriefing(ctx context.Context, notify bool) (string, error) {
	loc := d.Cfg.Location()
	now := time.Now().In(loc)
	start, end := dayBounds(loc, now)
	var (
		wg      sync.WaitGroup
		metrics []database.Metric
		events  []agent.CalendarEvent
		tasks   []database.Node
		recent  []database.Node
		evErr   error
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		metrics, _ = d.DB.MetricsRange(ctx, start.AddDate(0, 0, -1).Format("2006-01-02"), start.Format("2006-01-02"))
	}()
	go func() { defer wg.Done(); events, evErr = d.Agent.EventsBetween(ctx, start, end) }()
	go func() { defer wg.Done(); tasks, _ = d.DB.OpenTasks(ctx, 25) }()
	go func() {
		defer wg.Done()
		from := now.Add(-24 * time.Hour)
		recent, _ = d.DB.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeInsight, database.TypeNote, database.TypeArticle}, From: &from, Limit: 15})
	}()
	wg.Wait()

	var data strings.Builder
	fmt.Fprintf(&data, "Data: %s\n\n## Saúde (última noite)\n", now.Format("Monday, 02/01/2006"))
	latest := map[string]database.Metric{}
	for _, m := range metrics { // sorted date DESC → first wins
		if _, ok := latest[m.Kind]; !ok {
			latest[m.Kind] = m
		}
	}
	if len(latest) == 0 {
		data.WriteString("sem dados\n")
	}
	kinds := make([]string, 0, len(latest))
	for k := range latest {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		m := latest[k]
		if strings.HasSuffix(k, "_minutes") {
			fmt.Fprintf(&data, "- %s: %s\n", k, fmtMinutes(m.Value))
		} else {
			fmt.Fprintf(&data, "- %s: %.0f %s\n", k, m.Value, m.Unit)
		}
	}
	data.WriteString("\n## Agenda de hoje\n")
	if evErr != nil {
		data.WriteString("(agenda indisponível)\n")
	}
	if len(events) == 0 {
		data.WriteString("sem eventos\n")
	}
	for _, e := range events {
		when := e.Start.In(loc).Format("15:04") + "–" + e.End.In(loc).Format("15:04")
		if e.AllDay {
			when = "dia inteiro"
		}
		fmt.Fprintf(&data, "- %s %s", when, e.Summary)
		if e.Location != "" {
			fmt.Fprintf(&data, " @ %s", e.Location)
		}
		data.WriteString("\n")
	}
	var overdue, today, other []string
	for _, t := range tasks {
		switch {
		case t.DueAt != nil && t.DueAt.Before(start):
			overdue = append(overdue, taskLine(t, loc))
		case t.DueAt != nil && t.DueAt.Before(end):
			today = append(today, taskLine(t, loc))
		default:
			other = append(other, taskLine(t, loc))
		}
	}
	fmt.Fprintf(&data, "\n## Tarefas atrasadas (%d)\n%s\n## Tarefas de hoje (%d)\n%s\n## Outras pendências\n%s\n",
		len(overdue), strings.Join(overdue, "\n"), len(today), strings.Join(today, "\n"), strings.Join(other[:min(len(other), 10)], "\n"))
	if len(recent) > 0 {
		data.WriteString("\n## Capturas recentes (24h)\n")
		for _, n := range recent {
			fmt.Fprintf(&data, "- [%s] %s\n", n.Type, n.Title)
		}
	}

	text := d.compose(ctx, "briefing",
		`Você é o chief-of-staff pessoal do usuário. Gere um BRIEFING MATINAL acionável em português, conciso (máx. 220 palavras), com seções:
🛌 Recuperação (interprete sono/recuperação e recomende intensidade do dia), 📅 Agenda, ✅ Top 3 prioridades (com IDs), ⚠️ Riscos/atrasos, 💡 Uma sugestão.
Use apenas os dados fornecidos. Formatação compatível com Telegram Markdown simples (*negrito*, listas com -).`,
		data.String(), "☀️ *Briefing matinal*\n\n"+data.String())
	title := "Briefing matinal " + now.Format("02/01/2006")
	if _, err := d.saveInsight(ctx, title, "morning:"+now.Format("2006-01-02"), text, []string{"briefing"}); err != nil {
		return text, err
	}
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}

// EveningReview summarises what was completed and the system state.
func (d *Deps) EveningReview(ctx context.Context, notify bool) (string, error) {
	loc := d.Cfg.Location()
	now := time.Now().In(loc)
	start, _ := dayBounds(loc, now)
	done, _ := d.DB.UpdatedBetween(ctx, []string{database.TypeTask}, database.StatusDone, start, now.Add(time.Minute), 50)
	created, _ := d.DB.ListNodes(ctx, database.NodeFilter{From: &start, Limit: 200})
	open, _ := d.DB.OpenTasks(ctx, 100)
	errCount, _ := d.DB.CountLogs(ctx, "ERROR", start)
	qs, _ := d.DB.QueueStats(ctx)
	usage, _ := d.DB.UsageSummary(ctx, start)
	var cost float64
	for _, u := range usage {
		cost += u.CostUSD
	}
	byType := map[string]int{}
	for _, n := range created {
		byType[n.Type]++
	}
	var data strings.Builder
	fmt.Fprintf(&data, "Data: %s\n\n## Tarefas concluídas hoje (%d)\n", now.Format("02/01/2006"), len(done))
	for _, t := range done {
		data.WriteString(taskLine(t, loc) + "\n")
	}
	overdue := 0
	for _, t := range open {
		if t.DueAt != nil && t.DueAt.Before(now) {
			overdue++
		}
	}
	fmt.Fprintf(&data, "\n## Capturas de hoje\n%v\n\n## Pendências\nabertas=%d atrasadas=%d\n\n## Sistema\nerros=%d fila_pendente=%d fila_falhas=%d custo_llm_hoje=US$%.4f\n",
		byType, len(open), overdue, errCount, qs[database.TaskPending], qs[database.TaskFailed], cost)
	text := d.compose(ctx, "evening",
		`Gere um BALANÇO NOTURNO curto (máx. 180 palavras) em português: 🏁 conquistas do dia, 📥 o que foi capturado, 🔜 3 focos sugeridos para amanhã (com IDs de tarefas quando houver), 🩺 estado do sistema em uma linha. Telegram Markdown simples.`,
		data.String(), "🌙 *Balanço do dia*\n\n"+data.String())
	if _, err := d.saveInsight(ctx, "Balanço "+now.Format("02/01/2006"), "evening:"+now.Format("2006-01-02"), text, []string{"review", "diario"}); err != nil {
		return text, err
	}
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}

// WeeklyReview audits orphans, broken links and accumulated pending items.
func (d *Deps) WeeklyReview(ctx context.Context, notify bool) (string, error) {
	loc := d.Cfg.Location()
	now := time.Now().In(loc)
	weekAgo := now.AddDate(0, 0, -7)
	var (
		wg      sync.WaitGroup
		orphans []database.Node
		broken  []string
		open    []database.Node
		created []database.Node
		usage   []database.UsageRow
		qs      map[string]int
	)
	wg.Add(5)
	go func() {
		defer wg.Done()
		orphans, _ = d.DB.OrphanNodes(ctx, []string{database.TypeHealth, database.TypePerson}, 100)
	}()
	go func() { defer wg.Done(); broken = d.brokenLinks(ctx) }()
	go func() { defer wg.Done(); open, _ = d.DB.OpenTasks(ctx, 500) }()
	go func() {
		defer wg.Done()
		created, _ = d.DB.ListNodes(ctx, database.NodeFilter{From: &weekAgo, Limit: 2000})
	}()
	go func() {
		defer wg.Done()
		usage, _ = d.DB.UsageSummary(ctx, weekAgo)
		qs, _ = d.DB.QueueStats(ctx)
	}()
	wg.Wait()

	// Self-healing: re-run linking for orphans and embed anything missing.
	for i, o := range orphans {
		if i >= 30 {
			break
		}
		_ = d.Agent.QueueEnrich(ctx, o.ID)
	}
	_, _ = d.DB.Enqueue(ctx, agent.TaskReembed, nil, database.EnqueueOpts{DedupeKey: agent.TaskReembed})

	var stale, overdue []string
	for _, t := range open {
		if t.DueAt != nil && t.DueAt.Before(now) {
			overdue = append(overdue, taskLine(t, loc))
		} else if now.Sub(t.CreatedAt) > 14*24*time.Hour {
			stale = append(stale, taskLine(t, loc))
		}
	}
	var cost float64
	for _, u := range usage {
		cost += u.CostUSD
	}
	var data strings.Builder
	fmt.Fprintf(&data, "Semana até %s\n\n## Crescimento\n%d nós novos\n\n## Nós órfãos (%d)\n", now.Format("02/01/2006"), len(created), len(orphans))
	for i, o := range orphans {
		if i >= 15 {
			fmt.Fprintf(&data, "… e mais %d\n", len(orphans)-15)
			break
		}
		fmt.Fprintf(&data, "- #%d [%s] %s\n", o.ID, o.Type, o.Title)
	}
	fmt.Fprintf(&data, "\n## Links quebrados (%d)\n%s\n", len(broken), strings.Join(broken[:min(len(broken), 15)], "\n"))
	fmt.Fprintf(&data, "\n## Tarefas atrasadas (%d)\n%s\n", len(overdue), strings.Join(overdue[:min(len(overdue), 15)], "\n"))
	fmt.Fprintf(&data, "\n## Tarefas paradas >14 dias (%d)\n%s\n", len(stale), strings.Join(stale[:min(len(stale), 15)], "\n"))
	fmt.Fprintf(&data, "\n## Sistema\nfila_falhas=%d custo_llm_7d=US$%.4f tamanho_db=%.1fMB\n", qs[database.TaskFailed], cost, float64(d.DB.Size())/1e6)
	text := d.compose(ctx, "weekly",
		`Gere um RELATÓRIO DE MANUTENÇÃO SEMANAL do Second Brain em português (máx. 300 palavras): 📈 resumo da semana, 🧹 higiene do grafo (órfãos, links quebrados — sugira ações concretas), ⏳ pendências acumuladas (sugira o que delegar, reagendar ou descartar), 🎯 3 intenções para a próxima semana. Telegram Markdown simples.`,
		data.String(), "🗓️ *Weekly Review*\n\n"+data.String())
	year, week := now.ISOWeek()
	if _, err := d.saveInsight(ctx, fmt.Sprintf("Weekly Review %d-W%02d", year, week), fmt.Sprintf("weekly:%d-W%02d", year, week), text, []string{"review", "semanal"}); err != nil {
		return text, err
	}
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}

func (d *Deps) brokenLinks(ctx context.Context) []string {
	titles, err := d.DB.AllTitles(ctx)
	if err != nil {
		return nil
	}
	var out []string
	_ = d.DB.IterateNodes(ctx, func(n *database.Node) error {
		for _, t := range agent.WikiLinks(n.Content) {
			if _, ok := titles[strings.ToLower(t)]; !ok {
				out = append(out, fmt.Sprintf("- #%d %s → [[%s]]", n.ID, n.Title, t))
			}
		}
		return nil
	})
	return out
}

// Maintenance purges temp files, old tasks/logs and vacuums SQLite.
func (d *Deps) Maintenance(ctx context.Context) error {
	purged := 0
	if d.PurgeTemp != nil {
		purged = d.PurgeTemp(24 * time.Hour)
	}
	tasks, _ := d.DB.PurgeTasks(ctx, time.Now().AddDate(0, 0, -7))
	logs, _ := d.DB.PurgeLogs(ctx, time.Now().AddDate(0, 0, -d.Cfg.GetInt("LOG_RETENTION_DAYS", 90)))
	_ = d.DB.PurgeSeen(ctx, time.Now().AddDate(0, 0, -120))
	full := time.Now().In(d.Cfg.Location()).Weekday() == time.Sunday
	if err := d.DB.Maintenance(ctx, full); err != nil {
		return err
	}
	d.Log.Info("manutenção concluída", "temp_files", purged, "tasks", tasks, "logs", logs, "vacuum_full", full, "db_mb", float64(d.DB.Size())/1e6)
	return nil
}
