package agent

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

func TestRecencyFactor(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	node := func(typ string, age time.Duration) *database.Node {
		return &database.Node{Type: typ, CreatedAt: now.Add(-age), Meta: map[string]any{}}
	}
	year := 365 * 24 * time.Hour
	if f := recencyFactor(node(database.TypeNote, 0), now, 365); f != 1 {
		t.Errorf("fresh = %v", f)
	}
	if f := recencyFactor(node(database.TypeNote, year), now, 365); math.Abs(f-0.75) > 0.001 {
		t.Errorf("one half-life = %v, want 0.75", f)
	}
	if f := recencyFactor(node(database.TypeNote, 30*year), now, 365); f < recencyFloor || f > recencyFloor+0.001 {
		t.Errorf("ancient = %v, want floor %v", f, recencyFloor)
	}
	if f := recencyFactor(node(database.TypeNote, year), now, 0); f != 1 {
		t.Errorf("disabled = %v", f)
	}
	task := node(database.TypeTask, 5*year)
	task.Status = database.StatusOpen
	if f := recencyFactor(task, now, 365); f != 1 {
		t.Errorf("open task aged: %v", f)
	}
	task.Status = database.StatusDone
	if f := recencyFactor(task, now, 365); f == 1 {
		t.Errorf("done task not aged")
	}
	if f := recencyFactor(node(database.TypePerson, 9*year), now, 365); f != 1 {
		t.Errorf("person aged: %v", f)
	}
	unknown := node(database.TypeNote, 0)
	unknown.Meta[database.MetaDateUnknown] = true
	unknown.CreatedAt = now.Add(-9 * year)
	if f := recencyFactor(unknown, now, 365); f != 1 {
		t.Errorf("undated aged: %v", f)
	}
	future := now.Add(30 * 24 * time.Hour)
	ev := node(database.TypeEvent, 3*year)
	ev.DueAt = &future
	if f := recencyFactor(ev, now, 365); f != 1 {
		t.Errorf("upcoming event aged: %v", f)
	}
	past := now.Add(-2 * year)
	ev.DueAt = &past
	if f := recencyFactor(ev, now, 365); f >= 0.7 { // aged by the day it happened, not by when it was synced
		t.Errorf("past event = %v", f)
	}
}

func TestWantsHistory(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	for q, want := range map[string]bool{
		"o que eu pensava em 2018?": true, "notas antigas sobre Go": true, "faz anos atrás": true, "há 5 anos comprei": true,
		"o que fiz em 2026": false, "reunião de hoje": false, "orçamento do projeto": false,
	} {
		if got := wantsHistory(q, now); got != want {
			t.Errorf("wantsHistory(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestSearchPrefersNewerContent(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	now := time.Now()
	mk := func(title string, age time.Duration, meta map[string]any) *database.Node {
		n := &database.Node{Type: database.TypeNote, Title: title, Content: "telefone de contato da empresa Acme", CreatedAt: now.Add(-age), Meta: meta}
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	year := 365 * 24 * time.Hour
	older := mk("Acme contato antigo", 6*year, nil)
	newer := mk("Acme contato novo", 10*24*time.Hour, nil)
	undated := mk("Acme contato importado", 0, map[string]any{database.MetaDateUnknown: true})

	res, err := a.Search(ctx, "telefone contato Acme", database.NodeFilter{}, 5)
	if err != nil || len(res) != 3 {
		t.Fatalf("res = %v, %v", res, err)
	}
	pos := map[int64]int{}
	for i, r := range res {
		pos[r.Node.ID] = i
	}
	if pos[newer.ID] > pos[older.ID] {
		t.Fatalf("old note outranked the new one: %v", pos)
	}
	// A question about the past disables the weighting: scores are the plain fusion ones again.
	hist, _ := a.Search(ctx, "telefone contato Acme em 2019", database.NodeFilter{}, 5)
	for _, r := range hist {
		if r.Node.ID == older.ID {
			if r.Score < 0.9*res[pos[newer.ID]].Score { // no age penalty: comparable to the new one
				t.Fatalf("history query still penalised: %v vs %v", r.Score, res[pos[newer.ID]].Score)
			}
		}
	}
	_ = undated
}

func TestContextDatesAndBrief(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	n := &database.Node{Type: database.TypeNote, Source: "import:evernote", CreatedAt: time.Date(2021, 3, 4, 0, 0, 0, 0, time.UTC),
		Meta: map[string]any{database.MetaImportAt: "2026-09-01T10:00:00.000000Z"}}
	got := contextDates(n, loc, now)
	if !strings.Contains(got, "criado=2021-03-04") || !strings.Contains(got, "idade=5 anos") || !strings.Contains(got, "importado=2026-09-01") {
		t.Errorf("contextDates = %q", got)
	}
	n.Meta = map[string]any{database.MetaDateUnknown: true}
	if got := contextDates(n, loc, now); got != "data=desconhecida" {
		t.Errorf("unknown = %q", got)
	}
	if b := brief(n, loc); b.Created != "data desconhecida" {
		t.Errorf("brief = %+v", b)
	}
}

func TestCleanupKeepsNewestCopy(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	mk := func(at time.Time) *database.Node {
		n := &database.Node{Type: database.TypeNote, Title: "Receita", Content: "bolo de cenoura", CreatedAt: at}
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	old := mk(time.Now().AddDate(-5, 0, 0))
	fresh := mk(time.Now().AddDate(0, -1, 0))
	if _, err := a.SuggestCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := db.ListCleanup(ctx, database.CleanupPending, 10)
	if len(list) != 1 || list[0].NodeIDs[0] != fresh.ID || list[0].NodeIDs[1] != old.ID {
		t.Fatalf("suggestions = %+v (keeper must be the newest)", list)
	}
	if _, err := a.ApplyCleanup(ctx, list[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetNode(ctx, fresh.ID); err != nil {
		t.Fatalf("newest copy was removed: %v", err)
	}
	if _, err := db.GetNode(ctx, old.ID); err == nil {
		t.Fatal("old copy should be in the trash")
	}
}
