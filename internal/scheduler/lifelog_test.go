package scheduler

import (
	"context"
	"fmt"
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

func newDeps(t *testing.T) (*Deps, *fakeNotifier) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := &fakeNotifier{}
	return &Deps{Cfg: cfg, DB: db, Agent: agent.New(cfg, db, llm.NewManager(cfg, nil), log), Notifier: n, Log: log}, n
}

func TestGoalsBlockCrossesTasks(t *testing.T) {
	d, _ := newDeps(t)
	ctx := context.Background()
	loc := d.Cfg.Location()
	soon := time.Now().In(loc).AddDate(0, 0, 20).Format("2006-01-02")
	goal := profile.Item{Kind: "goal", Title: "Publicar livro de receitas", Values: map[string]string{"deadline": soon, "progress": "20", "next": "Fechar o sumário"}}
	if err := d.Agent.Profile().Save(ctx, &goal); err != nil {
		t.Fatal(err)
	}
	done := profile.Item{Kind: "goal", Title: "Ler 12 livros", Values: map[string]string{"progress": "100"}}
	d.Agent.Profile().Save(ctx, &done)
	d.Agent.Ingest(ctx, agent.IngestInput{Type: database.TypeTask, Title: "Escrever capítulo do livro de receitas", Source: "web"})
	t2, _, _ := d.Agent.Ingest(ctx, agent.IngestInput{Type: database.TypeTask, Title: "Testar receitas do livro", Source: "web"})
	d.DB.SetStatus(ctx, t2.ID, database.StatusDone)
	d.Agent.Ingest(ctx, agent.IngestInput{Type: database.TypeTask, Title: "Comprar pão", Source: "web"})

	block := d.goalsBlock(ctx)
	for _, want := range []string{"🎯 *Metas*", "*Publicar livro de receitas* — 20%", "faltam 20 dias e o progresso está baixo", "1 abertas, 1 concluídas (1 na semana)", "próximo passo: Fechar o sumário", "*Ler 12 livros* — 100% ✅"} {
		if !strings.Contains(block, want) {
			t.Errorf("missing %q in:\n%s", want, block)
		}
	}
	if d.goalsBlock(ctx) == "" {
		t.Fatal("block should be stable")
	}
	// The personal block is appended only to the notification, never to the saved note.
	text, err := d.WeeklyReview(ctx, true)
	if err != nil || !strings.Contains(text, "Publicar livro de receitas") {
		t.Fatalf("weekly = %q, %v", text, err)
	}
	saved, _ := d.DB.GetNodeBySource(ctx, "routine", "weekly:"+weekRef(time.Now().In(loc)))
	if saved == nil || strings.Contains(saved.Content, "Publicar livro") {
		t.Fatalf("goals leaked into the saved note: %+v", saved)
	}
}

func weekRef(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

func TestReviewDiaryAndYearJobs(t *testing.T) {
	d, n := newDeps(t)
	ctx := context.Background()
	d.Agent.Ingest(ctx, agent.IngestInput{Type: database.TypeInsight, Title: "Meu insight", Content: "Feito é melhor que perfeito.", Source: "web"})

	text, err := d.Review(ctx, true)
	if err != nil || !strings.Contains(text, "Feito é melhor que perfeito.") || len(n.sent) != 1 {
		t.Fatalf("review = %q, %v", text, err)
	}
	if text, _ := d.Review(ctx, true); text != "" || len(n.sent) != 1 { // rescheduled for tomorrow
		t.Fatalf("second review = %q", text)
	}
	d.Cfg.Update(map[string]string{"REVIEW_PER_DAY": "0"})
	if text, _ := d.Review(ctx, true); text != "" {
		t.Fatalf("disabled review = %q", text)
	}

	text, err = d.Diary(ctx, true)
	if err != nil || !strings.Contains(text, "Diário de hoje") || len(n.sent) != 2 {
		t.Fatalf("diary = %q, %v", text, err)
	}
	for _, ans := range []string{"a", "b", "c"} {
		if _, err := d.Agent.DiaryAnswer(ctx, ans); err != nil {
			t.Fatal(err)
		}
	}
	if text, _ := d.Diary(ctx, true); text != "" { // already written today
		t.Fatalf("diary twice = %q", text)
	}

	// Year review: nothing to summarise is quiet; with data it is sent once.
	if text, err := d.YearReview(ctx, true); err != nil || text != "" {
		t.Fatalf("empty year = %q, %v", text, err)
	}
	last := time.Now().Year() - 1
	d.DB.CreateNode(ctx, &database.Node{Type: database.TypeNote, Title: "Do ano passado", Content: "x", CreatedAt: time.Date(last, 6, 1, 12, 0, 0, 0, time.UTC)})
	text, err = d.YearReview(ctx, true)
	if err != nil || !strings.Contains(text, "Retrospectiva") || len(n.sent) != 3 {
		t.Fatalf("year = %q, %v", text, err)
	}
	if text, _ := d.YearReview(ctx, true); text != "" || len(n.sent) != 3 {
		t.Fatalf("year twice = %q", text)
	}
}

func TestMorningBriefingHasOnThisDay(t *testing.T) {
	d, _ := newDeps(t)
	ctx := context.Background()
	old := time.Now().In(d.Cfg.Location()).AddDate(-1, 0, 0)
	d.DB.CreateNode(ctx, &database.Node{Type: database.TypeNote, Title: "Uma lembrança", Content: "x", CreatedAt: old})
	text, err := d.MorningBriefing(ctx, false)
	if err != nil || !strings.Contains(text, "Neste dia, em anos anteriores") || !strings.Contains(text, "Uma lembrança") {
		t.Fatalf("briefing = %q, %v", text, err)
	}
}
