package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

type fakeNotifier struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeNotifier) Notify(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeNotifier) SendDocument(context.Context, int64, string, io.Reader, string) error {
	return nil
}
func (f *fakeNotifier) AllowedIDs() []int64 { return nil }

func TestNewRoutines(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := agent.New(cfg, db, llm.NewManager(cfg, nil), log)
	n := &fakeNotifier{}
	d := &Deps{Cfg: cfg, DB: db, Agent: ag, Notifier: n, Log: log}
	ctx := context.Background()

	// Tasks from e-mails (triaged at sync time) show up in the twice-a-day digest.
	note, _, _ := ag.Ingest(ctx, agent.IngestInput{Title: "📧 Proposta", Content: "x", Source: "gmail", SourceRef: "m1"})
	task, _, _ := ag.Ingest(ctx, agent.IngestInput{Type: database.TypeTask, Title: "Responder proposta", Source: "gmail", SourceRef: "m1#0"})
	db.AddEdge(ctx, task.ID, note.ID, "derived_from", 1)
	text, err := d.ActionItems(ctx, true)
	if err != nil || !strings.Contains(text, "Responder proposta") || !strings.Contains(text, "← 📧 Proposta") || len(n.sent) != 1 {
		t.Fatalf("digest = %q, %v, sent %d", text, err, len(n.sent))
	}
	// Second run: the cursor moved, nothing new, no message.
	if text, err := d.ActionItems(ctx, true); err != nil || text != "" || len(n.sent) != 1 {
		t.Fatalf("second digest = %q, %v", text, err)
	}

	// Memory without an LLM is a no-op that still moves the cursor.
	if text, err := d.MemoryRun(ctx, true); err != nil || text != "" {
		t.Fatalf("memory = %q, %v", text, err)
	}
	if _, ok, _ := db.KVGet(ctx, "memory.cursor"); !ok {
		t.Error("memory cursor not saved")
	}

	// Weekly cleanup notifies with the counts and the review link.
	old := time.Now().AddDate(0, 0, -40)
	stale := &database.Node{Type: database.TypeTask, Title: "Esquecida", CreatedAt: old}
	db.CreateNode(ctx, stale)
	db.ExecContext(ctx, `UPDATE nodes SET updated_at = created_at WHERE id = ?`, stale.ID)
	text, err = d.Cleanup(ctx, true)
	if err != nil || !strings.Contains(text, "1 sugestões (1 tarefas paradas)") || !strings.Contains(text, "/content/cleanup") || len(n.sent) != 2 {
		t.Fatalf("cleanup = %q, %v", text, err)
	}

	// The new jobs are registered with their defaults.
	s := New(time.UTC, log)
	if err := Register(s, d); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, j := range s.Jobs() {
		got[j.Name] = j.Spec
	}
	for name, spec := range map[string]string{"actions": "0 12,18 * * *", "memory": "30 22 * * *", "cleanup": "30 17 * * 0"} {
		if got[name] != spec {
			t.Errorf("%s = %q", name, got[name])
		}
	}
}
