package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDuplicateGroupsAndMerge(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	mk := func(n *Node) *Node {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	old := time.Now().Add(-48 * time.Hour)
	a := mk(&Node{Type: TypeArticle, Title: "Go Docs", Content: "https://go.dev", Tags: []string{"go"}, CreatedAt: old, Meta: map[string]any{"url": "https://go.dev"}})
	b := mk(&Node{Type: TypeArticle, Title: "go docs", Content: "https://go.dev", Tags: []string{"docs"}})
	c := mk(&Node{Type: TypeArticle, Title: "Documentação do Go", Content: "Texto novo sobre a linguagem", Meta: map[string]any{"url": "https://go.dev", "lang": "pt"}})
	mk(&Node{Type: TypeNote, Title: "Go Docs", Content: "https://go.dev"}) // other type: not a duplicate
	p := mk(&Node{Type: TypePerson, Title: "Ana"})
	q := mk(&Node{Type: TypeNote, Title: "Projeto"})
	for _, e := range [][2]int64{{b.ID, p.ID}, {q.ID, c.ID}, {a.ID, b.ID}} {
		if err := db.AddEdge(ctx, e[0], e[1], "related", 1); err != nil {
			t.Fatal(err)
		}
	}

	groups, err := db.DuplicateGroups(ctx, 100)
	if err != nil || len(groups) != 1 || len(groups[0]) != 3 || groups[0][0] != a.ID {
		t.Fatalf("groups = %v, %v", groups, err)
	}
	k, err := db.MergeNodes(ctx, a.ID, []int64{b.ID, c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.Content, "Texto novo sobre a linguagem") || strings.Count(k.Content, "https://go.dev") != 1 ||
		k.Meta["lang"] != "pt" || len(k.Tags) != 2 || len(k.Meta["merged_from"].([]any)) != 2 {
		t.Fatalf("merged = %+v", k)
	}
	if _, _, err := db.TrashNodes(ctx, []int64{b.ID, c.ID}, true); err != nil {
		t.Fatal(err)
	}
	links, _ := db.Neighbors(ctx, a.ID)
	if len(links) != 2 { // Ana (from b) and Projeto (from c); a→b became a self-loop and was dropped
		t.Fatalf("links = %+v", links)
	}
	if groups, _ := db.DuplicateGroups(ctx, 100); len(groups) != 0 {
		t.Fatalf("groups after merge = %v", groups)
	}
}

func TestCleanupSuggestionsLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	var ids []int64
	for _, title := range []string{"A", "B", "C"} {
		n := &Node{Type: TypeNote, Title: title}
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}
	list := []CleanupSuggestion{
		{Kind: CleanupDuplicate, Action: ActionMerge, NodeIDs: []int64{ids[0], ids[1]}, Reason: "dup"},
		{Kind: CleanupEmpty, Action: ActionTrash, NodeIDs: []int64{ids[2]}},
		{Kind: CleanupEmpty, Action: ActionMerge, NodeIDs: []int64{ids[2]}}, // a merge needs two nodes
	}
	added, err := db.ReplaceCleanupSuggestions(ctx, list)
	if err != nil || added[CleanupDuplicate] != 1 || added[CleanupEmpty] != 1 {
		t.Fatalf("added = %v, %v", added, err)
	}
	got, err := db.ListCleanup(ctx, CleanupPending, 10)
	if err != nil || len(got) != 2 || len(got[0].Nodes) != 2 || got[0].Nodes[0].ID != ids[0] {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if err := db.ResolveCleanup(ctx, got[1].ID, CleanupDismissed); err != nil {
		t.Fatal(err)
	}
	// Next week: the dismissed proposal is not suggested again; pending ones are replaced.
	if added, _ := db.ReplaceCleanupSuggestions(ctx, list); added[CleanupEmpty] != 0 || added[CleanupDuplicate] != 1 {
		t.Fatalf("second run added = %v", added)
	}
	if n, _ := db.CleanupPendingCount(ctx); n != 1 {
		t.Fatalf("pending = %d", n)
	}
	// A suggestion whose node vanished is dropped.
	if _, _, err := db.TrashNodes(ctx, []int64{ids[1]}, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CleanupPendingCount(ctx); n != 0 {
		t.Fatalf("pending after delete = %d", n)
	}
	if got, _ := db.ListCleanup(ctx, CleanupPending, 10); len(got) != 0 {
		t.Fatalf("stale suggestion listed: %+v", got)
	}
}

func TestDetectorsAndHelpers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	long := time.Now().AddDate(0, 0, -40)
	mk := func(n *Node) *Node {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	empty := mk(&Node{Type: TypeNote, Title: "Sem título", CreatedAt: long})
	mk(&Node{Type: TypeNote, Title: "Vazia nova"})
	stale := mk(&Node{Type: TypeTask, Title: "Velha", CreatedAt: long})
	due := long.AddDate(0, 0, 5)
	late := mk(&Node{Type: TypeTask, Title: "Atrasada", DueAt: &due})
	mk(&Node{Type: TypeTask, Title: "Nova"})
	lonely := mk(&Node{Type: TypePerson, Title: "Fulano", Source: "agent", CreatedAt: long})
	busy := mk(&Node{Type: TypePerson, Title: "Ciclano", Source: "agent", CreatedAt: long})
	for _, other := range []*Node{mk(&Node{Type: TypeNote, Title: "x", Content: "1"}), mk(&Node{Type: TypeNote, Title: "y", Content: "2"})} {
		db.AddEdge(ctx, other.ID, busy.ID, "mentions", 1)
	}
	db.ExecContext(ctx, `UPDATE nodes SET updated_at = ? WHERE id IN (?, ?)`, fmtTime(long), stale.ID, late.ID)

	before := time.Now().AddDate(0, 0, -7)
	if ns, _ := db.EmptyNodes(ctx, before, 10); len(ns) != 1 || ns[0].ID != empty.ID {
		t.Errorf("empty = %+v", ns)
	}
	month := time.Now().AddDate(0, 0, -30)
	if ns, _ := db.StaleTasks(ctx, month, month, 10); len(ns) != 2 {
		t.Errorf("stale = %+v", ns)
	}
	if ns, _ := db.LonelyAutoPersons(ctx, time.Now().AddDate(0, 0, -14), 10); len(ns) != 1 || ns[0].ID != lonely.ID {
		t.Errorf("lonely = %+v", ns)
	}

	// SetMetaKey is bookkeeping: updated_at does not move.
	n, _ := db.GetNode(ctx, stale.ID)
	if err := db.SetMetaKey(ctx, stale.ID, "actions_at", "2026-09-28T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	n2, _ := db.GetNode(ctx, stale.ID)
	if !n2.UpdatedAt.Equal(n.UpdatedAt) || n2.Meta["actions_at"] != "2026-09-28T12:00:00Z" {
		t.Fatalf("meta = %+v", n2)
	}

	// Derived origins and per-day counts.
	db.AddEdge(ctx, late.ID, empty.ID, "derived_from", 1)
	if o, _ := db.DerivedOrigins(ctx, []int64{late.ID, stale.ID}); len(o) != 1 || o[late.ID].ID != empty.ID {
		t.Fatalf("origins = %+v", o)
	}
	if c, _ := db.DerivedTaskCount(ctx, empty.ID); c != 1 {
		t.Fatalf("derived count = %d", c)
	}
	per, _ := db.CreatedPerDay(ctx, time.Now().AddDate(0, 0, -1), loc)
	total := 0
	for _, v := range per {
		total += v
	}
	if total != 5 {
		t.Errorf("per day = %v", per)
	}

	// Near duplicates: pairs above the threshold, each once.
	for id, v := range map[int64][]float32{empty.ID: {1, 0, 0}, stale.ID: {0.99, 0.14, 0}, late.ID: {0, 1, 0}} {
		if err := db.SaveEmbedding(ctx, id, "m", v); err != nil {
			t.Fatal(err)
		}
	}
	pairs := db.NearDuplicates("m", []int64{empty.ID, stale.ID}, 0.95)
	if len(pairs) != 1 || pairs[0].A != min(empty.ID, stale.ID) || pairs[0].Score < 0.95 {
		t.Fatalf("pairs = %+v", pairs)
	}
}
