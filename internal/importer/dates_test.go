package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

func TestDedupeKeepsNewest(t *testing.T) {
	d := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	items := dedupe([]Item{
		{Format: FormatJSON, Ref: "a", Title: "novo", CreatedAt: d(2025)},
		{Format: FormatJSON, Ref: "a", Title: "velho", CreatedAt: d(2015)}, // later in the file, but older
		{Format: FormatJSON, Ref: "b", Title: "b1", CreatedAt: d(2020)},
		{Format: FormatJSON, Ref: "b", Title: "b2"}, // undated never beats a dated one
		{Format: FormatJSON, Ref: "c", Title: "c1"},
		{Format: FormatJSON, Ref: "c", Title: "c2"}, // both undated: the last one, as before
	})
	got := map[string]string{}
	for _, it := range items {
		got[it.Ref] = it.Title
	}
	if len(items) != 3 || got["a"] != "novo" || got["b"] != "b1" || got["c"] != "c2" {
		t.Fatalf("dedupe = %+v", got)
	}
}

func TestDedupePersonsNewerWins(t *testing.T) {
	old := Item{Format: FormatVCard, Ref: "1", Type: database.TypePerson, Title: "Ana Lima", CreatedAt: time.Date(2018, 5, 1, 0, 0, 0, 0, time.UTC),
		Content: "**Telefone:** +55 11 90000-0001\n**Organização:** Velha SA", Meta: map[string]any{"phones": []string{"+55 11 90000-0001"}, "org": "Velha SA"}}
	fresh := Item{Format: FormatVCard, Ref: "2", Type: database.TypePerson, Title: "ana lima", CreatedAt: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC),
		Content: "**Telefone:** +55 11 98888-0002\n**Organização:** Nova SA", Meta: map[string]any{"phones": []string{"+55 11 98888-0002"}, "org": "Nova SA"}}
	for name, in := range map[string][]Item{"older first": {old, fresh}, "newer first": {fresh, old}} {
		out := dedupe(in)
		if len(out) != 1 {
			t.Fatalf("%s: %d items", name, len(out))
		}
		p := out[0]
		if !strings.Contains(p.Content, "**Organização:** Nova SA _(antes: Velha SA)_") ||
			!strings.Contains(p.Content, "**Telefone:** +55 11 98888-0002, +55 11 90000-0001") {
			t.Errorf("%s: content = %q", name, p.Content)
		}
		phones := metaStrings(p.Meta["phones"])
		if p.Meta["org"] != "Nova SA" || len(phones) != 2 || phones[0] != "+55 11 98888-0002" {
			t.Errorf("%s: meta = %+v", name, p.Meta)
		}
		if p.CreatedAt.Year() != 2025 {
			t.Errorf("%s: date = %v", name, p.CreatedAt)
		}
	}
}

func TestMergeContactPhoneTypesStayWhole(t *testing.T) {
	got := mergeContact("**Telefone:** +55 11 1 (home, voice)", "**Telefone:** +55 11 2 (cell), +55 11 1 (home, voice)", true)
	if got != "**Telefone:** +55 11 2 (cell), +55 11 1 (home, voice)" {
		t.Fatalf("merge = %q", got)
	}
}

func TestMergePersonExisting(t *testing.T) {
	im, db, _, _ := setupImporter(t)
	ctx := context.Background()
	p := &database.Node{Type: database.TypePerson, Title: "Rui", Source: "import:vcard", SourceRef: "old", CreatedAt: time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC),
		Content: "**Telefone:** 111\n**Organização:** Velha", Meta: map[string]any{"phones": []any{"111"}, "org": "Velha"}}
	if err := db.CreateNode(ctx, p); err != nil {
		t.Fatal(err)
	}
	// An undated record never overrides a dated one, but adds what is missing.
	blind := &Item{Format: FormatVCard, Type: database.TypePerson, Title: "Rui", Content: "**Telefone:** 222\n**Cargo:** Chefe",
		Meta: map[string]any{"phones": []string{"222"}, "org": "Outra", "title": "Chefe"}}
	if _, out, err := im.mergePerson(ctx, p, blind); err != nil || out != outUpdated {
		t.Fatalf("merge blind: %d %v", out, err)
	}
	got, _ := db.GetNode(ctx, p.ID)
	if got.Meta["org"] != "Velha" || got.Meta["title"] != "Chefe" || !strings.Contains(got.Content, "**Telefone:** 111, 222") {
		t.Fatalf("after blind: %q %+v", got.Content, got.Meta)
	}
	// A newer record wins the conflicts and becomes the person's date.
	newer := &Item{Format: FormatVCard, Type: database.TypePerson, Title: "Rui", CreatedAt: time.Date(2025, 2, 2, 0, 0, 0, 0, time.UTC),
		Content: "**Telefone:** 333\n**Organização:** Nova", Meta: map[string]any{"phones": []string{"333"}, "org": "Nova"}}
	if _, out, err := im.mergePerson(ctx, got, newer); err != nil || out != outUpdated {
		t.Fatalf("merge newer: %d %v", out, err)
	}
	got, _ = db.GetNode(ctx, p.ID)
	phones := metaStrings(got.Meta["phones"])
	if got.Meta["org"] != "Nova" || phones[0] != "333" || !strings.Contains(got.Content, "**Organização:** Nova _(antes: Velha)_") || got.CreatedAt.Year() != 2025 {
		t.Fatalf("after newer: %q %+v %v", got.Content, got.Meta, got.CreatedAt)
	}
	// Same data again: nothing to do.
	if _, out, err := im.mergePerson(ctx, got, newer); err != nil || out != outSkipped {
		t.Fatalf("idempotent merge: %d %v", out, err)
	}
}

func TestUndatedItemsAreFlagged(t *testing.T) {
	im, db, _, dir := setupImporter(t)
	ctx := context.Background()
	csv := filepath.Join(dir, "n.csv")
	os.WriteFile(csv, []byte("id,title,content,date\n1,Com data,um,2016-04-05\n2,Sem data,dois,\n3,Epoch,tres,1970-01-01\n"), 0o600)
	rep := im.Run(ctx, []File{{Path: csv, Name: "n.csv"}}, Options{})
	if rep.Created != 3 {
		t.Fatalf("report: %+v", rep)
	}
	dated, _ := db.FindByTitle(ctx, "", "Com data")
	blind, _ := db.FindByTitle(ctx, "", "Sem data")
	epoch, _ := db.FindByTitle(ctx, "", "Epoch")
	if dated.DateUnknown() || dated.CreatedAt.Year() != 2016 {
		t.Fatalf("dated = %+v", dated)
	}
	if !blind.DateUnknown() || !epoch.DateUnknown() {
		t.Fatalf("undated not flagged: %v %v", blind.Meta, epoch.Meta)
	}
	if _, ok := blind.ImportedAt(); !ok {
		t.Fatal("import date missing")
	}
	// Re-importing the file with the date filled in adopts it.
	os.WriteFile(csv, []byte("id,title,content,date\n1,Com data,um,2016-04-05\n2,Sem data,dois,2020-02-03\n3,Epoch,tres,1970-01-01\n"), 0o600)
	im.Run(ctx, []File{{Path: csv, Name: "n.csv"}}, Options{})
	blind, _ = db.FindByTitle(ctx, "", "Sem data")
	if blind.DateUnknown() || blind.CreatedAt.Year() != 2020 {
		t.Fatalf("date not adopted: %v unknown=%v", blind.CreatedAt, blind.DateUnknown())
	}
}
