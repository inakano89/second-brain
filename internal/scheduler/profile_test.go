package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/profile"
)

func TestRemindersAndPersonalBlock(t *testing.T) {
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
	loc := cfg.Location()
	now := time.Now().In(loc)

	med := profile.Item{Kind: "medication", Title: "Losartana", Sensitive: true, Values: map[string]string{
		"dose": "50 mg", "times": now.Add(-2 * time.Minute).Format("15:04"), "stock": "3"}}
	if err := ag.Profile().Save(ctx, &med); err != nil {
		t.Fatal(err)
	}
	db.KVSet(ctx, "reminders.cursor", now.Add(-10*time.Minute).UTC().Format(time.RFC3339Nano))
	if err := d.Reminders(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 1 || !strings.Contains(n.sent[0], "Losartana") || !strings.Contains(n.sent[0], "/tomei") {
		t.Fatalf("reminder: %q", n.sent)
	}
	if err := d.Reminders(ctx); err != nil || len(n.sent) != 1 {
		t.Fatalf("reminder repeated: %q %v", n.sent, err)
	}

	// The evening report carries the personal block, but the saved note (read by the AI) does not.
	text, err := d.EveningReview(ctx, true)
	if err != nil || !strings.Contains(text, "Amanhã e próximos 7 dias") || !strings.Contains(text, "Estoque") {
		t.Fatalf("evening = %q, %v", text, err)
	}
	notes, _ := db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeInsight}, Limit: 10})
	for _, nd := range notes {
		if strings.Contains(nd.Content, "Losartana") {
			t.Fatalf("personal data saved in a node: %q", nd.Content)
		}
	}
}
