// Package agent is the cognitive core: ingestion, auto-tagging, auto-linking,
// hybrid search, RAG chat with function calling and controlled host actions.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/profile"
	"github.com/inakano89/second-brain/internal/queue"
)

// Task kinds handled by the agent.
const (
	TaskEnrich    = "node.enrich"
	TaskClipFetch = "clip.fetch"
	TaskReembed   = "node.reembed"
)

// MetaNoLLM marks nodes whose enrichment must use offline heuristics only (bulk imports).
const MetaNoLLM = "no_llm"

// Agent orchestrates knowledge processing.
type Agent struct {
	cfg     *config.Config
	db      *database.DB
	llm     *llm.Manager
	log     *slog.Logger
	actions *Actions
	profile *profile.Store

	gMu sync.RWMutex
	g   GoogleAPI

	qcacheMu sync.Mutex
	qcache   map[string][]float32
	qorder   []string
}

// New creates an Agent.
func New(cfg *config.Config, db *database.DB, m *llm.Manager, log *slog.Logger) *Agent {
	a := &Agent{cfg: cfg, db: db, llm: m, log: log.With("component", "agent"), qcache: map[string][]float32{}}
	a.actions = NewActions(cfg, log)
	a.profile = profile.New(cfg, db, log)
	return a
}

// Profile exposes the personal profile store.
func (a *Agent) Profile() *profile.Store { return a.profile }

// DB exposes the database.
func (a *Agent) DB() *database.DB { return a.db }

// LLM exposes the LLM manager.
func (a *Agent) LLM() *llm.Manager { return a.llm }

// Actions exposes the host action registry.
func (a *Agent) Actions() *Actions { return a.actions }

func (a *Agent) calendar() GoogleAPI { return a.google(GoogleCalendar) }

// Location returns the configured timezone.
func (a *Agent) Location() *time.Location { return a.cfg.Location() }

// RegisterTasks wires queue handlers.
func (a *Agent) RegisterTasks(w *queue.Worker) {
	w.Handle(TaskEnrich, func(ctx context.Context, p json.RawMessage) error {
		var in struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(p, &in); err != nil {
			return queue.Permanent(err)
		}
		return a.Enrich(ctx, in.ID)
	})
	w.Handle(TaskClipFetch, func(ctx context.Context, p json.RawMessage) error {
		var in struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(p, &in); err != nil {
			return queue.Permanent(err)
		}
		return a.fetchClip(ctx, in.ID)
	})
	w.Handle(TaskReembed, func(ctx context.Context, _ json.RawMessage) error {
		_, err := a.ReembedMissing(ctx, 200)
		return err
	})
}

// IngestInput describes new content entering the brain.
type IngestInput struct {
	Type      string
	Title     string
	Content   string
	Summary   string
	Source    string
	SourceRef string
	Status    string
	Tags      []string
	Meta      map[string]any
	DueAt     *time.Time
	CreatedAt time.Time
	Enrich    bool
}

// explicitSources are channels where the user sends content by hand: a deleted item sent
// again comes back. Everything else (integrations, imports, the agent) returns
// database.ErrDeleted for items the user deleted with "não trazer de volta".
var explicitSources = map[string]bool{"telegram": true, "voice": true, "web": true, "api": true, "clip": true, "watcher": true}

// Ingest stores a node (upserting by source ref) and optionally queues enrichment.
func (a *Agent) Ingest(ctx context.Context, in IngestInput) (*database.Node, bool, error) {
	if in.Type == "" {
		in.Type = database.TypeNote
	}
	if strings.TrimSpace(in.Title) == "" {
		in.Title = extract.FirstLine(in.Content)
	}
	if in.Meta == nil {
		in.Meta = map[string]any{}
	}
	if in.SourceRef != "" {
		gone, err := a.db.IsDeletedRef(ctx, in.Source, in.SourceRef)
		if err != nil {
			return nil, false, err
		}
		if gone && !explicitSources[in.Source] {
			return nil, false, database.ErrDeleted
		}
		if gone { // sent again on purpose: it may come back
			if err := a.db.ForgetDeletedRef(ctx, in.Source, in.SourceRef); err != nil {
				return nil, false, err
			}
		}
	}
	n := &database.Node{
		Type: in.Type, Title: extract.Truncate(strings.TrimSpace(in.Title), 200), Content: in.Content, Summary: in.Summary,
		Source: in.Source, SourceRef: in.SourceRef, Status: in.Status, Tags: in.Tags, Meta: in.Meta, DueAt: in.DueAt, CreatedAt: in.CreatedAt,
	}
	created, err := a.db.UpsertBySource(ctx, n)
	if err != nil {
		return nil, false, err
	}
	if in.Enrich {
		if err := a.QueueEnrich(ctx, n.ID); err != nil {
			a.log.Warn("falha ao enfileirar enriquecimento", "id", n.ID, "err", err)
		}
	}
	if created {
		a.log.Info("nó criado", "id", n.ID, "type", n.Type, "source", n.Source, "title", n.Title)
	}
	return n, created, nil
}

// QueueEnrich schedules enrichment for a node.
func (a *Agent) QueueEnrich(ctx context.Context, id int64) error {
	_, err := a.db.Enqueue(ctx, TaskEnrich, map[string]int64{"id": id}, database.EnqueueOpts{DedupeKey: fmt.Sprintf("enrich:%d", id)})
	return err
}

type entity struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type extractedTask struct {
	Title string `json:"title"`
	Due   string `json:"due"`
}

type enrichResult struct {
	Summary  string          `json:"summary"`
	Tags     []string        `json:"tags"`
	Entities []entity        `json:"entities"`
	Tasks    []extractedTask `json:"tasks"`
	TypeHint string          `json:"type_hint"`
	Title    string          `json:"title"`
}

const enrichSystem = `Você é o motor de indexação de um "Second Brain" pessoal.
Analise o conteúdo e responda SOMENTE com JSON no formato:
{"title":"título curto e descritivo (máx 80 chars)","summary":"resumo objetivo em 1-3 frases","tags":["até 8 tags em minúsculas, sem #, use hífen para compostos"],
"entities":[{"name":"nome próprio","kind":"person|organization|place|project|concept"}],
"tasks":[{"title":"ação concreta e pendente explicitamente mencionada","due":"YYYY-MM-DD ou vazio"}],
"type_hint":"note|task|event|insight|article"}
Regras: não invente fatos; tarefas apenas se houver ação pendente clara; mantenha o idioma original do conteúdo.`

var taskSources = map[string]bool{"telegram": true, "voice": true, "web": true, "watcher": true, "gmail": true}

// Enrich runs auto-tagging, entity extraction, embedding and auto-linking for a node.
func (a *Agent) Enrich(ctx context.Context, id int64) error {
	n, err := a.db.GetNode(ctx, id)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	alreadyEnriched, _ := n.Meta["enriched"].(bool)
	var res enrichResult
	if !alreadyEnriched {
		res, err = a.analyze(ctx, n)
		if err != nil {
			return err
		}
		a.applyAnalysis(n, res)
		n.Meta["enriched"] = true
		n.Meta["enriched_at"] = time.Now().UTC().Format(time.RFC3339)
		if err := a.db.UpdateNode(ctx, n); err != nil {
			return err
		}
	}

	// Linking and embedding run concurrently.
	g, gctx := errgroup.WithContext(ctx)
	if !alreadyEnriched {
		g.Go(func() error { return a.linkEntities(gctx, n, res.Entities) })
		g.Go(func() error { return a.extractTasks(gctx, n, res.Tasks) })
	}
	g.Go(func() error { return a.linkWiki(gctx, n) })
	g.Go(func() error { return a.linkByTags(gctx, n) })
	g.Go(func() error { return a.embedAndLink(gctx, n) })
	if err := g.Wait(); err != nil {
		return err
	}
	a.log.Debug("nó enriquecido", "id", n.ID, "tags", n.Tags)
	return nil
}

func nodeText(n *database.Node, limit int) string {
	var b strings.Builder
	b.WriteString(n.Title)
	if n.Summary != "" {
		b.WriteString("\n" + n.Summary)
	}
	if n.Content != "" {
		b.WriteString("\n\n" + n.Content)
	}
	return extract.Truncate(b.String(), limit)
}

func (a *Agent) analyze(ctx context.Context, n *database.Node) (enrichResult, error) {
	if noLLM, _ := n.Meta[MetaNoLLM].(bool); noLLM || !a.llm.Enabled() || n.Type == database.TypeHealth || n.Type == database.TypePerson {
		return heuristicAnalysis(n), nil
	}
	var res enrichResult
	loc := a.cfg.Location()
	prompt := fmt.Sprintf("Data atual: %s\nTipo atual: %s\nFonte: %s\n\nConteúdo:\n%s", time.Now().In(loc).Format("2006-01-02 (Monday)"), n.Type, n.Source, nodeText(n, 12000))
	err := a.llm.CompleteJSON(ctx, "", llm.Request{
		Purpose: "enrich", System: enrichSystem, MaxTokens: 4000, Effort: "low",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}},
	}, &res)
	if err != nil {
		if llm.IsRetryable(err) {
			return res, err
		}
		a.log.Warn("enriquecimento LLM falhou, usando heurística", "id", n.ID, "err", err)
		return heuristicAnalysis(n), nil
	}
	return res, nil
}

func (a *Agent) applyAnalysis(n *database.Node, res enrichResult) {
	if n.Summary == "" && res.Summary != "" {
		n.Summary = strings.TrimSpace(res.Summary)
	}
	if quick, _ := n.Meta["quick"].(bool); quick && res.Title != "" {
		n.Title = extract.Truncate(res.Title, 120)
	}
	n.Tags = database.NormalizeTags(append(n.Tags, res.Tags...))
	if quick, _ := n.Meta["quick"].(bool); quick && n.Type == database.TypeNote {
		switch res.TypeHint {
		case database.TypeTask:
			n.Type, n.Status = database.TypeTask, database.StatusOpen
		case database.TypeInsight:
			n.Type = database.TypeInsight
		}
	}
	if len(res.Entities) > 0 {
		var names []string
		for _, e := range res.Entities {
			names = append(names, e.Name)
		}
		n.Meta["entities"] = names
	}
}

func (a *Agent) linkEntities(ctx context.Context, n *database.Node, ents []entity) error {
	for _, e := range ents {
		name := strings.TrimSpace(e.Name)
		if len([]rune(name)) < 2 || strings.EqualFold(name, n.Title) {
			continue
		}
		if e.Kind == "person" {
			p, err := a.db.FindByTitle(ctx, database.TypePerson, name)
			if errors.Is(err, database.ErrNotFound) {
				if gone, _ := a.db.IsDeletedRef(ctx, database.AutoPersonSource, strings.ToLower(name)); gone {
					continue
				}
				p = &database.Node{Type: database.TypePerson, Title: name, Source: "agent", Meta: map[string]any{"auto": true}}
				if err := a.db.CreateNode(ctx, p); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if err := a.db.AddEdge(ctx, n.ID, p.ID, "mentions", 1); err != nil {
				return err
			}
			continue
		}
		if other, err := a.db.FindByTitle(ctx, "", name); err == nil && other.ID != n.ID {
			if err := a.db.AddEdge(ctx, n.ID, other.ID, "mentions", 0.8); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Agent) extractTasks(ctx context.Context, n *database.Node, tasks []extractedTask) error {
	if !taskSources[n.Source] || n.Type == database.TypeTask {
		return nil
	}
	loc := a.cfg.Location()
	for i, t := range tasks {
		if i >= 5 || strings.TrimSpace(t.Title) == "" {
			break
		}
		var due *time.Time
		if d, err := time.ParseInLocation("2006-01-02", t.Due, loc); err == nil {
			due = &d
		}
		task, _, err := a.Ingest(ctx, IngestInput{
			Type: database.TypeTask, Title: t.Title, Source: "agent", SourceRef: fmt.Sprintf("task:%d:%d", n.ID, i),
			DueAt: due, Meta: map[string]any{"from_node": n.ID},
		})
		if errors.Is(err, database.ErrDeleted) {
			continue
		}
		if err != nil {
			return err
		}
		if err := a.db.AddEdge(ctx, task.ID, n.ID, "derived_from", 1); err != nil {
			return err
		}
	}
	return nil
}

var wikiRe = regexp.MustCompile(`\[\[([^\]|#]+)(?:[#|][^\]]*)?\]\]`)

// WikiLinks returns the [[targets]] referenced in text.
func WikiLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range wikiRe.FindAllStringSubmatch(text, -1) {
		t := strings.TrimSpace(m[1])
		if t != "" && !seen[strings.ToLower(t)] {
			seen[strings.ToLower(t)] = true
			out = append(out, t)
		}
	}
	return out
}

func (a *Agent) linkWiki(ctx context.Context, n *database.Node) error {
	for _, t := range WikiLinks(n.Content) {
		if other, err := a.db.FindByTitle(ctx, "", t); err == nil {
			if err := a.db.AddEdge(ctx, n.ID, other.ID, "links_to", 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Agent) linkByTags(ctx context.Context, n *database.Node) error {
	if len(n.Tags) < 2 {
		return nil
	}
	rel, err := a.db.RelatedByTags(ctx, n.ID, n.Tags, 2, 5)
	if err != nil {
		return err
	}
	for _, r := range rel {
		if err := a.db.AddEdge(ctx, n.ID, r.ID, "shares_tags", 0.5); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) threshold(model string) float64 {
	if v := a.cfg.GetFloat("AUTOLINK_THRESHOLD", 0); v > 0 {
		return v
	}
	if strings.HasPrefix(model, "local:") {
		return 0.45
	}
	return 0.72
}

func (a *Agent) embedAndLink(ctx context.Context, n *database.Node) error {
	vecs, model, err := a.llm.Embed(ctx, []string{nodeText(n, 6000)})
	if err != nil {
		if llm.IsRetryable(err) {
			return err
		}
		a.log.Warn("embedding falhou", "id", n.ID, "err", err)
		return nil
	}
	if err := a.db.SaveEmbedding(ctx, n.ID, model, vecs[0]); err != nil {
		return err
	}
	if n.Type == database.TypeHealth {
		return nil
	}
	th := a.threshold(model)
	for _, h := range a.db.VectorSearch(model, vecs[0], 6, map[int64]bool{n.ID: true}) {
		if h.Score < th {
			break
		}
		if err := a.db.AddEdge(ctx, n.ID, h.ID, "related", h.Score); err != nil {
			return err
		}
	}
	return nil
}

// ReembedMissing embeds nodes lacking a vector for the active model (goroutine batches).
func (a *Agent) ReembedMissing(ctx context.Context, limit int) (int, error) {
	model := a.llm.EmbedModel()
	ids, err := a.db.NodesWithoutEmbedding(ctx, model, limit)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	nodes, err := a.db.GetNodes(ctx, ids)
	if err != nil {
		return 0, err
	}
	const batch = 32
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	var mu sync.Mutex
	done := 0
	for start := 0; start < len(nodes); start += batch {
		chunk := nodes[start:min(start+batch, len(nodes))]
		g.Go(func() error {
			texts := make([]string, len(chunk))
			for i := range chunk {
				texts[i] = nodeText(&chunk[i], 6000)
			}
			vecs, m, err := a.llm.Embed(gctx, texts)
			if err != nil {
				return err
			}
			for i := range chunk {
				if err := a.db.SaveEmbedding(gctx, chunk[i].ID, m, vecs[i]); err != nil {
					return err
				}
			}
			mu.Lock()
			done += len(chunk)
			mu.Unlock()
			return nil
		})
	}
	err = g.Wait()
	return done, err
}

// ---- heuristic fallback (no LLM) ----

var sentenceRe = regexp.MustCompile(`(?s)^(.{20,280}?[.!?])(\s|$)`)
var todoRe = regexp.MustCompile(`(?mi)^\s*(?:- \[ \]|todo:|\[ \]|fazer:)\s*(.+)$`)
var hashtagRe = regexp.MustCompile(`(?:^|\s)#([\p{L}\p{N}_\-/]{2,40})`)

func heuristicAnalysis(n *database.Node) enrichResult {
	var res enrichResult
	body := strings.TrimSpace(n.Content)
	if m := sentenceRe.FindStringSubmatch(body); m != nil {
		res.Summary = strings.TrimSpace(m[1])
	} else {
		res.Summary = extract.Truncate(body, 200)
	}
	for _, m := range hashtagRe.FindAllStringSubmatch(body, -1) {
		res.Tags = append(res.Tags, m[1])
	}
	freq := map[string]int{}
	for _, t := range llm.Tokenize(n.Title + " " + n.Title + " " + body) {
		if len([]rune(t)) > 3 && !isNumeric(t) {
			freq[t]++
		}
	}
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range freq {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v == kvs[j].v {
			return kvs[i].k < kvs[j].k
		}
		return kvs[i].v > kvs[j].v
	})
	for i := 0; i < len(kvs) && i < 5; i++ {
		if kvs[i].v >= 2 {
			res.Tags = append(res.Tags, kvs[i].k)
		}
	}
	for _, m := range todoRe.FindAllStringSubmatch(body, 5) {
		res.Tasks = append(res.Tasks, extractedTask{Title: strings.TrimSpace(m[1])})
	}
	return res
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
