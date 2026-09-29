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
	type entry struct {
		name, key string
		fn        JobFunc
		opts      []JobOption
	}
	// Missed runs (PC off, program closed, sleep) are caught up at start; reports that
	// only make sense near their time have a window.
	jobs := []entry{
		{"morning", "CRON_MORNING", func(ctx context.Context) error { _, err := d.MorningBriefing(ctx, true); return err }, []JobOption{CatchUpWithin(5 * time.Hour)}},
		{"evening", "CRON_EVENING", func(ctx context.Context) error { _, err := d.EveningReview(ctx, true); return err }, []JobOption{CatchUpWithin(3 * time.Hour)}},
		{"weekly", "CRON_WEEKLY", func(ctx context.Context) error { _, err := d.WeeklyReview(ctx, true); return err }, []JobOption{CatchUpWithin(72 * time.Hour)}},
		{"actions", "CRON_ACTIONS", func(ctx context.Context) error { _, err := d.ActionItems(ctx, true); return err }, nil},
		{"memory", "CRON_MEMORY", func(ctx context.Context) error { _, err := d.MemoryRun(ctx, true); return err }, nil},
		{"cleanup", "CRON_CLEANUP", func(ctx context.Context) error { _, err := d.Cleanup(ctx, true); return err }, []JobOption{CatchUpWithin(72 * time.Hour)}},
		{"review", "CRON_REVIEW", func(ctx context.Context) error { _, err := d.Review(ctx, true); return err }, []JobOption{CatchUpWithin(5 * time.Hour)}},
		{"diary", "CRON_DIARY", func(ctx context.Context) error { _, err := d.Diary(ctx, true); return err }, []JobOption{CatchUpWithin(2 * time.Hour)}},
		{"year-review", "CRON_YEAR_REVIEW", func(ctx context.Context) error { _, err := d.YearReview(ctx, true); return err }, []JobOption{CatchUpWithin(45 * 24 * time.Hour)}},
		{"reminders", "CRON_REMINDERS", d.Reminders, nil},
		{"maintenance", "CRON_MAINTENANCE", d.Maintenance, nil},
		{"backup", "CRON_BACKUP", func(ctx context.Context) error { _, err := d.Backup(ctx); return err }, nil},
		{"rss", "CRON_RSS", enqueue(rss.TaskPoll), nil},
		{"gmail", "CRON_GMAIL", enqueue(google.TaskGmailSync), nil},
		{"calendar", "CRON_CALENDAR", enqueue(google.TaskCalendarSync), nil},
		{"drive", "CRON_DRIVE", enqueue(google.TaskDriveSync), nil},
		{"contacts", "CRON_CONTACTS", enqueue(google.TaskContactsSync), nil},
		{"google-tasks", "CRON_GOOGLE_TASKS", enqueue(google.TaskTasksSync), nil},
		{"youtube", "CRON_YOUTUBE", enqueue(google.TaskYouTubeSync), nil},
		{"takeout", "CRON_TAKEOUT", enqueue(google.TaskTakeoutSync), nil},
		{"zepp", "CRON_ZEPP", enqueue(zepp.TaskSync), nil},
	}
	if d.Update != nil {
		jobs = append(jobs, entry{"update", "CRON_UPDATE", d.Update, nil})
	}
	if d.Models != nil {
		jobs = append(jobs, entry{"models", "CRON_MODELS", d.Models, nil})
	}
	var errs []error
	for _, j := range jobs {
		if err := s.Add(j.name, d.Cfg.Get(j.key), j.fn, j.opts...); err != nil {
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
	if errors.Is(err, database.ErrDeleted) { // the user deleted today's note: keep it deleted
		return nil, nil
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
	var (
		memory  agent.MemoryView
		aiTasks []agent.TaskOrigin
		onDay   []agent.DayMemory
	)
	wg.Add(7)
	go func() { defer wg.Done(); onDay, _ = d.Agent.OnThisDay(ctx, now) }()
	go func() { defer wg.Done(); memory, _ = d.Agent.Memory(ctx, 5) }()
	go func() { defer wg.Done(); aiTasks, _ = d.Agent.NewAITasks(ctx, now.Add(-24*time.Hour), 15) }()
	go func() {
		defer wg.Done()
		metrics, _ = d.DB.MetricsRange(ctx, start.AddDate(0, 0, -1).Format("2006-01-02"), start.Format("2006-01-02"))
	}()
	go func() { defer wg.Done(); events, evErr = d.Agent.EventsBetween(ctx, start, end) }()
	go func() { defer wg.Done(); tasks, _ = d.DB.OpenTasks(ctx, 25) }()
	go func() {
		defer wg.Done()
		from := now.Add(-24 * time.Hour)
		recent, _ = d.DB.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeInsight, database.TypeNote, database.TypeArticle}, From: &from, KnownDate: true, Limit: 15})
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
	if memory.Priorities != nil {
		data.WriteString("\n## Prioridades atuais (memória)\n" + extract.Truncate(memory.Priorities.Content, 1500) + "\n")
	}
	if len(aiTasks) > 0 {
		data.WriteString("\n## Tarefas novas criadas pela IA (24h, de reuniões e e-mails)\n")
		for _, t := range aiTasks {
			line := taskLine(t.Task, loc)
			if t.Origin != nil {
				line += " ← " + t.Origin.Title
			}
			data.WriteString(line + "\n")
		}
	}
	if len(recent) > 0 {
		data.WriteString("\n## Capturas recentes (24h)\n")
		for _, n := range recent {
			fmt.Fprintf(&data, "- [%s] %s\n", n.Type, n.Title)
		}
	}
	if len(onDay) > 0 {
		data.WriteString("\n## Neste dia, em anos anteriores\n")
		for _, m := range onDay {
			for _, n := range m.Nodes {
				fmt.Fprintf(&data, "- %d ano(s) atrás (%d): [%s] %s\n", m.YearsAgo, m.Date.Year(), n.Type, n.Title)
			}
		}
	}

	text := d.compose(ctx, "briefing",
		`Você é o chief-of-staff pessoal do usuário. Gere um BRIEFING MATINAL acionável em português, conciso (máx. 220 palavras), com seções:
🛌 Recuperação (interprete sono/recuperação e recomende intensidade do dia), 📅 Agenda, ✅ Top 3 prioridades (com IDs; alinhe às prioridades atuais da memória quando houver), 📝 Tarefas novas da IA (se houver), 🕰️ Neste dia (uma linha sobre o que aconteceu em anos anteriores, se houver), ⚠️ Riscos/atrasos, 💡 Uma sugestão.
Use apenas os dados fornecidos. Formatação compatível com Telegram Markdown simples (*negrito*, listas com -).`,
		data.String(), "☀️ *Briefing matinal*\n\n"+data.String())
	title := "Briefing matinal " + now.Format("02/01/2006")
	if _, err := d.saveInsight(ctx, title, "morning:"+now.Format("2006-01-02"), text, []string{"briefing"}); err != nil {
		return text, err
	}
	text = withBlock(text, d.personalBlock(ctx, false)) // not saved: the note is readable by the AI
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
	created, _ := d.DB.ListNodes(ctx, database.NodeFilter{From: &start, KnownDate: true, Limit: 200}) // undated imports are not captures
	imported, _ := d.DB.CountImported(ctx, start, start.AddDate(0, 0, 1))
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
	if imported > 0 {
		fmt.Fprintf(&data, "\n## Importados hoje\n%d itens vieram de importações (não são capturas novas)\n", imported)
	}
	fmt.Fprintf(&data, "\n## Capturas de hoje\n%v\n\n## Pendências\nabertas=%d atrasadas=%d\n\n## Sistema\nerros=%d fila_pendente=%d fila_falhas=%d custo_llm_hoje=US$%.4f\n",
		byType, len(open), overdue, errCount, qs[database.TaskPending], qs[database.TaskFailed], cost)
	text := d.compose(ctx, "evening",
		`Gere um BALANÇO NOTURNO curto (máx. 180 palavras) em português: 🏁 conquistas do dia, 📥 o que foi capturado, 🔜 3 focos sugeridos para amanhã (com IDs de tarefas quando houver), 🩺 estado do sistema em uma linha. Telegram Markdown simples.`,
		data.String(), "🌙 *Balanço do dia*\n\n"+data.String())
	if _, err := d.saveInsight(ctx, "Balanço "+now.Format("02/01/2006"), "evening:"+now.Format("2006-01-02"), text, []string{"review", "diario"}); err != nil {
		return text, err
	}
	text = withBlock(text, d.personalBlock(ctx, true))
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

		imported int
	)
	var (
		cleanup   int
		decisions []database.Node
		learnings []database.Node
	)
	wg.Add(7)
	go func() { defer wg.Done(); cleanup, _ = d.DB.CleanupPendingCount(ctx) }()
	go func() {
		defer wg.Done()
		decisions, _ = d.DB.ListNodes(ctx, database.NodeFilter{Tag: agent.TagDecision, From: &weekAgo, Limit: 20})
		learnings, _ = d.DB.ListNodes(ctx, database.NodeFilter{Tag: agent.TagLearning, From: &weekAgo, Limit: 20})
	}()
	go func() {
		defer wg.Done()
		orphans, _ = d.DB.OrphanNodes(ctx, []string{database.TypeHealth, database.TypePerson}, 100)
	}()
	go func() { defer wg.Done(); broken = d.brokenLinks(ctx) }()
	go func() { defer wg.Done(); open, _ = d.DB.OpenTasks(ctx, 500) }()
	go func() {
		defer wg.Done()
		created, _ = d.DB.ListNodes(ctx, database.NodeFilter{From: &weekAgo, KnownDate: true, Limit: 2000})
		imported, _ = d.DB.CountImported(ctx, weekAgo, now.Add(time.Minute))
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
	fmt.Fprintf(&data, "Semana até %s\n\n## Crescimento\n%d nós novos", now.Format("02/01/2006"), len(created))
	if imported > 0 {
		fmt.Fprintf(&data, " (mais %d itens importados, que não contam como novos)", imported)
	}
	fmt.Fprintf(&data, "\n\n## Nós órfãos (%d)\n", len(orphans))
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
	if len(decisions)+len(learnings) > 0 {
		data.WriteString("\n## Memória da semana\n")
		for _, n := range decisions {
			fmt.Fprintf(&data, "- Decisão: %s\n", n.Title)
		}
		for _, n := range learnings {
			fmt.Fprintf(&data, "- Aprendizado: %s\n", n.Title)
		}
	}
	fmt.Fprintf(&data, "\n## Faxina sugerida\n%d sugestões aguardando aprovação em Conteúdo → Faxina\n", cleanup)
	fmt.Fprintf(&data, "\n## Sistema\nfila_falhas=%d custo_llm_7d=US$%.4f tamanho_db=%.1fMB\n", qs[database.TaskFailed], cost, float64(d.DB.Size())/1e6)
	text := d.compose(ctx, "weekly",
		`Gere um RELATÓRIO DE MANUTENÇÃO SEMANAL do Second Brain em português (máx. 300 palavras): 📈 resumo da semana, 🧠 decisões e aprendizados (se houver), 🧹 higiene do grafo (órfãos, links quebrados, faxina pendente — sugira ações concretas), ⏳ pendências acumuladas (sugira o que delegar, reagendar ou descartar), 🎯 3 intenções para a próxima semana. Telegram Markdown simples.`,
		data.String(), "🗓️ *Weekly Review*\n\n"+data.String())
	year, week := now.ISOWeek()
	if _, err := d.saveInsight(ctx, fmt.Sprintf("Weekly Review %d-W%02d", year, week), fmt.Sprintf("weekly:%d-W%02d", year, week), text, []string{"review", "semanal"}); err != nil {
		return text, err
	}
	text = withBlock(text, d.goalsBlock(ctx)) // personal: not saved, not sent to a model
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
	trash := 0
	if d.Agent != nil {
		trash, _ = d.Agent.PurgeTrash(ctx, nil, time.Now().Add(-database.TrashRetention))
	}
	if d.Agent != nil { // safety net for PROFILE_ENCRYPT_ALL edited by hand in .env
		if _, err := d.Agent.Profile().Sync(ctx); err != nil {
			d.Log.Warn("perfil: falha ao ajustar a criptografia", "err", err)
		}
	}
	full := time.Now().In(d.Cfg.Location()).Weekday() == time.Sunday
	if err := d.DB.Maintenance(ctx, full); err != nil {
		return err
	}
	d.Log.Info("manutenção concluída", "temp_files", purged, "tasks", tasks, "logs", logs, "trash", trash, "vacuum_full", full, "db_mb", float64(d.DB.Size())/1e6)
	return nil
}

// cursor reads the last successful run of a cursor-based routine (default: 24 h ago).
func (d *Deps) cursor(ctx context.Context, key string, now time.Time) time.Time {
	if v, ok, _ := d.DB.KVGet(ctx, key); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.Before(now) {
			return t
		}
	}
	return now.Add(-24 * time.Hour)
}

func (d *Deps) link(path string) string {
	if base := strings.TrimRight(d.Cfg.PublicURL(), "/"); base != "" {
		return base + path
	}
	return path
}

// ActionItems creates tasks from new meeting notes and transcripts and reports every task
// the AI created since the last run (meetings, notes and e-mails).
func (d *Deps) ActionItems(ctx context.Context, notify bool) (string, error) {
	loc := d.Cfg.Location()
	now := time.Now()
	from := d.cursor(ctx, "actions.cursor", now)
	rep, err := d.Agent.ExtractMeetingTasks(ctx, from, now)
	if err != nil {
		return "", err
	}
	fresh, err := d.Agent.NewAITasks(ctx, from, 30)
	if err != nil {
		return "", err
	}
	_ = d.DB.KVSet(ctx, "actions.cursor", now.UTC().Format(time.RFC3339Nano))
	if len(fresh) == 0 {
		d.Log.Info("tarefas de reuniões: nada novo", "meetings", rep.Scanned)
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📝 *Tarefas novas* (desde %s)\n\n", from.In(loc).Format("02/01 15:04"))
	for i, t := range fresh {
		if i == 15 {
			fmt.Fprintf(&b, "… e mais %d\n", len(fresh)-15)
			break
		}
		b.WriteString(taskLine(t.Task, loc))
		if t.Origin != nil {
			b.WriteString(" ← " + extract.Truncate(t.Origin.Title, 60))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nConcluir: /done <id> · Revisar: %s", d.link("/dashboard"))
	text := b.String()
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}

// MemoryRun distills the period since the last run into the memory layer.
func (d *Deps) MemoryRun(ctx context.Context, notify bool) (string, error) {
	now := time.Now()
	loc := d.Cfg.Location()
	var last time.Time
	if v, ok, _ := d.DB.KVGet(ctx, "memory.cursor"); ok {
		last, _ = time.Parse(time.RFC3339, v)
	}
	rep, err := d.Agent.DistillMemory(ctx, agent.MemoryWindow(last, now, loc), now)
	if err != nil {
		return "", err
	}
	_ = d.DB.KVSet(ctx, "memory.cursor", now.UTC().Format(time.RFC3339Nano))
	if rep == nil || rep.Snapshot == nil {
		d.Log.Info("memória: nada novo ou sem IA")
		return "", nil
	}
	_ = d.DB.KVSet(ctx, "memory.latest", fmt.Sprint(rep.Snapshot.ID))
	if len(rep.Decisions)+len(rep.Learnings) == 0 {
		return rep.Snapshot.Content, nil
	}
	var b strings.Builder
	b.WriteString("🧠 *Memória do dia*\n")
	for _, n := range rep.Decisions {
		b.WriteString("\n✔️ " + n.Title)
	}
	for _, n := range rep.Learnings {
		b.WriteString("\n💡 " + n.Title)
	}
	text := b.String()
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}

// Cleanup refreshes the cleanup suggestions and tells the user how many await review.
func (d *Deps) Cleanup(ctx context.Context, notify bool) (string, error) {
	counts, err := d.Agent.SuggestCleanup(ctx)
	if err != nil {
		return "", err
	}
	total := 0
	var parts []string
	for _, k := range agent.CleanupKinds {
		if counts[k] > 0 {
			total += counts[k]
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], strings.ToLower(agent.CleanupLabels[k])))
		}
	}
	d.Log.Info("faxina semanal", "suggestions", total)
	if total == 0 {
		return "", nil
	}
	text := fmt.Sprintf("🧹 *Faxina semanal*: %d sugestões (%s).\nRevise e aprove: %s", total, strings.Join(parts, ", "), d.link("/content/cleanup"))
	if notify {
		d.notify(ctx, text)
	}
	return text, nil
}
