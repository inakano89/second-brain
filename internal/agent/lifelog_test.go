package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

func TestOnThisDay(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	loc := a.cfg.Location()
	today := time.Date(2026, 9, 29, 8, 0, 0, 0, loc)
	mk := func(n *database.Node) {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	at := func(y int) time.Time { return time.Date(y, 9, 29, 15, 0, 0, 0, loc) }
	mk(&database.Node{Type: database.TypeNote, Title: "Nota de 2025", Content: "um ano atrás", CreatedAt: at(2025)})
	mk(&database.Node{Type: database.TypeNote, Title: "Nota de 2023", Content: "três anos", CreatedAt: at(2023)})
	mk(&database.Node{Type: database.TypeNote, Title: "Nota de 2024", Content: "dois anos: fora da lista 1,3,5", CreatedAt: at(2024)})
	mk(&database.Node{Type: database.TypeInsight, Title: "Briefing matinal 29/09/2025", Source: "routine", CreatedAt: at(2025)})
	mk(&database.Node{Type: database.TypeNote, Title: "Sem data", Source: "import:opml", CreatedAt: at(2021), Meta: map[string]any{database.MetaDateUnknown: true}})
	ev := at(2021)
	mk(&database.Node{Type: database.TypeEvent, Title: "Casamento", Source: "calendar", CreatedAt: time.Now(), DueAt: &ev})

	mem, err := a.OnThisDay(ctx, today)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, m := range mem {
		for _, n := range m.Nodes {
			titles = append(titles, n.Title)
		}
	}
	if strings.Join(titles, "|") != "Nota de 2025|Nota de 2023|Casamento" {
		t.Fatalf("memories = %v", titles)
	}
	out := FormatOnThisDay(mem)
	if !strings.Contains(out, "1 ano atrás (2025): *Nota de 2025*") || !strings.Contains(out, "3 anos atrás (2023)") {
		t.Fatalf("format = %q", out)
	}
	a.cfg.Update(map[string]string{"ON_THIS_DAY_YEARS": "1,5"})
	mem, _ = a.OnThisDay(ctx, today)
	if len(mem) != 2 || mem[0].YearsAgo != 1 || mem[1].Nodes[0].Title != "Casamento" { // 5 years ago: the event (the undated import is skipped)
		t.Fatalf("custom years = %+v", mem)
	}
	a.cfg.Update(map[string]string{"ON_THIS_DAY_YEARS": "off"})
	if mem, _ := a.OnThisDay(ctx, today); mem != nil {
		t.Fatalf("off = %+v", mem)
	}
}

func TestSpacedReview(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	book := "**Autor:** Fulano\n\n## Destaques\n\n> Primeiro destaque\n— p. 1\n\n> Segundo destaque\n— p. 2\n\n📝 Minha nota\n\n"
	n := &database.Node{Type: database.TypeArticle, Title: "Livro X", Content: book, Source: "import:kindle", SourceRef: "k", CreatedAt: time.Now().AddDate(-2, 0, 0)}
	if err := db.CreateNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	// A periodic report is never enrolled.
	db.CreateNode(ctx, &database.Node{Type: database.TypeInsight, Title: "Balanço", Content: "x", Source: "routine"})
	ins := &database.Node{Type: database.TypeInsight, Title: "Insight meu", Content: "Consistência vence intensidade.", Source: "web"}
	db.CreateNode(ctx, ins)

	day0 := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	items, err := a.ReviewDue(ctx, day0, 5)
	if err != nil || len(items) != 2 {
		t.Fatalf("day 0 = %+v, %v", items, err)
	}
	seen := map[string]int{}
	for _, it := range items {
		seen[it.Node.Title] = it.Step
	}
	if seen["Livro X"] != 1 || seen["Insight meu"] != 1 || seen["Balanço"] != 0 {
		t.Fatalf("seen = %v", seen)
	}
	text := FormatReviews(items)
	if !strings.Contains(text, "> Primeiro destaque") && !strings.Contains(text, "> Segundo destaque") {
		t.Fatalf("text = %q", text)
	}
	// Nothing is due right after; the first interval is one day.
	if items, _ := a.ReviewDue(ctx, day0.Add(time.Hour), 5); len(items) != 0 {
		t.Fatalf("too early: %+v", items)
	}
	items, _ = a.ReviewDue(ctx, day0.AddDate(0, 0, 1), 5)
	if len(items) != 2 {
		t.Fatalf("day 1 = %+v", items)
	}
	for _, it := range items {
		if it.Step != 2 {
			t.Errorf("%s step = %d", it.Node.Title, it.Step)
		}
	}
	// Second interval is 3 days: not due at day 3, due at day 4; books rotate highlights.
	if items, _ := a.ReviewDue(ctx, day0.AddDate(0, 0, 3), 5); len(items) != 0 {
		t.Fatalf("day 3 = %+v", items)
	}
	items, _ = a.ReviewDue(ctx, day0.AddDate(0, 0, 4), 5)
	texts := map[string]bool{}
	for _, it := range items {
		texts[it.Text] = true
	}
	if len(items) != 2 || !texts["📝 Minha nota"] {
		t.Fatalf("day 4 = %+v", items)
	}
	total, due, _ := db.ReviewStats(ctx, day0.AddDate(0, 0, 4))
	if total != 2 || due != 0 {
		t.Fatalf("stats = %d/%d", total, due)
	}
}

func TestDiaryFlow(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	now := time.Date(2026, 9, 29, 21, 30, 0, 0, a.cfg.Location())
	// The window is checked against the wall clock: move the session into the present.
	msg, err := a.StartDiary(ctx, time.Now())
	if err != nil || !strings.Contains(msg, "Diário de hoje") || !strings.Contains(msg, "1. Como foi o seu dia") {
		t.Fatalf("start = %q, %v", msg, err)
	}
	st := a.DiaryActive(ctx)
	if st == nil || len(st.Questions) != 3 {
		t.Fatalf("state = %+v", st)
	}
	reply, err := a.DiaryAnswer(ctx, "Foi produtivo.")
	if err != nil || !strings.Contains(reply, "2/3") {
		t.Fatalf("answer 1 = %q, %v", reply, err)
	}
	day := time.Now().In(a.cfg.Location()).Format("2006-01-02")
	n, err := db.GetNodeBySource(ctx, "diary", "diary:"+day)
	if err != nil || !strings.Contains(n.Content, "Foi produtivo.") {
		t.Fatalf("node after first answer = %+v, %v", n, err)
	}
	a.DiaryAnswer(ctx, "Aprendi Go.")
	reply, err = a.DiaryAnswer(ctx, "Da minha família.")
	if err != nil || !strings.Contains(reply, "salvo") {
		t.Fatalf("last = %q, %v", reply, err)
	}
	n, _ = db.GetNodeBySource(ctx, "diary", "diary:"+day)
	if strings.Count(n.Content, "## ") != 3 || !strings.Contains(n.Content, "Da minha família.") || !contains(n.Tags, "diario") {
		t.Fatalf("final note = %+v", n)
	}
	if a.DiaryActive(ctx) != nil {
		t.Fatal("session should be closed")
	}
	if _, err := a.DiaryAnswer(ctx, "solta"); err == nil {
		t.Fatal("answer without a session must fail")
	}
	// Skipping drops the session.
	a.StartDiary(ctx, time.Now())
	a.DiarySkip(ctx)
	if a.DiaryActive(ctx) != nil {
		t.Fatal("skip did not close the session")
	}
	_ = now
}

func TestYearReviewWithoutLLM(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	loc := a.cfg.Location()
	mk := func(n *database.Node) *database.Node {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	d := func(m time.Month) time.Time { return time.Date(2025, m, 10, 12, 0, 0, 0, loc) }
	n1 := mk(&database.Node{Type: database.TypeNote, Title: "Projeto Atlas", Content: strings.Repeat("planejamento ", 30), Tags: []string{"trabalho", "atlas"}, CreatedAt: d(time.March)})
	mk(&database.Node{Type: database.TypeNote, Title: "Viagem", Content: "praia", Tags: []string{"trabalho"}, CreatedAt: d(time.July)})
	mk(&database.Node{Type: database.TypeInsight, Title: "Usar SQLite", Content: "x", Source: MemorySource, Tags: []string{TagDecision}, CreatedAt: d(time.April)})
	mk(&database.Node{Type: database.TypeNote, Title: "Sem data", Source: "import:x", Meta: map[string]any{database.MetaDateUnknown: true}, CreatedAt: d(time.May)})
	mk(&database.Node{Type: database.TypeTask, Title: "Declarar imposto", Status: database.StatusOpen, CreatedAt: d(time.February)})
	p := mk(&database.Node{Type: database.TypePerson, Title: "Maria"})
	db.AddEdge(ctx, n1.ID, p.ID, "mentions", 1)

	n, text, err := a.YearReview(ctx, 2025)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Retrospectiva 2025", "#trabalho (2)", "Maria (1 menções)", "Usar SQLite", "Declarar imposto", "[[Projeto Atlas]]", "note: 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Sem data") {
		t.Error("undated imports must not count")
	}
	if n.Type != database.TypeInsight || n.SourceRef != "year:2025" {
		t.Fatalf("node = %+v", n)
	}
	if _, _, err := a.YearReview(ctx, 2010); err == nil {
		t.Fatal("empty year should fail")
	}
	if _, _, err := a.YearReview(ctx, time.Now().Year()+1); err == nil {
		t.Fatal("future year should fail")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
