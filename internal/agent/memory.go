package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

// Memory nodes: source "memory"; decisions and learnings are insights tagged "decisao" /
// "aprendizado", the current priorities live in one node (ref "priorities") and every run
// writes a snapshot of the period (ref "day:<date>").
const (
	MemorySource      = "memory"
	memoryPriorities  = "priorities"
	TagDecision       = "decisao"
	TagLearning       = "aprendizado"
	TagMemory         = "memoria"
	memoryInputBudget = 40000
)

// memorySkip are sources that bring outside information, not the user's own life and work.
var memorySkip = map[string]bool{"routine": true, MemorySource: true, "rss": true, "newsletter": true, "youtube": true, "health": true, "webhook": true, "contacts": true}

type memoryItem struct {
	Title   string  `json:"title"`
	Detail  string  `json:"detail"`
	Why     string  `json:"why"`
	Sources []int64 `json:"sources"`
}

type memoryResult struct {
	Decisions     []memoryItem `json:"decisions"`
	Learnings     []memoryItem `json:"learnings"`
	Priorities    []memoryItem `json:"priorities"`
	OpenQuestions []memoryItem `json:"open_questions"`
	Diary         string       `json:"diary"`
}

// MemoryReport summarises a memory run.
type MemoryReport struct {
	Snapshot   *database.Node
	Decisions  []database.Node
	Learnings  []database.Node
	Priorities int
	Inputs     int
}

const memorySystem = `Você mantém a MEMÓRIA de longo prazo de um "Second Brain" pessoal. Leia o que entrou no período (notas, reuniões, e-mails, tarefas e conversas com o assistente) e responda SOMENTE JSON:
{"decisions":[{"title":"a decisão em 1 frase afirmativa","detail":"contexto, motivo e alternativas descartadas","sources":[ids]}],
"learnings":[{"title":"o aprendizado em 1 frase","detail":"como foi aprendido e quando aplicar","sources":[ids]}],
"priorities":[{"title":"prioridade","why":"por que importa agora","sources":[ids]}],
"open_questions":[{"title":"pergunta ou pendência sem resposta","sources":[ids]}],
"diary":"2 a 4 frases, em primeira pessoa, sobre o que marcou o período"}
Regras:
- Só o que está nos dados; não invente. Decisões são escolhas feitas (não intenções vagas). Aprendizados são lições ou fatos úteis para o futuro.
- Não repita decisões já listadas em <decisoes_recentes>, a menos que tenham mudado (diga o que mudou).
- priorities é a lista COMPLETA e atualizada (no máximo 7), partindo de <prioridades_atuais>: mantenha as que continuam valendo, remova as concluídas e acrescente as novas. Se não houver sinal de mudança, repita a lista atual.
- sources são ids dos itens de origem. Listas podem ficar vazias. Mantenha o idioma do usuário.`

// MemoryWindow returns the period a memory run should read: from the last run (at most a
// week back) or the start of today, whichever is earlier, so a second run on the same day
// rewrites that day's snapshot instead of splitting it.
func MemoryWindow(last, now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	from := day
	if !last.IsZero() && last.Before(day) {
		from = last
	}
	if weekAgo := day.AddDate(0, 0, -7); from.Before(weekAgo) {
		from = weekAgo
	}
	return from
}

// DistillMemory turns what changed in [from, to) into decisions, learnings, priorities and a
// snapshot, all linked to their sources. It returns nil when there is no LLM or nothing new.
func (a *Agent) DistillMemory(ctx context.Context, from, to time.Time) (*MemoryReport, error) {
	if !a.llm.Enabled() {
		return nil, nil
	}
	var (
		changed   []database.Node
		chat      []database.ChatMessage
		current   *database.Node
		decisions []database.Node
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		changed, err = a.db.ChangedSince(gctx, []string{database.TypeNote, database.TypeEvent, database.TypeInsight, database.TypeTask, database.TypeArticle}, from, to, 600)
		return
	})
	g.Go(func() (err error) { chat, err = a.db.ChatSince(gctx, from, 300); return })
	g.Go(func() error {
		n, err := a.db.GetNodeBySource(gctx, MemorySource, memoryPriorities)
		if err == nil {
			current = n
			return nil
		}
		if errors.Is(err, database.ErrNotFound) {
			return nil
		}
		return err
	})
	g.Go(func() (err error) {
		decisions, err = a.db.ListNodes(gctx, database.NodeFilter{Tag: TagDecision, Limit: 25})
		return
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	loc := a.cfg.Location()
	var items strings.Builder
	valid := map[int64]string{}
	inputs := 0
	for i := len(changed) - 1; i >= 0; i-- { // newest first, until the budget runs out
		n := changed[i]
		if memorySkip[n.Source] || strings.HasPrefix(n.Source, "import:") || n.Type == database.TypeArticle && n.Source != "web" && n.Source != "clip" && n.Source != "telegram" {
			continue
		}
		body := n.Content
		if n.Summary != "" && len([]rune(body)) > 1500 {
			body = n.Summary + "\n" + extract.Truncate(body, 800)
		}
		status := ""
		if n.Type == database.TypeTask {
			status = " status=" + n.Status
		}
		entry := fmt.Sprintf("[id=%d tipo=%s fonte=%s data=%s%s] %s\n%s\n---\n", n.ID, n.Type, n.Source, n.UpdatedAt.In(loc).Format("2006-01-02"), status, n.Title, extract.Truncate(body, 1500))
		if items.Len()+len(entry) > memoryInputBudget {
			break
		}
		items.WriteString(entry)
		valid[n.ID] = n.Title
		inputs++
	}
	var talk strings.Builder
	for _, m := range chat {
		who := "Usuário"
		if m.Role == llm.RoleAssistant {
			who = "Assistente"
		}
		line := fmt.Sprintf("%s (%s): %s\n", who, m.TS.In(loc).Format("02/01 15:04"), extract.Truncate(m.Content, 500))
		if talk.Len()+len(line) > 8000 {
			break
		}
		talk.WriteString(line)
	}
	if inputs == 0 && talk.Len() == 0 {
		return nil, nil
	}

	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Período: %s a %s\n\n<prioridades_atuais>\n", from.In(loc).Format("02/01/2006 15:04"), to.In(loc).Format("02/01/2006 15:04"))
	if current != nil {
		prompt.WriteString(extract.Truncate(current.Content, 3000))
	}
	prompt.WriteString("\n</prioridades_atuais>\n\n<decisoes_recentes>\n")
	for _, d := range decisions {
		fmt.Fprintf(&prompt, "- %s (%s)\n", d.Title, d.CreatedAt.In(loc).Format("02/01"))
	}
	prompt.WriteString("</decisoes_recentes>\n\n<itens>\n" + items.String() + "</itens>\n")
	if talk.Len() > 0 {
		prompt.WriteString("\n<conversas>\n" + talk.String() + "</conversas>\n")
	}
	var res memoryResult
	if err := a.llm.CompleteJSON(ctx, "", llm.Request{Purpose: "memory", System: memorySystem, MaxTokens: 6000,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt.String()}}}, &res); err != nil {
		return nil, err
	}

	rep := &MemoryReport{Inputs: inputs}
	sources := func(ids []int64) []int64 {
		var out []int64
		for _, id := range ids {
			if _, ok := valid[id]; ok {
				out = append(out, id)
			}
		}
		return out
	}
	links := func(ids []int64) string {
		var names []string
		for _, id := range sources(ids) {
			names = append(names, "[["+valid[id]+"]]")
		}
		if len(names) == 0 {
			return ""
		}
		return "\n\nFontes: " + strings.Join(names, ", ")
	}
	save := func(kind, tag, label string, it memoryItem) (*database.Node, error) {
		title := strings.TrimSpace(it.Title)
		if title == "" {
			return nil, nil
		}
		n, _, err := a.Ingest(ctx, IngestInput{
			Type: database.TypeInsight, Title: extract.Truncate(title, 160), Content: strings.TrimSpace(it.Detail) + links(it.Sources),
			Summary: extract.Truncate(title, 280), Source: MemorySource, SourceRef: kind + ":" + shortHash(foldTitle(title)),
			Tags: []string{TagMemory, tag}, Meta: map[string]any{"kind": kind, "label": label, "enriched": true}, Enrich: true,
		})
		if errors.Is(err, database.ErrDeleted) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, src := range sources(it.Sources) {
			if err := a.db.AddEdge(ctx, n.ID, src, "derived_from", 1); err != nil {
				return nil, err
			}
		}
		return n, nil
	}
	for _, it := range res.Decisions[:min(len(res.Decisions), 10)] {
		n, err := save("decision", TagDecision, "Decisão", it)
		if err != nil {
			return nil, err
		}
		if n != nil {
			rep.Decisions = append(rep.Decisions, *n)
		}
	}
	for _, it := range res.Learnings[:min(len(res.Learnings), 10)] {
		n, err := save("learning", TagLearning, "Aprendizado", it)
		if err != nil {
			return nil, err
		}
		if n != nil {
			rep.Learnings = append(rep.Learnings, *n)
		}
	}

	var prio strings.Builder
	for i, p := range res.Priorities[:min(len(res.Priorities), 7)] {
		if strings.TrimSpace(p.Title) == "" {
			continue
		}
		fmt.Fprintf(&prio, "%d. **%s**", i+1, strings.TrimSpace(p.Title))
		if w := strings.TrimSpace(p.Why); w != "" {
			prio.WriteString(" — " + w)
		}
		prio.WriteString("\n")
		rep.Priorities++
	}
	if rep.Priorities > 0 { // an empty answer keeps the current list
		pn, _, err := a.Ingest(ctx, IngestInput{
			Type: database.TypeInsight, Title: "Prioridades atuais", Content: prio.String() + "\n_Atualizado em " + to.In(loc).Format("02/01/2006 15:04") + "._",
			Source: MemorySource, SourceRef: memoryPriorities, Tags: []string{TagMemory, "prioridades"},
			Meta: map[string]any{"kind": "priorities", "enriched": true}, Enrich: true,
		})
		if err != nil && !errors.Is(err, database.ErrDeleted) {
			return nil, err
		}
		if pn != nil {
			for _, p := range res.Priorities {
				for _, src := range sources(p.Sources) {
					_ = a.db.AddEdge(ctx, pn.ID, src, "derived_from", 0.8)
				}
			}
		}
	}

	day := to.In(loc)
	title := "Memória de " + day.Format("02/01/2006")
	if f := from.In(loc); f.Format("2006-01-02") != day.Format("2006-01-02") && to.Sub(from) > 26*time.Hour {
		title = "Memória de " + f.Format("02/01") + " a " + day.Format("02/01/2006")
	}
	var snap strings.Builder
	if d := strings.TrimSpace(res.Diary); d != "" {
		snap.WriteString(d + "\n\n")
	}
	section := func(name string, nodes []database.Node) {
		if len(nodes) == 0 {
			return
		}
		snap.WriteString("## " + name + "\n")
		for _, n := range nodes {
			snap.WriteString("- [[" + n.Title + "]]\n")
		}
		snap.WriteString("\n")
	}
	section("Decisões", rep.Decisions)
	section("Aprendizados", rep.Learnings)
	if prio.Len() > 0 {
		snap.WriteString("## Prioridades\n" + prio.String() + "\n")
	}
	if len(res.OpenQuestions) > 0 {
		snap.WriteString("## Perguntas em aberto\n")
		for _, q := range res.OpenQuestions[:min(len(res.OpenQuestions), 8)] {
			if t := strings.TrimSpace(q.Title); t != "" {
				snap.WriteString("- " + t + links(q.Sources) + "\n")
			}
		}
	}
	sn, _, err := a.Ingest(ctx, IngestInput{
		Type: database.TypeInsight, Title: title, Content: strings.TrimSpace(snap.String()), Summary: extract.Truncate(strings.TrimSpace(res.Diary), 280),
		Source: MemorySource, SourceRef: "day:" + day.Format("2006-01-02"), Tags: []string{TagMemory, "diario"},
		Meta: map[string]any{"kind": "snapshot", "from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339), "enriched": true}, Enrich: true,
	})
	if err != nil && !errors.Is(err, database.ErrDeleted) {
		return nil, err
	}
	if sn != nil {
		rep.Snapshot = sn
		for _, n := range append(append([]database.Node{}, rep.Decisions...), rep.Learnings...) {
			_ = a.db.AddEdge(ctx, sn.ID, n.ID, "summarizes", 1)
		}
	}
	a.log.Info("memória atualizada", "inputs", inputs, "decisions", len(rep.Decisions), "learnings", len(rep.Learnings), "priorities", rep.Priorities)
	return rep, nil
}

// MemoryView is what the dashboard, briefing and chat show from the memory layer.
type MemoryView struct {
	Priorities *database.Node
	Decisions  []database.Node
	Learnings  []database.Node
}

// Memory loads the current priorities and the latest decisions and learnings.
func (a *Agent) Memory(ctx context.Context, limit int) (MemoryView, error) {
	var v MemoryView
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		n, err := a.db.GetNodeBySource(gctx, MemorySource, memoryPriorities)
		if err == nil {
			v.Priorities = n
		} else if !errors.Is(err, database.ErrNotFound) {
			return err
		}
		return nil
	})
	g.Go(func() (err error) {
		v.Decisions, err = a.db.ListNodes(gctx, database.NodeFilter{Tag: TagDecision, Limit: limit})
		return
	})
	g.Go(func() (err error) {
		v.Learnings, err = a.db.ListNodes(gctx, database.NodeFilter{Tag: TagLearning, Limit: limit})
		return
	})
	return v, g.Wait()
}

// memoryPrompt is the memory block added to the chat system prompt.
func (a *Agent) memoryPrompt(ctx context.Context) string {
	v, err := a.Memory(ctx, 8)
	if err != nil || (v.Priorities == nil && len(v.Decisions) == 0 && len(v.Learnings) == 0) {
		return ""
	}
	loc := a.cfg.Location()
	var b strings.Builder
	b.WriteString("\n\n<memoria>\n")
	if v.Priorities != nil {
		b.WriteString("Prioridades atuais do usuário:\n" + extract.Truncate(v.Priorities.Content, 1500) + "\n")
	}
	if len(v.Decisions) > 0 {
		b.WriteString("Decisões recentes:\n")
		for _, d := range v.Decisions {
			fmt.Fprintf(&b, "- %s: %s\n", d.CreatedAt.In(loc).Format("02/01"), d.Title)
		}
	}
	if len(v.Learnings) > 0 {
		b.WriteString("Aprendizados recentes:\n")
		for _, l := range v.Learnings {
			fmt.Fprintf(&b, "- %s: %s\n", l.CreatedAt.In(loc).Format("02/01"), l.Title)
		}
	}
	b.WriteString("Use esta memória para alinhar respostas às prioridades e não contradizer decisões já tomadas (cite-as quando relevante).\n</memoria>")
	return b.String()
}
