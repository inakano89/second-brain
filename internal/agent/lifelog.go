package agent

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
)

// ---- "Neste dia" ----

// DayMemory holds what happened on the same calendar day some years ago.
type DayMemory struct {
	YearsAgo int
	Date     time.Time
	Nodes    []database.Node
}

// onThisDaySkip are sources whose nodes are periodic reports or feeds, not memories.
var onThisDaySkip = map[string]bool{"routine": true, MemorySource: true, "rss": true, "newsletter": true, "youtube": true, "health": true, "webhook": true}

// OnThisDayYears returns ON_THIS_DAY_YEARS ("1,3,5" by default; "off" turns the feature off).
func (a *Agent) OnThisDayYears() []int {
	raw := a.cfg.Get("ON_THIS_DAY_YEARS")
	var out []int
	for _, p := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if y, err := strconv.Atoi(p); err == nil && y > 0 && y <= 60 {
			out = append(out, y)
		}
	}
	return out
}

// OnThisDay lists notes, insights, articles and events of day's month and day in the configured
// years back. Only content with a known original date takes part.
func (a *Agent) OnThisDay(ctx context.Context, day time.Time) ([]DayMemory, error) {
	years := a.OnThisDayYears()
	if len(years) == 0 {
		return nil, nil
	}
	loc := a.cfg.Location()
	byYear, err := a.db.OnThisDay(ctx, day, years, []string{database.TypeNote, database.TypeInsight, database.TypeArticle, database.TypeEvent}, loc, 12)
	if err != nil {
		return nil, err
	}
	var out []DayMemory
	for _, y := range years {
		var keep []database.Node
		for _, n := range byYear[y] {
			if onThisDaySkip[n.Source] || len(keep) >= 3 {
				continue
			}
			keep = append(keep, n)
		}
		if len(keep) > 0 {
			out = append(out, DayMemory{YearsAgo: y, Date: day.In(loc).AddDate(-y, 0, 0), Nodes: keep})
		}
	}
	return out, nil
}

// FormatOnThisDay renders memories as a Telegram-friendly list ("" when there are none).
func FormatOnThisDay(mem []DayMemory) string {
	if len(mem) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("🕰️ *Neste dia*\n")
	for _, m := range mem {
		label := fmt.Sprintf("%d anos atrás", m.YearsAgo)
		if m.YearsAgo == 1 {
			label = "1 ano atrás"
		}
		for i, n := range m.Nodes {
			head := ""
			if i == 0 {
				head = label + " (" + strconv.Itoa(m.Date.Year()) + "): "
			} else {
				head = "  · "
			}
			line := head + "*" + oneLine(n.Title) + "*"
			if s := strings.TrimSpace(firstNonEmptyStr(n.Summary, n.Content)); s != "" {
				line += " — " + extract.Truncate(oneLine(s), 110)
			}
			fmt.Fprintf(&b, "• %s (#%d)\n", line, n.ID)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func oneLine(s string) string {
	return strings.NewReplacer("*", "", "_", " ", "`", "'", "[", "(", "]", ")").Replace(strings.Join(strings.Fields(s), " "))
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ---- Retrospectiva do ano ----

const yearReviewSystem = `Você escreve a RETROSPECTIVA DO ANO de um Second Brain pessoal, em português, a partir dos dados agregados fornecidos (nunca invente fatos além deles).
Estruture em Markdown com estas seções: ## Panorama (2-3 frases com os números mais marcantes), ## Temas do ano (o que ocupou a cabeça, com base nas tags e nos itens), ## Pessoas (quem mais apareceu), ## Decisões e aprendizados (agrupe por assunto), ## O que ficou de fora (pendências e tarefas que não andaram), ## Para o próximo ano (3 intenções concretas).
Seja específico, cite títulos entre [[colchetes duplos]] quando falar de uma nota e mantenha tom de conversa, sem exageros. Máximo de 450 palavras.`

// YearReview builds (and stores as an insight) the retrospective of a calendar year: themes,
// people, decisions, numbers and what was left undone. Only content with a known original date
// counts; the personal profile is never used.
func (a *Agent) YearReview(ctx context.Context, year int) (*database.Node, string, error) {
	loc := a.cfg.Location()
	from := time.Date(year, 1, 1, 0, 0, 0, 0, loc)
	to := from.AddDate(1, 0, 0)
	if from.After(time.Now()) {
		return nil, "", fmt.Errorf("o ano %d ainda não começou", year)
	}
	f := database.NodeFilter{From: &from, To: &to, KnownDate: true, Limit: 5000}
	nodes, err := a.db.ListNodes(ctx, f)
	if err != nil {
		return nil, "", err
	}
	// Calendar events are mirrored when they sync: they belong to the year they happen in.
	nodes = slices.DeleteFunc(nodes, func(n database.Node) bool { return n.Type == database.TypeEvent })
	if evs, err := a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeEvent}, DueFrom: &from, DueTo: &to, Limit: 5000}); err == nil {
		nodes = append(nodes, evs...)
	}
	byType, byMonth := map[string]int{}, [12]int{}
	tags := map[string]int{}
	var written []database.Node
	for _, n := range nodes {
		if onThisDaySkip[n.Source] && n.Source != MemorySource {
			continue
		}
		byType[n.Type]++
		byMonth[n.EffectiveAt().In(loc).Month()-1]++
		for _, t := range n.Tags {
			tags[t]++
		}
		if n.Type == database.TypeNote || n.Type == database.TypeInsight || n.Type == database.TypeArticle {
			written = append(written, n)
		}
	}
	if len(written)+byType[database.TypeTask] == 0 {
		return nil, "", fmt.Errorf("não há conteúdo datado em %d para resumir", year)
	}
	people, _ := a.db.TopMentionedPeople(ctx, from, to, 10)
	decisions, _ := a.db.ListNodes(ctx, database.NodeFilter{Tag: TagDecision, From: &from, To: &to, Limit: 30})
	learnings, _ := a.db.ListNodes(ctx, database.NodeFilter{Tag: TagLearning, From: &from, To: &to, Limit: 30})
	doneTasks, _ := a.db.CountNodes(ctx, database.NodeFilter{Types: []string{database.TypeTask}, Status: database.StatusDone, From: &from, To: &to})
	openTasks, _ := a.db.CountNodes(ctx, database.NodeFilter{Types: []string{database.TypeTask}, Status: database.StatusOpen, From: &from, To: &to})
	var openList []database.Node
	if openTasks > 0 {
		openList, _ = a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeTask}, Status: database.StatusOpen, From: &from, To: &to, Order: "oldest", Limit: 10})
	}

	type kv struct {
		K string
		V int
	}
	top := func(m map[string]int, n int) []kv {
		var l []kv
		for k, v := range m {
			l = append(l, kv{k, v})
		}
		sort.Slice(l, func(i, j int) bool { return l[i].V > l[j].V || (l[i].V == l[j].V && l[i].K < l[j].K) })
		return l[:min(n, len(l))]
	}
	var d strings.Builder
	fmt.Fprintf(&d, "Ano: %d\n\n## Números\n", year)
	for _, t := range database.NodeTypes {
		if byType[t] > 0 {
			fmt.Fprintf(&d, "- %s: %d\n", t, byType[t])
		}
	}
	fmt.Fprintf(&d, "- tarefas concluídas: %d; ainda abertas: %d\n", doneTasks, openTasks)
	d.WriteString("\n## Itens criados por mês\n")
	for i, c := range byMonth {
		fmt.Fprintf(&d, "- %s: %d\n", monthsPT[i], c)
	}
	d.WriteString("\n## Tags mais usadas\n")
	for _, t := range top(tags, 15) {
		fmt.Fprintf(&d, "- #%s (%d)\n", t.K, t.V)
	}
	d.WriteString("\n## Pessoas mais citadas\n")
	for _, p := range people {
		fmt.Fprintf(&d, "- %s (%d menções)\n", p.Name, p.Count)
	}
	// The longest and most linked writing shows what mattered.
	sort.Slice(written, func(i, j int) bool { return len(written[i].Content) > len(written[j].Content) })
	d.WriteString("\n## Textos mais longos do ano\n")
	for _, n := range written[:min(12, len(written))] {
		fmt.Fprintf(&d, "- [[%s]] (%s, %s): %s\n", n.Title, n.Type, n.CreatedAt.In(loc).Format("02/01"), extract.Truncate(oneLine(firstNonEmptyStr(n.Summary, n.Content)), 160))
	}
	d.WriteString("\n## Decisões\n")
	for _, n := range decisions {
		fmt.Fprintf(&d, "- %s (%s)\n", n.Title, n.CreatedAt.In(loc).Format("02/01"))
	}
	d.WriteString("\n## Aprendizados\n")
	for _, n := range learnings {
		fmt.Fprintf(&d, "- %s (%s)\n", n.Title, n.CreatedAt.In(loc).Format("02/01"))
	}
	d.WriteString("\n## Tarefas do ano que continuam abertas (mais antigas)\n")
	for _, n := range openList {
		fmt.Fprintf(&d, "- %s (criada em %s)\n", n.Title, n.CreatedAt.In(loc).Format("02/01"))
	}

	text := a.composeText(ctx, "year-review", yearReviewSystem, d.String(), "# Retrospectiva "+strconv.Itoa(year)+"\n\n"+d.String())
	title := fmt.Sprintf("Retrospectiva %d", year)
	n, _, err := a.Ingest(ctx, IngestInput{
		Type: database.TypeInsight, Title: title, Content: text, Summary: extract.Truncate(oneLine(text), 280),
		Source: "routine", SourceRef: fmt.Sprintf("year:%d", year), Tags: []string{"retrospectiva", "review"},
		Meta: map[string]any{"enriched": true, "year": year}, Enrich: true,
	})
	if err != nil {
		return nil, text, err
	}
	return n, text, nil
}

var monthsPT = []string{"jan", "fev", "mar", "abr", "mai", "jun", "jul", "ago", "set", "out", "nov", "dez"}

// composeText asks the model to write text from data, falling back to the raw data when no
// model is configured or the call fails.
func (a *Agent) composeText(ctx context.Context, purpose, system, data, fallback string) string {
	if !a.llm.Enabled() {
		return fallback
	}
	text, err := a.Ask(ctx, purpose, system, data)
	if err != nil || strings.TrimSpace(text) == "" {
		a.log.Warn("LLM indisponível, usando o texto sem IA", "purpose", purpose, "err", err)
		return fallback + "\n\n_(gerado sem LLM)_"
	}
	return text
}

// ---- Revisão espaçada ----

// reviewIntervals are the days until a reviewed item comes back, growing with each showing;
// after the last one it repeats every 240 days.
var reviewIntervals = []int{1, 3, 7, 14, 30, 60, 120, 240}

// ReviewItem is one thing to remember today.
type ReviewItem struct {
	Node database.Node
	Text string
	Step int // 1 = first time shown
}

// ReviewDue returns up to limit items to review at now: those whose interval elapsed, topped up
// with newly enrolled highlights (Kindle books) and insights. Each returned item is rescheduled.
func (a *Agent) ReviewDue(ctx context.Context, now time.Time, limit int) ([]ReviewItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	due, err := a.db.DueReviews(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	if len(due) < limit {
		cands, err := a.db.ReviewCandidates(ctx, limit-len(due))
		if err != nil {
			return nil, err
		}
		for _, n := range cands {
			if err := a.db.EnrollReview(ctx, n.ID, now); err != nil {
				return nil, err
			}
		}
		if len(cands) > 0 {
			if due, err = a.db.DueReviews(ctx, now, limit); err != nil {
				return nil, err
			}
		}
	}
	var out []ReviewItem
	for _, r := range due {
		n, err := a.db.GetNode(ctx, r.NodeID)
		if err != nil {
			continue
		}
		hl := highlights(n)
		if len(hl) == 0 {
			continue
		}
		out = append(out, ReviewItem{Node: *n, Text: hl[r.Cursor%len(hl)], Step: r.Step + 1})
		r.Step++
		r.Cursor++
		r.LastAt = now
		r.NextAt = now.AddDate(0, 0, reviewIntervals[min(r.Step-1, len(reviewIntervals)-1)])
		if err := a.db.SaveReview(ctx, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// highlights splits a node into the pieces worth remembering: quotes and notes of a Kindle book,
// or the text of an insight.
func highlights(n *database.Node) []string {
	if n.Source == "import:kindle" {
		var out []string
		for _, block := range strings.Split(n.Content, "\n\n") {
			block = strings.TrimSpace(block)
			if strings.HasPrefix(block, "> ") || strings.HasPrefix(block, "📝") {
				out = append(out, strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(block, "> "), "\n> ", "\n")))
			}
		}
		return out
	}
	if t := strings.TrimSpace(n.Content); t != "" {
		return []string{extract.Truncate(t, 700)}
	}
	return nil
}

// FormatReviews renders the day's review as a Telegram message ("" when nothing is due).
func FormatReviews(items []ReviewItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("🔁 *Revisão do dia*\n")
	for _, it := range items {
		nth := "primeira vez"
		if it.Step > 1 {
			nth = strconv.Itoa(it.Step) + "ª vez"
		}
		fmt.Fprintf(&b, "\n📚 *%s* — %s\n", oneLine(it.Node.Title), nth)
		for _, ln := range strings.Split(strings.TrimSpace(it.Text), "\n") {
			b.WriteString("> " + strings.TrimSpace(ln) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
