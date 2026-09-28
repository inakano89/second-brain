package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNodesFTSGraphQueue(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	a := &Node{Type: TypeNote, Title: "Reunião com João", Content: "Discutimos o orçamento do projeto Atlas", Tags: []string{"#Projeto", "atlas"}}
	b := &Node{Type: TypePerson, Title: "João"}
	if err := db.CreateNode(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateNode(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := db.AddEdge(ctx, a.ID, b.ID, "mentions", 1); err != nil {
		t.Fatal(err)
	}
	hits, err := db.SearchFTS(ctx, "orcamento atlas", NodeFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != a.ID {
		t.Fatalf("fts hits = %+v", hits)
	}
	g, err := db.Subgraph(ctx, GraphFilter{IDs: []int64{a.ID}, Hops: 1})
	if err != nil || len(g.Nodes) != 2 || len(g.Edges) != 1 {
		t.Fatalf("graph = %+v, %v", g, err)
	}
	if err := db.SaveEmbedding(ctx, a.ID, "m", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbedding(ctx, b.ID, "m", []float32{0.9, 0.1, 0}); err != nil {
		t.Fatal(err)
	}
	vh := db.VectorSearch("m", []float32{1, 0, 0}, 1, nil)
	if len(vh) != 1 || vh[0].ID != a.ID {
		t.Fatalf("vector = %+v", vh)
	}

	if _, err := db.Enqueue(ctx, "x", map[string]int{"n": 1}, EnqueueOpts{DedupeKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Enqueue(ctx, "x", nil, EnqueueOpts{DedupeKey: "k"}); err != nil {
		t.Fatal(err)
	}
	st, _ := db.QueueStats(ctx)
	if st[TaskPending] != 1 {
		t.Fatalf("dedupe failed: %v", st)
	}
	task, err := db.ClaimTask(ctx)
	if err != nil || task == nil || task.Attempts != 1 {
		t.Fatalf("claim = %+v, %v", task, err)
	}
	if err := db.FailTask(ctx, task, errors.New("boom"), true); err != nil {
		t.Fatal(err)
	}
	if next, _ := db.ClaimTask(ctx); next != nil {
		t.Fatalf("task should be delayed by backoff")
	}
	if err := db.DeleteNode(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.EdgeCount(ctx); c != 0 {
		t.Fatalf("cascade failed: %d edges", c)
	}
	if err := db.Maintenance(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := db.Snapshot(ctx, filepath.Join(t.TempDir(), "snap.db")); err != nil {
		t.Fatal(err)
	}
}
