package web

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/scheduler"
)

// statTile is one KPI: value, change against the previous period and an optional note.
type statTile struct {
	Label    string
	Value    string
	Delta    string // "▲ 12% vs 7 dias anteriores"
	Tone     string // good | bad | "" (neutral)
	Note     string
	NoteTone string // warn | ""
	Href     string
}

// chartCol is one column of a single-series column chart.
type chartCol struct {
	Label string // tooltip and table ("seg, 28/09")
	Short string // axis
	Value string
	Pct   float64
	Muted bool // previous period (de-emphasis)
	Zero  bool
}

type colChart struct {
	Title   string
	Unit    string // table header for values
	Max     string
	Cols    []chartCol
	Current string // legend: emphasised period (empty = single series, no legend)
	Past    string // legend: muted period
}

type todoItem struct {
	Task    database.Node
	Origin  *database.Node
	Overdue bool
	Today   bool
	AI      bool
}

type dashView struct {
	Days       int
	Tiles      []statTile
	Captures   colChart
	Costs      colChart
	Todo       []todoItem
	Memory     agent.MemoryView
	Cleanup    int
	Usage      []database.UsageRow
	UsageTotal float64
	TokensIn   int64
	TokensOut  int64
	Counts     []typeInfo
	Nodes      int
	Edges      int
	Vectors    map[string]int
	DBSize     string
	Queue      map[string]int
	Failed     []database.Task
	Jobs       []scheduler.JobInfo
	Briefing   *database.Node
	Metrics    []database.Metric
	Integs     []integ
}

var weekdays = [...]string{"dom", "seg", "ter", "qua", "qui", "sex", "sáb"}

// delta formats the change between two periods and its tone (upGood: rising is good).
func delta(cur, prev float64, upGood bool, what string) (string, string) {
	if prev == 0 && cur == 0 {
		return "sem mudança " + what, ""
	}
	if prev == 0 {
		return "▲ novo " + what, map[bool]string{true: "good", false: "bad"}[upGood]
	}
	pct := (cur - prev) / prev * 100
	switch {
	case math.Abs(pct) < 1:
		return "= " + what, ""
	case pct > 0:
		return fmt.Sprintf("▲ %.0f%% %s", pct, what), map[bool]string{true: "good", false: "bad"}[upGood]
	}
	return fmt.Sprintf("▼ %.0f%% %s", -pct, what), map[bool]string{true: "bad", false: "good"}[upGood]
}

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 365 {
		days = 30
	}
	loc := s.Cfg.Location()
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	weekAgo, twoWeeks := now.AddDate(0, 0, -7), now.AddDate(0, 0, -14)
	v := dashView{Days: days, Vectors: s.DB.VectorCount(), DBSize: fmt.Sprintf("%.1f MB", float64(s.DB.Size())/1e6)}
	var (
		perDay            map[string]int
		open              []database.Node
		doneWeek, donePrv []database.Node
		ai                []agent.TaskOrigin
		usage7, usage14   []database.UsageRow
		daily             []database.DailyUsage
		counts            map[string]int
	)
	g, ctx := errgroup.WithContext(r.Context())
	g.Go(func() (err error) { perDay, err = s.DB.CreatedPerDay(ctx, today.AddDate(0, 0, -13), loc); return })
	g.Go(func() (err error) { open, err = s.DB.OpenTasks(ctx, 1000); return })
	g.Go(func() (err error) {
		doneWeek, err = s.DB.UpdatedBetween(ctx, []string{database.TypeTask}, database.StatusDone, weekAgo, now.Add(time.Minute), 5000)
		return
	})
	g.Go(func() (err error) {
		donePrv, err = s.DB.UpdatedBetween(ctx, []string{database.TypeTask}, database.StatusDone, twoWeeks, weekAgo, 5000)
		return
	})
	g.Go(func() (err error) { ai, err = s.Agent.NewAITasks(ctx, now.Add(-24*time.Hour), 20); return })
	g.Go(func() (err error) { usage7, err = s.DB.UsageSummary(ctx, weekAgo); return })
	g.Go(func() (err error) { usage14, err = s.DB.UsageSummary(ctx, twoWeeks); return })
	g.Go(func() (err error) { v.Usage, err = s.DB.UsageSummary(ctx, now.AddDate(0, 0, -days)); return })
	g.Go(func() (err error) { daily, err = s.DB.UsageDaily(ctx, time.Now().AddDate(0, 0, -14)); return })
	g.Go(func() (err error) { v.Memory, err = s.Agent.Memory(ctx, 5); return })
	g.Go(func() (err error) { v.Cleanup, err = s.DB.CleanupPendingCount(ctx); return })
	g.Go(func() (err error) { counts, err = s.DB.CountByType(ctx); return })
	g.Go(func() (err error) { v.Edges, err = s.DB.EdgeCount(ctx); return })
	g.Go(func() (err error) { v.Queue, err = s.DB.QueueStats(ctx); return })
	g.Go(func() (err error) { v.Failed, err = s.DB.ListTasks(ctx, database.TaskFailed, 10); return })
	g.Go(func() (err error) {
		v.Metrics, err = s.DB.MetricsRange(ctx, now.AddDate(0, 0, -7).Format("2006-01-02"), now.Format("2006-01-02"))
		return
	})
	g.Go(func() error {
		if id, ok, _ := s.DB.KVGet(ctx, "routine.latest.morning"); ok {
			if n, err := strconv.ParseInt(id, 10, 64); err == nil {
				v.Briefing, _ = s.DB.GetNode(ctx, n)
			}
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.Hooks.Jobs != nil {
		v.Jobs = s.Hooks.Jobs()
	}
	v.Integs = s.integrations()
	for _, u := range v.Usage {
		v.UsageTotal += u.CostUSD
		v.TokensIn += u.InputTokens
		v.TokensOut += u.OutputTokens
	}
	for _, t := range database.NodeTypes {
		v.Counts = append(v.Counts, typeInfo{Type: t, Label: typeLabels[t], Color: template.CSS(typeColors[t]), Count: counts[t]})
		v.Nodes += counts[t]
	}

	// Captures per day: the last 7 days emphasised, the 7 before as context.
	var capCur, capPrev, capMax float64
	for i := 13; i >= 0; i-- {
		day := today.AddDate(0, 0, -i)
		n := float64(perDay[day.Format("2006-01-02")])
		capMax = max(capMax, n)
		if i < 7 {
			capCur += n
		} else {
			capPrev += n
		}
		v.Captures.Cols = append(v.Captures.Cols, chartCol{Label: weekdays[day.Weekday()] + ", " + day.Format("02/01"), Short: day.Format("02"),
			Value: fmt.Sprint(int(n)), Pct: n, Muted: i >= 7, Zero: n == 0})
	}
	top := niceMax(capMax)
	for i := range v.Captures.Cols {
		v.Captures.Cols[i].Pct = v.Captures.Cols[i].Pct / top * 100
	}
	v.Captures.Title, v.Captures.Unit, v.Captures.Max = "Capturas por dia", "Itens", fmt.Sprint(top)
	v.Captures.Current, v.Captures.Past = "Últimos 7 dias", "7 dias anteriores"

	// LLM cost per day (single series).
	costDay := map[string]float64{}
	var costMax float64
	for _, d := range daily {
		costDay[d.Day] += d.CostUSD
	}
	for i := 13; i >= 0; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i)
		c := costDay[day.Format("2006-01-02")]
		costMax = max(costMax, c)
		v.Costs.Cols = append(v.Costs.Cols, chartCol{Label: weekdays[day.Weekday()] + ", " + day.Format("02/01"), Short: day.Format("02"), Value: fmt.Sprintf("US$ %.4f", c), Pct: c, Zero: c == 0})
	}
	ctop := niceMax(costMax)
	for i := range v.Costs.Cols {
		v.Costs.Cols[i].Pct = v.Costs.Cols[i].Pct / ctop * 100
	}
	v.Costs.Title, v.Costs.Unit, v.Costs.Max = "Custo diário (14 dias)", "Custo", fmt.Sprintf("US$ %g", ctop)

	// To do: overdue, due today, then new tasks created by the AI.
	end := today.AddDate(0, 0, 1)
	listed := map[int64]bool{}
	overdue := 0
	for _, t := range open {
		if t.DueAt == nil || !t.DueAt.Before(end) {
			continue
		}
		it := todoItem{Task: t, Overdue: t.DueAt.Before(today), Today: !t.DueAt.Before(today)}
		if it.Overdue {
			overdue++
		}
		if len(v.Todo) < 8 {
			v.Todo = append(v.Todo, it)
			listed[t.ID] = true
		}
	}
	for _, t := range ai {
		if !listed[t.Task.ID] && len(v.Todo) < 12 {
			v.Todo = append(v.Todo, todoItem{Task: t.Task, Origin: t.Origin, AI: true})
		}
	}

	var cost7, cost14 float64
	for _, u := range usage7 {
		cost7 += u.CostUSD
	}
	for _, u := range usage14 {
		cost14 += u.CostUSD
	}
	capDelta, capTone := delta(capCur, capPrev, true, "vs 7 dias antes")
	doneDelta, doneTone := delta(float64(len(doneWeek)), float64(len(donePrv)), true, "vs 7 dias antes")
	costDelta, costTone := delta(cost7, cost14-cost7, false, "vs 7 dias antes")
	openTile := statTile{Label: "Tarefas abertas", Value: humanInt(int64(len(open))), Href: "/content?type=task&status=open&order=oldest"}
	if overdue > 0 {
		openTile.Note, openTile.NoteTone = fmt.Sprintf("⚠ %d atrasada(s)", overdue), "warn"
	} else {
		openTile.Note = "nenhuma atrasada"
	}
	aiTile := statTile{Label: "Novas da IA (24 h)", Value: fmt.Sprint(len(ai)), Note: "de reuniões, notas e e-mails", Href: "/content?type=task&status=open&source=agent"}
	cleanTile := statTile{Label: "Faxina", Value: fmt.Sprint(v.Cleanup), Note: "sugestões para revisar", Href: "/content/cleanup"}
	if v.Cleanup == 0 {
		cleanTile.Note = "nada pendente"
	}
	v.Tiles = []statTile{
		{Label: "Capturas (7 dias)", Value: humanInt(int64(capCur)), Delta: capDelta, Tone: capTone, Href: "/content"},
		openTile,
		{Label: "Concluídas (7 dias)", Value: humanInt(int64(len(doneWeek))), Delta: doneDelta, Tone: doneTone, Href: "/content?type=task&status=done&order=updated"},
		aiTile,
		cleanTile,
		{Label: "Custo de IA (7 dias)", Value: fmt.Sprintf("US$ %.2f", cost7), Delta: costDelta, Tone: costTone, Href: "#costs"},
	}
	s.render(w, "dashboard", s.page(r, "Painel", "dashboard", v))
}

// dashboardTask completes or discards a task from the dashboard's to-do list.
func (s *Server) dashboardTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "tarefa inválida", http.StatusBadRequest)
		return
	}
	n, err := s.DB.GetNode(r.Context(), id)
	if err != nil || n.Type != database.TypeTask {
		http.Error(w, "tarefa não encontrada", http.StatusNotFound)
		return
	}
	switch r.PathValue("action") {
	case "done":
		err = s.DB.SetStatus(r.Context(), id, database.StatusDone)
	case "discard": // wrong suggestion: trash it and keep the AI from recreating it
		_, _, err = s.DB.TrashNodes(r.Context(), []int64{id}, true)
	default:
		http.Error(w, "ação inválida", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if !isHTMX(r) {
		redirectFlash(w, r, "/dashboard", "Tarefa atualizada.", false)
		return
	}
	label := "✓ Concluída"
	if r.PathValue("action") == "discard" {
		label = "Descartada (está na lixeira)"
	}
	fmt.Fprintf(w, `<li class="todo-done"><s>%s</s> <small class="muted">%s</small></li>`, template.HTMLEscapeString(n.Title), label)
}
