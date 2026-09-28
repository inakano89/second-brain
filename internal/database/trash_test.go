package database

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTrashRestoreAndTombstones(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	mk := func(n *Node) *Node {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	a := mk(&Node{Type: TypeArticle, Title: "Favorito A", Content: "https://a.example", Source: "import:bookmarks", SourceRef: "a", Meta: map[string]any{"import_batch": "b1", "file": "x.png"}})
	b := mk(&Node{Type: TypeArticle, Title: "Favorito B", Content: "https://b.example", Source: "import:bookmarks", SourceRef: "b", Meta: map[string]any{"import_batch": "b1"}})
	p := mk(&Node{Type: TypePerson, Title: "Maria", Source: "agent", Meta: map[string]any{"auto": true}})
	keep := mk(&Node{Type: TypeNote, Title: "Fica", Content: "texto"})
	for _, e := range [][2]int64{{a.ID, b.ID}, {a.ID, p.ID}, {keep.ID, a.ID}} {
		if err := db.AddEdge(ctx, e[0], e[1], "related", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveEmbedding(ctx, a.ID, "m", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}

	if n, _ := db.CountNodes(ctx, NodeFilter{Batch: "b1"}); n != 2 {
		t.Fatalf("batch count = %d", n)
	}
	batch, n, err := db.TrashNodes(ctx, []int64{a.ID, b.ID, p.ID}, true)
	if err != nil || n != 3 {
		t.Fatalf("trash = %d, %v", n, err)
	}
	if _, err := db.GetNode(ctx, a.ID); err != ErrNotFound {
		t.Fatalf("node still there: %v", err)
	}
	if len(db.VectorSearch("m", []float32{1, 0}, 5, nil)) != 0 {
		t.Error("vector not removed")
	}
	for _, ref := range [][2]string{{"import:bookmarks", "a"}, {AutoPersonSource, "maria"}} {
		if gone, _ := db.IsDeletedRef(ctx, ref[0], ref[1]); !gone {
			t.Errorf("tombstone %v missing", ref)
		}
	}
	items, total, err := db.ListTrash(ctx, "favorito", "", 10, 0)
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("list trash = %d %v", total, err)
	}
	if bs, _ := db.TrashBatches(ctx, 10); len(bs) != 1 || bs[0].Count != 3 || bs[0].Batch != batch {
		t.Fatalf("batches = %+v", bs)
	}

	// B came back meanwhile (e.g. imported without tombstone): restoring must not duplicate it.
	if _, err := db.ClearDeletedRefs(ctx); err != nil {
		t.Fatal(err)
	}
	mk(&Node{Type: TypeArticle, Title: "Favorito B (novo)", Source: "import:bookmarks", SourceRef: "b"})
	restored, err := db.RestoreBatch(ctx, batch)
	if err != nil || restored != 2 {
		t.Fatalf("restored = %d, %v", restored, err)
	}
	got, err := db.GetNode(ctx, a.ID)
	if err != nil || got.UID != a.UID || got.Meta["file"] != "x.png" {
		t.Fatalf("restored node = %+v, %v", got, err)
	}
	links, _ := db.Neighbors(ctx, a.ID)
	if len(links) != 2 { // Maria and Fica; the old B is gone
		t.Fatalf("links after restore = %d", len(links))
	}
	if hits, _ := db.SearchFTS(ctx, "favorito", NodeFilter{}, 10); len(hits) != 2 {
		t.Errorf("fts after restore = %d", len(hits))
	}
	if c, _ := db.TrashCount(ctx); c != 0 {
		t.Errorf("trash count = %d", c)
	}

	// Purge: explicit ids and by age, returning media files.
	if _, _, err := db.TrashNodes(ctx, []int64{a.ID}, false); err != nil {
		t.Fatal(err)
	}
	if gone, _ := db.IsDeletedRef(ctx, "import:bookmarks", "a"); gone {
		t.Error("tombstone without forget")
	}
	files, n, err := db.PurgeTrash(ctx, nil, time.Now().Add(-TrashRetention))
	if err != nil || n != 0 || len(files) != 0 {
		t.Fatalf("young purge = %v %d %v", files, n, err)
	}
	files, n, err = db.PurgeTrash(ctx, nil, time.Now().Add(time.Second))
	if err != nil || n != 1 || len(files) != 1 || files[0] != "x.png" {
		t.Fatalf("purge = %v %d %v", files, n, err)
	}
}

func TestTrashManyNodes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	var ids []int64
	for i := range 1000 {
		n := &Node{Type: TypeArticle, Title: fmt.Sprintf("Link %d", i), Source: "import:bookmarks", SourceRef: fmt.Sprint(i)}
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}
	for i := 1; i < len(ids); i += 3 {
		_ = db.AddEdge(ctx, ids[i-1], ids[i], "related", 1)
	}
	all, err := db.NodeIDs(ctx, NodeFilter{Source: "import:bookmarks"}, 5000)
	if err != nil || len(all) != 1000 {
		t.Fatalf("ids = %d, %v", len(all), err)
	}
	batch, n, err := db.TrashNodes(ctx, all, true)
	if err != nil || n != 1000 {
		t.Fatalf("trash = %d, %v", n, err)
	}
	if c, _ := db.CountNodes(ctx, NodeFilter{}); c != 0 {
		t.Fatalf("left %d nodes", c)
	}
	if c, _ := db.DeletedRefCount(ctx); c != 1000 {
		t.Fatalf("tombstones = %d", c)
	}
	if r, err := db.RestoreBatch(ctx, batch); err != nil || r != 1000 {
		t.Fatalf("restore = %d, %v", r, err)
	}
	if c, _ := db.EdgeCount(ctx); c != 333 {
		t.Errorf("edges = %d", c)
	}
}

func TestSpecialFilters(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	for _, n := range []*Node{
		{Type: TypeArticle, Title: "Go Docs", Content: "linguagem go documentação"},
		{Type: TypeArticle, Title: "go docs", Content: "outra cópia"},
		{Type: TypeNote, Title: "Go Docs", Content: "nota com o mesmo título"},
		{Type: TypeNote, Title: "Vazia"},
	} {
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	count := func(f NodeFilter) int {
		t.Helper()
		n, err := db.CountNodes(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(NodeFilter{Special: "dup"}); n != 2 {
		t.Errorf("dup = %d", n)
	}
	if n := count(NodeFilter{Special: "empty"}); n != 1 {
		t.Errorf("empty = %d", n)
	}
	if n := count(NodeFilter{Special: "orphan"}); n != 4 {
		t.Errorf("orphan = %d", n)
	}
	if n := count(NodeFilter{Text: "go documentacao"}); n != 1 {
		t.Errorf("text AND = %d", n)
	}
	if n := count(NodeFilter{Text: "docs", Types: []string{TypeArticle}}); n != 2 {
		t.Errorf("text + type = %d", n)
	}
	nodes, err := db.ListNodes(ctx, NodeFilter{Special: "dup", Order: "title"})
	if err != nil || len(nodes) != 2 || nodes[0].Type != TypeArticle {
		t.Fatalf("dup list = %+v, %v", nodes, err)
	}
}
