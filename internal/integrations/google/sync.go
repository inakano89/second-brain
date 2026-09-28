package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/queue"
)

// Task kinds.
const (
	TaskCalendarSync = "google.calendar.sync"
	TaskGmailSync    = "google.gmail.sync"
)

// Syncer imports calendar events and actionable e-mails into the graph.
type Syncer struct {
	g  *Client
	ag *agent.Agent
}

// NewSyncer creates a syncer.
func NewSyncer(g *Client, ag *agent.Agent) *Syncer { return &Syncer{g: g, ag: ag} }

// RegisterTasks wires queue handlers.
func (s *Syncer) RegisterTasks(w *queue.Worker) {
	w.Handle(TaskCalendarSync, func(ctx context.Context, _ json.RawMessage) error {
		n, err := s.SyncCalendar(ctx)
		if err == nil && n > 0 {
			s.g.log.Info("calendar sincronizado", "events", n)
		}
		return err
	})
	w.Handle(TaskGmailSync, func(ctx context.Context, _ json.RawMessage) error {
		n, err := s.SyncGmail(ctx)
		if err == nil && n > 0 {
			s.g.log.Info("gmail sincronizado", "items", n)
		}
		return err
	})
}

// SyncCalendar mirrors events from yesterday to +14 days as event nodes.
func (s *Syncer) SyncCalendar(ctx context.Context) (int, error) {
	if !s.g.Connected() {
		return 0, nil
	}
	now := time.Now()
	evs, err := s.g.ListEvents(ctx, now.AddDate(0, 0, -1), now.AddDate(0, 0, 14))
	if err != nil {
		return 0, err
	}
	for _, ev := range evs {
		if _, err := s.ag.UpsertEventNode(ctx, ev); err != nil {
			return 0, err
		}
	}
	return len(evs), nil
}

type emailAnalysis struct {
	Summary  string `json:"summary"`
	Priority string `json:"priority"`
	Actions  []struct {
		Title string `json:"title"`
		Due   string `json:"due"`
	} `json:"actions"`
}

// SyncGmail processes unread priority mail (pending actions → tasks) and newsletters (→ articles).
func (s *Syncer) SyncGmail(ctx context.Context) (int, error) {
	if !s.g.Connected() {
		return 0, nil
	}
	cfg := s.g.cfg
	total := 0
	if q := cfg.Get("GMAIL_QUERY"); q != "" {
		n, err := s.process(ctx, q, "gmail", s.handlePriority)
		if err != nil {
			return total, err
		}
		total += n
	}
	if q := cfg.Get("GMAIL_NEWSLETTER_QUERY"); q != "" {
		n, err := s.process(ctx, q, "newsletter", s.handleNewsletter)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *Syncer) process(ctx context.Context, query, ns string, fn func(context.Context, *Email) error) (int, error) {
	ids, err := s.g.ListMessageIDs(ctx, query, 25)
	if err != nil {
		return 0, err
	}
	db := s.ag.DB()
	var fresh []string
	for _, id := range ids {
		if seen, _ := db.Seen(ctx, ns, id); !seen {
			fresh = append(fresh, id)
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	var mu sync.Mutex
	count := 0
	for _, id := range fresh {
		g.Go(func() error {
			e, err := s.g.GetMessage(gctx, id)
			if err != nil {
				return err
			}
			if err := fn(gctx, e); err != nil {
				return err
			}
			_, err = db.MarkSeen(gctx, ns, id)
			mu.Lock()
			count++
			mu.Unlock()
			return err
		})
	}
	return count, g.Wait()
}

func (s *Syncer) handlePriority(ctx context.Context, e *Email) error {
	loc := s.g.cfg.Location()
	var an emailAnalysis
	if s.ag.LLM().Enabled() {
		err := s.ag.LLM().CompleteJSON(ctx, "", llm.Request{
			Purpose: "gmail", MaxTokens: 2000, Effort: "low",
			System: `Você tria e-mails. Responda SOMENTE JSON: {"summary":"1-2 frases","priority":"high|normal|low","actions":[{"title":"ação pendente para o destinatário","due":"YYYY-MM-DD ou vazio"}]}.
Inclua ações apenas se o e-mail exigir algo do destinatário. Ignore marketing.`,
			Messages: []llm.Message{{Role: llm.RoleUser, Content: fmt.Sprintf("Data de hoje: %s\nDe: %s\nAssunto: %s\n\n%s", time.Now().In(loc).Format("2006-01-02"), e.From, e.Subject, extract.Truncate(e.Body, 8000))}},
		}, &an)
		if err != nil {
			return err
		}
	} else {
		an.Summary, an.Priority = e.Snippet, "normal"
	}
	if len(an.Actions) == 0 && an.Priority != "high" {
		return nil
	}
	content := fmt.Sprintf("**De:** %s\n**Data:** %s\n**Link:** %s\n\n%s\n\n---\n%s", e.From, e.Date.In(loc).Format("02/01/2006 15:04"), e.Link, an.Summary, extract.Truncate(e.Body, 6000))
	note, _, err := s.ag.Ingest(ctx, agent.IngestInput{
		Type: database.TypeNote, Title: "📧 " + e.Subject, Content: content, Summary: an.Summary, Tags: []string{"email"},
		Source: "gmail", SourceRef: e.ID, CreatedAt: e.Date, Meta: map[string]any{"from": e.From, "link": e.Link, "priority": an.Priority, "enriched": true},
	})
	if err != nil {
		return err
	}
	for i, a := range an.Actions {
		if strings.TrimSpace(a.Title) == "" {
			continue
		}
		var due *time.Time
		if d, err := time.ParseInLocation("2006-01-02", a.Due, loc); err == nil {
			due = &d
		}
		t, _, err := s.ag.Ingest(ctx, agent.IngestInput{
			Type: database.TypeTask, Title: a.Title, Content: "Origem: " + e.Subject + "\n" + e.Link, DueAt: due, Tags: []string{"email"},
			Source: "gmail", SourceRef: fmt.Sprintf("%s#%d", e.ID, i), Meta: map[string]any{"email_id": e.ID},
		})
		if err != nil {
			return err
		}
		_ = s.ag.DB().AddEdge(ctx, t.ID, note.ID, "derived_from", 1)
	}
	return s.ag.QueueEnrich(ctx, note.ID)
}

func (s *Syncer) handleNewsletter(ctx context.Context, e *Email) error {
	summary := e.Snippet
	if s.ag.LLM().Enabled() {
		text, err := s.ag.Ask(ctx, "newsletter", "Resuma a newsletter em tópicos objetivos (máx. 8 bullets), destacando fatos e links relevantes. Mantenha o idioma original.",
			fmt.Sprintf("Assunto: %s\nDe: %s\n\n%s", e.Subject, e.From, extract.Truncate(e.Body, 20000)))
		if err != nil {
			return err
		}
		summary = text
	}
	_, _, err := s.ag.Ingest(ctx, agent.IngestInput{
		Type: database.TypeArticle, Title: e.Subject, Content: summary + "\n\n---\n" + extract.Truncate(e.Body, 20000), Summary: extract.Truncate(summary, 400),
		Tags: []string{"newsletter"}, Source: "newsletter", SourceRef: e.ID, CreatedAt: e.Date, Meta: map[string]any{"from": e.From, "link": e.Link}, Enrich: true,
	})
	return err
}
