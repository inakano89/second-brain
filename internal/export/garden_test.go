package export

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

func gardenDB(t *testing.T) (*database.DB, context.Context) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, context.Background()
}

func TestGardenPublishesOnlyTaggedNotes(t *testing.T) {
	db, ctx := gardenDB(t)
	mk := func(n *database.Node) *database.Node {
		t.Helper()
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	day := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	a := mk(&database.Node{Type: database.TypeNote, Title: "Ideias sobre Go", Content: "Veja [[Segunda nota]] e [[Segredo]] e [[Maria]].\n\n- item **forte**\n\n<script>alert(1)</script>", Tags: []string{"publico", "go", "Ideias"}, CreatedAt: day, Summary: "Um resumo"})
	b := mk(&database.Node{Type: database.TypeNote, Title: "Segunda nota", Content: "Texto b", Tags: []string{"publico", "ideias"}, CreatedAt: day.AddDate(0, 0, 1)})
	mk(&database.Node{Type: database.TypeNote, Title: "Segredo", Content: "senha do banco: 1234", Tags: []string{"privado"}, CreatedAt: day})
	mk(&database.Node{Type: database.TypePerson, Title: "Maria", Content: "telefone 999", Tags: []string{"publico"}}) // people are never published
	mk(&database.Node{Type: database.TypeTask, Title: "Tarefa", Tags: []string{"publico"}})
	c := mk(&database.Node{Type: database.TypeInsight, Title: "Ideias sobre Go", Content: "duplicado de título", Tags: []string{"publico"}, CreatedAt: day.AddDate(0, 0, 2)})
	if err := db.AddEdge(ctx, b.ID, c.ID, "related", 1); err != nil {
		t.Fatal(err)
	}
	priv := mk(&database.Node{Type: database.TypeNote, Title: "Nota privada ligada"})
	db.AddEdge(ctx, a.ID, priv.ID, "related", 1)

	files, rep, err := GardenFiles(ctx, db, GardenOpts{Title: "Meu Jardim", Tag: "publico", BaseURL: "https://jardim.exemplo.com", Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pages) != 3 || rep.Tags != 2 {
		t.Fatalf("report = %+v", rep)
	}
	for _, want := range []string{"index.html", "style.css", ".nojekyll", "feed.xml", "notas/ideias-sobre-go.html", "notas/ideias-sobre-go-2.html", "notas/segunda-nota.html", "tags/go.html", "tags/ideias.html"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing file %s (have %v)", want, keys(files))
		}
	}
	all := ""
	for _, f := range files {
		all += string(f)
	}
	for _, leak := range []string{"senha do banco", "Segredo</a>", "telefone 999", "Nota privada ligada", "Tarefa", "<script>"} {
		if strings.Contains(all, leak) {
			t.Errorf("leaked %q", leak)
		}
	}
	page := string(files["notas/ideias-sobre-go-2.html"]) // the newer duplicate title takes the plain slug
	for _, want := range []string{`<a href="segunda-nota.html">Segunda nota</a>`, "Segredo", "Maria", "<strong>forte</strong>", "&lt;script&gt;", `href="../tags/go.html">#go</a>`, "04/03/2026", `href="../style.css"`, "Notas ligadas"} {
		if !strings.Contains(page, want) {
			t.Errorf("note page missing %q", want)
		}
	}
	if strings.Contains(page, `href="segredo.html"`) || strings.Contains(page, `href="maria.html"`) {
		t.Error("links to unpublished notes must be plain text")
	}
	if !strings.Contains(string(files["notas/segunda-nota.html"]), `href="ideias-sobre-go-2.html"`) { // backlink from the first note
		t.Error("backlink missing")
	}
	idx := string(files["index.html"])
	if !strings.Contains(idx, "3 notas") || !strings.Contains(idx, `href="notas/segunda-nota.html"`) || !strings.Contains(idx, `href="tags/go.html">#go`) || !strings.Contains(idx, "Um resumo") {
		t.Errorf("index = %s", idx)
	}
	if feed := string(files["feed.xml"]); !strings.Contains(feed, "https://jardim.exemplo.com/notas/segunda-nota.html") {
		t.Errorf("feed = %s", feed)
	}
	if _, has := files["tags/publico.html"]; has {
		t.Error("the publishing tag is not a subject")
	}
	// No base URL: no feed.
	if f, _, _ := GardenFiles(ctx, db, GardenOpts{Title: "x", Loc: time.UTC}); f["feed.xml"] != nil {
		t.Error("feed without a public address")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestGardenZipAndDirectory(t *testing.T) {
	db, ctx := gardenDB(t)
	n := &database.Node{Type: database.TypeNote, Title: "Olá Mundo", Content: "texto", Tags: []string{"publico"}}
	db.CreateNode(ctx, n)
	opts := GardenOpts{Title: "J", Tag: "publico", Loc: time.UTC}

	var buf bytes.Buffer
	if rep, err := Garden(ctx, db, &buf, opts); err != nil || len(rep.Pages) != 1 {
		t.Fatalf("zip = %+v, %v", rep, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "notas/ola-mundo.html" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			found = strings.Contains(string(b), "Olá Mundo")
		}
	}
	if !found {
		t.Fatal("note page not in the zip")
	}

	dir := filepath.Join(t.TempDir(), "site")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "CNAME"), []byte("jardim.exemplo.com"), 0o644) // not ours: must survive
	if _, err := WriteGarden(ctx, db, dir, opts); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(dir, "notas", "ola-mundo.html")
	if _, err := os.Stat(page); err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(filepath.Join(dir, "index.html"))
	time.Sleep(20 * time.Millisecond)
	if _, err := WriteGarden(ctx, db, dir, opts); err != nil {
		t.Fatal(err)
	}
	if st2, _ := os.Stat(filepath.Join(dir, "index.html")); !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("unchanged files must not be rewritten")
	}
	// Un-tagging removes the page; the user's own files stay.
	n.Tags = []string{"outra"}
	db.UpdateNode(ctx, n)
	if _, err := WriteGarden(ctx, db, dir, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(page); !os.IsNotExist(err) {
		t.Error("unpublished page still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "notas")); !os.IsNotExist(err) {
		t.Error("empty folder left behind")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CNAME")); string(b) != "jardim.exemplo.com" {
		t.Error("foreign file touched")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "index.html")); !strings.Contains(string(b), "Nenhuma nota publicada") {
		t.Error("empty index expected")
	}
}

func TestGardenSlugAndOptions(t *testing.T) {
	for in, want := range map[string]string{"Olá, Mundo!": "ola-mundo", "  Ação & Reação  ": "acao-reacao", "///": "nota", "C++ / Go": "c-go"} {
		if got := gardenSlug(in); got != want {
			t.Errorf("gardenSlug(%q) = %q, want %q", in, got, want)
		}
	}
	get := func(k string) string {
		return map[string]string{"GARDEN_TAG": "#Compartilhar", "GARDEN_URL": "https://x.com/", "BRAIN_NAME": "Cérebro"}[k]
	}
	o := GardenOptionsFrom(get, time.UTC)
	if o.Tag != "compartilhar" || o.BaseURL != "https://x.com" || o.Title != "Cérebro" {
		t.Errorf("options = %+v", o)
	}
	if o := GardenOptionsFrom(func(string) string { return "" }, time.UTC); o.Tag != "publico" {
		t.Errorf("default tag = %q", o.Tag)
	}
}
