package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDateUnknownOrderAndFilters(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	mk := func(n *Node) *Node {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }
	old := mk(&Node{Type: TypeNote, Title: "old", Source: "import:markdown", CreatedAt: day(2015, 3, 1)})
	undated := mk(&Node{Type: TypeNote, Title: "undated", Source: "import:opml", Meta: map[string]any{MetaDateUnknown: true}})
	mine := mk(&Node{Type: TypeNote, Title: "mine", Source: "telegram", CreatedAt: day(2026, 1, 1)})
	gmail := mk(&Node{Type: TypeNote, Title: "mail", Source: "gmail", CreatedAt: day(2025, 5, 5)})
	evt := due(mk(&Node{Type: TypeEvent, Title: "trip", Source: "calendar", CreatedAt: day(2024, 1, 1)}), day(2030, 1, 1), t, db)

	titles := func(f NodeFilter) string {
		t.Helper()
		nodes, err := db.ListNodes(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range nodes {
			out = append(out, n.Title)
		}
		return strings.Join(out, ",")
	}
	if got := titles(NodeFilter{}); got != "mine,mail,trip,old,undated" {
		t.Fatalf("default order = %s (undated must come last)", got)
	}
	if got := titles(NodeFilter{Order: "oldest"}); got != "old,trip,mail,mine,undated" {
		t.Fatalf("oldest = %s", got)
	}
	if got := titles(NodeFilter{Order: "date"}); got != "trip,mine,mail,old,undated" {
		t.Fatalf("date order = %s (events by their own date)", got)
	}
	if got := titles(NodeFilter{KnownDate: true}); strings.Contains(got, "undated") {
		t.Fatalf("known date = %s", got)
	}
	if got := titles(NodeFilter{Origin: OriginImported}); got != "old,undated" {
		t.Fatalf("imported = %s", got)
	}
	if got := titles(NodeFilter{Origin: OriginMine}); got != "mine" {
		t.Fatalf("mine = %s", got)
	}
	if got := titles(NodeFilter{Origin: OriginAuto}); got != "mail,trip" {
		t.Fatalf("auto = %s", got)
	}
	cut := day(2020, 1, 1)
	if got := titles(NodeFilter{HideOldImports: &cut}); got != "mine,mail,trip,undated" {
		t.Fatalf("archive = %s (old import hidden, undated kept)", got)
	}
	// Matches mirrors the SQL filters (used on vector hits).
	for _, c := range []struct {
		f    NodeFilter
		n    *Node
		want bool
	}{
		{NodeFilter{Origin: OriginImported}, old, true}, {NodeFilter{Origin: OriginImported}, mine, false},
		{NodeFilter{Origin: OriginMine}, mine, true}, {NodeFilter{Origin: OriginAuto}, gmail, true}, {NodeFilter{Origin: OriginAuto}, old, false},
		{NodeFilter{KnownDate: true}, undated, false}, {NodeFilter{HideOldImports: &cut}, old, false}, {NodeFilter{HideOldImports: &cut}, undated, true},
	} {
		if got := c.f.Matches(c.n); got != c.want {
			t.Errorf("Matches(%+v, %s) = %v, want %v", c.f, c.n.Title, got, c.want)
		}
	}
	_ = evt
}

func due(n *Node, at time.Time, t *testing.T, db *DB) *Node {
	t.Helper()
	n.DueAt = &at
	if err := db.UpdateNode(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUpsertKeepsOriginalDate(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	// First import without a date, then a re-import that brings the real one.
	n := &Node{Type: TypeNote, Title: "a", Content: "x", Source: "import:json", SourceRef: "1", Meta: map[string]any{MetaDateUnknown: true}}
	if created, err := db.UpsertBySource(ctx, n); err != nil || !created {
		t.Fatal(created, err)
	}
	real := time.Date(2019, 6, 1, 9, 0, 0, 0, time.UTC)
	again := &Node{Type: TypeNote, Title: "a", Content: "x2", Source: "import:json", SourceRef: "1", CreatedAt: real}
	if created, err := db.UpsertBySource(ctx, again); err != nil || created {
		t.Fatal(created, err)
	}
	got, _ := db.GetNode(ctx, n.ID)
	if got.DateUnknown() || !got.CreatedAt.Equal(real) {
		t.Fatalf("date not adopted: %v unknown=%v", got.CreatedAt, got.DateUnknown())
	}
	// A later re-import without a date must not flag a dated node as undated.
	blind := &Node{Type: TypeNote, Title: "a", Content: "x3", Source: "import:json", SourceRef: "1", Meta: map[string]any{MetaDateUnknown: true}}
	if _, err := db.UpsertBySource(ctx, blind); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetNode(ctx, n.ID)
	if got.DateUnknown() || !got.CreatedAt.Equal(real) {
		t.Fatalf("known date lost: %v unknown=%v", got.CreatedAt, got.DateUnknown())
	}
}

func TestMergeKeepsNewestWithDatedVersions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	mk := func(title, content string, at time.Time, meta map[string]any) *Node {
		n := &Node{Type: TypeNote, Title: title, Content: content, CreatedAt: at, Meta: meta}
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	old := mk("Plano", "versão de 2015", time.Date(2015, 3, 12, 12, 0, 0, 0, time.UTC), nil)
	unknown := mk("Plano", "versão sem data", time.Now(), map[string]any{MetaDateUnknown: true})
	fresh := mk("Plano", "versão de 2025", time.Date(2025, 8, 1, 12, 0, 0, 0, time.UTC), nil)

	order, err := db.OrderNewestFirst(ctx, []int64{old.ID, unknown.ID, fresh.ID})
	if err != nil || order[0] != fresh.ID || order[1] != old.ID || order[2] != unknown.ID {
		t.Fatalf("order = %v, %v", order, err)
	}
	k, err := db.MergeNodes(ctx, fresh.ID, []int64{old.ID, unknown.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Content, "versão de 2025") || strings.Count(k.Content, VersionsHeading) != 1 ||
		!strings.Contains(k.Content, "### 12/03/2015 · “Plano” (#") || !strings.Contains(k.Content, "data desconhecida") {
		t.Fatalf("content = %q", k.Content)
	}
	if k.DateUnknown() {
		t.Fatal("keeper inherited the unknown-date flag")
	}
}

func TestMigrationFlagsLegacyUndatedImports(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	at := time.Now().UTC().Add(-time.Hour)
	stamp := map[string]any{MetaImportAt: at.Format(TimeLayout)}
	legacy := &Node{Type: TypeNote, Title: "legacy", Source: "import:opml", Meta: stamp, CreatedAt: at.Add(3 * time.Second)}
	dated := &Node{Type: TypeNote, Title: "dated", Source: "import:markdown", Meta: stamp, CreatedAt: time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, n := range []*Node{legacy, dated} {
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range splitSQL(migrations[5]) { // migration 6, applied to rows created before it existed
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	l, _ := db.GetNode(ctx, legacy.ID)
	d, _ := db.GetNode(ctx, dated.ID)
	if !l.DateUnknown() || d.DateUnknown() {
		t.Fatalf("legacy unknown=%v dated unknown=%v", l.DateUnknown(), d.DateUnknown())
	}
	if n, err := db.CountImported(ctx, at.Add(-time.Minute), time.Now()); err != nil || n != 2 {
		t.Fatalf("CountImported = %d, %v", n, err)
	}
}
