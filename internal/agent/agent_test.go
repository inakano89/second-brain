package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

// fakeLLM is an OpenAI-compatible server answering by the system prompt of each request.
type fakeLLM struct {
	mu      sync.Mutex
	systems []string
	answer  func(system, user string) string
}

func (f *fakeLLM) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		var system, user string
		for _, m := range body.Messages {
			switch m.Role {
			case "system":
				system = fmt.Sprint(m.Content)
			case "user":
				user = fmt.Sprint(m.Content)
			}
		}
		f.mu.Lock()
		f.systems = append(f.systems, system)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": f.answer(system, user)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupAgent(t *testing.T, f *fakeLLM) (*Agent, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	env := ""
	if f != nil {
		env = fmt.Sprintf("OPENAI_API_KEY=k\nOPENAI_BASE_URL=%s\nOPENAI_MODEL=gpt-x\nDEFAULT_LLM_PROVIDER=openai\nEMBEDDING_PROVIDER=local\nLLM_ROUTE_ACTIONS=openai\nLLM_ROUTE_MEMORY=openai\nLLM_ROUTE_CHAT=openai\n", f.server(t).URL)
	}
	os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600)
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(cfg, db, llm.NewManager(cfg, nil), slog.New(slog.NewTextHandler(io.Discard, nil))), db
}

var idRe = regexp.MustCompile(`source_id=(\d+)`)

func TestIsMeeting(t *testing.T) {
	long := strings.Repeat("Falamos sobre o projeto. ", 30)
	cases := []struct {
		n    database.Node
		want bool
	}{
		{database.Node{Type: database.TypeNote, Title: "Reunião de planejamento", Content: long}, true},
		{database.Node{Type: database.TypeNote, Title: "Meeting notes – Q4", Content: long}, true},
		{database.Node{Type: database.TypeNote, Title: "Anotações", Tags: []string{"ata"}, Content: long}, true},
		{database.Node{Type: database.TypeNote, Title: "Reunião", Content: "curta"}, false},
		{database.Node{Type: database.TypeNote, Title: "Receita de pão", Content: long}, false},
		{database.Node{Type: database.TypeArticle, Title: "Como fazer reuniões melhores", Source: "rss", Content: long}, false},
		{database.Node{Type: database.TypeEvent, Title: "Almoço", Content: long}, true},
		{database.Node{Type: database.TypeNote, Title: "Áudio", Tags: []string{"voz"}, Content: long}, true},
		{database.Node{Type: database.TypeTask, Title: "Reunião com Ana", Content: long}, false},
	}
	for _, c := range cases {
		if got := IsMeeting(&c.n); got != c.want {
			t.Errorf("%q: got %v", c.n.Title, got)
		}
	}
	if foldTitle("Enviar  a PROPOSTA, revisada!") != "enviar a proposta revisada" || !similarTitle("enviar proposta revisada ao cliente", "enviar proposta revisada") {
		t.Error("title folding")
	}
}

func TestExtractMeetingTasks(t *testing.T) {
	f := &fakeLLM{}
	f.answer = func(system, user string) string {
		if !strings.Contains(system, "AÇÕES COMBINADAS") {
			return "{}"
		}
		var tasks []map[string]any
		for _, m := range idRe.FindAllStringSubmatch(user, -1) {
			var id int64
			fmt.Sscan(m[1], &id)
			tasks = append(tasks,
				map[string]any{"source_id": id, "title": "Enviar proposta revisada", "owner": "me", "due": "2026-10-02", "context": "Combinado com o cliente."},
				map[string]any{"source_id": id, "title": "Revisar contrato", "owner": "Ana", "context": "Ana ficou com o jurídico."},
				map[string]any{"source_id": id, "title": "Pagar a conta de luz", "owner": "me"}, // already open
				map[string]any{"source_id": 999999, "title": "Inventada", "owner": "me"},        // not a meeting of this batch
			)
		}
		b, _ := json.Marshal(map[string]any{"tasks": tasks})
		return string(b)
	}
	a, db := setupAgent(t, f)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	meeting, _, err := a.Ingest(ctx, IngestInput{Title: "Reunião com cliente ACME", Content: strings.Repeat("Discutimos escopo e prazos. ", 20), Source: "web"})
	if err != nil {
		t.Fatal(err)
	}
	a.Ingest(ctx, IngestInput{Type: database.TypeTask, Title: "Pagar a conta de luz", Source: "web"})
	a.Ingest(ctx, IngestInput{Title: "Lista de compras", Content: strings.Repeat("arroz feijão ", 40), Source: "web"})
	emailNote, _, _ := a.Ingest(ctx, IngestInput{Title: "📧 Reunião de alinhamento", Content: strings.Repeat("texto ", 60), Source: "gmail", SourceRef: "m1"})
	emailTask, _, _ := a.Ingest(ctx, IngestInput{Type: database.TypeTask, Title: "Responder a Bia", Source: "gmail", SourceRef: "m1#0"})
	db.AddEdge(ctx, emailTask.ID, emailNote.ID, "derived_from", 1)

	rep, err := a.ExtractMeetingTasks(ctx, start, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 1 || len(rep.Created) != 2 { // the e-mail already has tasks; the grocery list is no meeting
		t.Fatalf("report = %+v", rep)
	}
	var mine, waiting *database.Node
	for i, n := range rep.Created {
		switch {
		case strings.HasPrefix(n.Title, "Aguardando Ana: "):
			waiting = &rep.Created[i]
		case n.Title == "Enviar proposta revisada":
			mine = &rep.Created[i]
		}
	}
	if mine == nil || waiting == nil || mine.DueAt == nil || mine.DueAt.In(a.Location()).Format("2006-01-02") != "2026-10-02" ||
		!strings.Contains(strings.Join(waiting.Tags, ","), "aguardando") || !strings.Contains(mine.Content, "[[Reunião com cliente ACME]]") {
		t.Fatalf("tasks = %+v", rep.Created)
	}
	if origins, _ := db.DerivedOrigins(ctx, []int64{mine.ID}); origins[mine.ID].ID != meeting.ID {
		t.Fatalf("origin = %+v", origins)
	}

	// Second run: the meeting was read and did not change.
	rep, err = a.ExtractMeetingTasks(ctx, start, time.Now().Add(time.Minute))
	if err != nil || rep.Scanned != 0 {
		t.Fatalf("second run = %+v, %v", rep, err)
	}
	fresh, err := a.NewAITasks(ctx, start, 10)
	if err != nil || len(fresh) != 3 { // two from the meeting + one from the e-mail
		t.Fatalf("new AI tasks = %d, %v", len(fresh), err)
	}
	for _, x := range fresh {
		if x.Origin == nil {
			t.Fatalf("task without origin: %+v", x.Task)
		}
	}

	// Without an LLM nothing is extracted.
	b, _ := setupAgent(t, nil)
	if rep, err := b.ExtractMeetingTasks(ctx, start, time.Now()); err != nil || rep.Scanned != 0 {
		t.Fatalf("no llm = %+v, %v", rep, err)
	}
}

func TestDistillMemoryAndChatContext(t *testing.T) {
	f := &fakeLLM{}
	var memoryPrompt string
	f.answer = func(system, user string) string {
		if !strings.Contains(system, "MEMÓRIA de longo prazo") {
			return "Olá!"
		}
		memoryPrompt = user
		ids := regexp.MustCompile(`\[id=(\d+)`).FindAllStringSubmatch(user, -1)
		src := []int64{}
		for _, m := range ids {
			var id int64
			fmt.Sscan(m[1], &id)
			src = append(src, id)
		}
		b, _ := json.Marshal(map[string]any{
			"decisions":      []any{map[string]any{"title": "Usar SQLite em vez de Postgres", "detail": "Menos operação.", "sources": src}},
			"learnings":      []any{map[string]any{"title": "Backups diários evitam dor de cabeça", "detail": "Aprendido no incidente.", "sources": []int64{999}}},
			"priorities":     []any{map[string]any{"title": "Lançar a v0.5", "why": "prometida para outubro"}, map[string]any{"title": "Correr 3x por semana"}},
			"open_questions": []any{map[string]any{"title": "Qual domínio usar?"}},
			"diary":          "Dia produtivo, com a decisão do banco.",
		})
		return string(b)
	}
	a, db := setupAgent(t, f)
	ctx := context.Background()
	from := time.Now().Add(-time.Hour)
	note, _, _ := a.Ingest(ctx, IngestInput{Title: "Arquitetura", Content: "Decidimos ficar no SQLite.", Source: "telegram", SourceRef: "tg:1"})
	a.Ingest(ctx, IngestInput{Type: database.TypeArticle, Title: "Notícia qualquer", Content: "rss", Source: "rss", SourceRef: "g1"})
	db.AppendChat(ctx, "web", llm.RoleUser, "vou priorizar a v0.5")

	rep, err := a.DistillMemory(ctx, from, time.Now().Add(time.Minute))
	if err != nil || rep == nil {
		t.Fatalf("memory = %+v, %v", rep, err)
	}
	if strings.Contains(memoryPrompt, "Notícia qualquer") || !strings.Contains(memoryPrompt, "vou priorizar a v0.5") {
		t.Fatalf("prompt = %s", memoryPrompt)
	}
	if len(rep.Decisions) != 1 || len(rep.Learnings) != 1 || rep.Priorities != 2 || rep.Snapshot == nil {
		t.Fatalf("report = %+v", rep)
	}
	d := rep.Decisions[0]
	if !strings.Contains(d.Content, "[[Arquitetura]]") || !strings.Contains(strings.Join(d.Tags, ","), TagDecision) {
		t.Fatalf("decision = %+v", d)
	}
	if links, _ := db.Neighbors(ctx, d.ID); len(links) != 2 { // source note + snapshot
		t.Fatalf("decision links = %+v", links)
	}
	if strings.Contains(rep.Learnings[0].Content, "Fontes") { // unknown source ids are dropped
		t.Fatalf("learning = %+v", rep.Learnings[0])
	}
	if !strings.Contains(rep.Snapshot.Content, "[[Usar SQLite em vez de Postgres]]") || !strings.Contains(rep.Snapshot.Content, "Qual domínio usar?") {
		t.Fatalf("snapshot = %s", rep.Snapshot.Content)
	}
	_ = note

	// Same day again: decisions and the snapshot are updated, not duplicated.
	if _, err := a.DistillMemory(ctx, from, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountNodes(ctx, database.NodeFilter{Source: MemorySource}); n != 4 { // decision, learning, priorities, snapshot
		t.Fatalf("memory nodes = %d", n)
	}

	v, err := a.Memory(ctx, 5)
	if err != nil || v.Priorities == nil || !strings.Contains(v.Priorities.Content, "Lançar a v0.5") || len(v.Decisions) != 1 {
		t.Fatalf("view = %+v, %v", v, err)
	}
	if _, err := a.Chat(ctx, ChatRequest{Channel: "web", Text: "o que decidi sobre o banco?", NoTools: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.systems[len(f.systems)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "<memoria>") || !strings.Contains(last, "Usar SQLite em vez de Postgres") || !strings.Contains(last, "Lançar a v0.5") {
		t.Fatalf("chat system prompt lacks memory:\n%s", last)
	}

	// Nothing new: no call, no snapshot.
	if rep, err := a.DistillMemory(ctx, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)); err != nil || rep != nil {
		t.Fatalf("empty period = %+v, %v", rep, err)
	}
}

func TestMemoryWindow(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Date(2026, 9, 28, 22, 30, 0, 0, loc)
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, loc)
	if got := MemoryWindow(time.Time{}, now, loc); !got.Equal(day) {
		t.Errorf("first run = %v", got)
	}
	if got := MemoryWindow(now.Add(-2*time.Hour), now, loc); !got.Equal(day) { // same day: redo the whole day
		t.Errorf("same day = %v", got)
	}
	if got := MemoryWindow(now.AddDate(0, 0, -3), now, loc); !got.Equal(now.AddDate(0, 0, -3)) {
		t.Errorf("missed days = %v", got)
	}
	if got := MemoryWindow(now.AddDate(0, 0, -30), now, loc); !got.Equal(day.AddDate(0, 0, -7)) {
		t.Errorf("capped = %v", got)
	}
}

func TestSuggestAndApplyCleanup(t *testing.T) {
	a, db := setupAgent(t, nil)
	ctx := context.Background()
	long := time.Now().AddDate(0, 0, -40)
	mk := func(in IngestInput) *database.Node {
		t.Helper()
		n, _, err := a.Ingest(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	d1 := mk(IngestInput{Type: database.TypeArticle, Title: "Go Docs", Content: "https://go.dev", Source: "import:bookmarks", SourceRef: "1", CreatedAt: long})
	d2 := mk(IngestInput{Type: database.TypeArticle, Title: "Go Docs", Content: "https://go.dev", Source: "import:bookmarks", SourceRef: "2"})
	stale := mk(IngestInput{Type: database.TypeTask, Title: "Tarefa esquecida", Source: "web", CreatedAt: long})
	db.ExecContext(ctx, `UPDATE nodes SET updated_at = created_at WHERE id = ?`, stale.ID)
	auto := &database.Node{Type: database.TypePerson, Title: "Beltrano", Source: "agent", CreatedAt: long}
	db.CreateNode(ctx, auto)
	// Near duplicates: same text, different titles, created recently.
	text := "Plano de treino: correr 5 km segunda, quarta e sexta, alongar depois."
	n1 := mk(IngestInput{Title: "Plano de treino", Content: text, Source: "web"})
	n2 := mk(IngestInput{Title: "Plano de treino", Content: text + " Beber água.", Source: "telegram", SourceRef: "tg:9"})
	// Daily reports look alike but are not duplicates.
	mk(IngestInput{Type: database.TypeInsight, Title: "Balanço", Content: text, Source: "routine", SourceRef: "evening:1"})
	mk(IngestInput{Type: database.TypeInsight, Title: "Balanço", Content: text + " Ok.", Source: "routine", SourceRef: "evening:2"})
	if _, err := a.ReembedMissing(ctx, 100); err != nil {
		t.Fatal(err)
	}

	counts, err := a.SuggestCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[database.CleanupDuplicate] != 1 || counts[database.CleanupStaleTask] != 1 || counts[database.CleanupLonelyPerson] != 1 || counts[database.CleanupNearDuplicate] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	list, _ := db.ListCleanup(ctx, database.CleanupPending, 20)
	find := func(kind string) database.CleanupSuggestion {
		for _, c := range list {
			if c.Kind == kind {
				return c
			}
		}
		t.Fatalf("no %s suggestion in %+v", kind, list)
		return database.CleanupSuggestion{}
	}
	dup := find(database.CleanupDuplicate)
	if dup.NodeIDs[0] != d1.ID {
		t.Fatalf("keeper = %v", dup.NodeIDs)
	}
	if near := find(database.CleanupNearDuplicate); len(near.NodeIDs) != 2 || near.NodeIDs[0] != n1.ID || near.NodeIDs[1] != n2.ID {
		t.Fatalf("near = %+v", near)
	}

	msg, err := a.ApplyCleanup(ctx, dup.ID, "")
	if err != nil || !strings.Contains(msg, "juntados") {
		t.Fatalf("merge = %q, %v", msg, err)
	}
	if _, err := db.GetNode(ctx, d2.ID); err != database.ErrNotFound {
		t.Fatal("copy not trashed")
	}
	if gone, _ := db.IsDeletedRef(ctx, "import:bookmarks", "2"); !gone {
		t.Error("merged copy must not come back on re-import")
	}
	if msg, _ := a.ApplyCleanup(ctx, dup.ID, ""); !strings.Contains(msg, "já resolvida") {
		t.Errorf("second apply = %q", msg)
	}
	if _, err := a.ApplyCleanup(ctx, find(database.CleanupStaleTask).ID, database.ActionDone); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.GetNode(ctx, stale.ID); n.Status != database.StatusDone {
		t.Fatalf("task = %+v", n)
	}
	if _, err := a.ApplyCleanup(ctx, find(database.CleanupNearDuplicate).ID, CleanupDismiss); err != nil {
		t.Fatal(err)
	}
	if n, err := a.ApplyAllCleanup(ctx, ""); err != nil || n != 1 { // the lonely person
		t.Fatalf("apply all = %d, %v", n, err)
	}
	if gone, _ := db.IsDeletedRef(ctx, database.AutoPersonSource, "beltrano"); !gone {
		t.Error("auto person must not be recreated")
	}

	// Next run: nothing left, and the dismissed pair is not suggested again.
	counts, err = a.SuggestCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range counts {
		if v != 0 {
			t.Errorf("%s still suggested: %d", k, v)
		}
	}
}
