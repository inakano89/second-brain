package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func setupImporter(t *testing.T) (*Importer, *database.DB, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "data", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := agent.New(cfg, db, llm.NewManager(cfg, nil), log)
	return New(cfg, ag, log), db, cfg, dir
}

func TestImportArchive(t *testing.T) {
	im, db, cfg, dir := setupImporter(t)
	ctx := context.Background()
	// Person auto-created earlier from a mention: the vCard must enrich it, not duplicate it.
	if err := db.CreateNode(ctx, &database.Node{Type: database.TypePerson, Title: "Maria Souza", Source: "agent"}); err != nil {
		t.Fatal(err)
	}
	nested := zipBytes(t, map[string]string{"contatos.vcf": "BEGIN:VCARD\nVERSION:3.0\nFN:Maria Souza\nEMAIL:maria@exemplo.com\nEND:VCARD\n"})
	archive := zipBytes(t, map[string]string{
		"Vault/Projetos/Atlas.md":        "---\ntags: [trabalho]\n---\nReunião com [[Maria Souza]] sobre o [[Orçamento]].",
		"Vault/Orçamento.md":             "---\ntitle: Orçamento 2026\n---\nValores do projeto.",
		"Vault/.obsidian/workspace.json": `{"title":"não importar","content":"x"}`,
		"Vault/anexos/foto.png":          "\x89PNG",
		"Vault/pacote.zip":               string(nested),
		"Vault/db.csv":                   "Name,Notes\nLinha,duplicada do Notion\n",
		"Takeout/Keep/Ideia.json":        `{"isTrashed":false,"title":"Ideia","textContent":"Texto da ideia","userEditedTimestampUsec":1700000000000000}`,
		"Takeout/Keep/Ideia.html":        "<html><body><p>duplicada</p></body></html>",
		"Takeout/archive_browser.html":   "<html><body><p>índice</p></body></html>",
	})
	zp := filepath.Join(dir, "export.zip")
	os.WriteFile(zp, archive, 0o644)
	opml := filepath.Join(dir, "feeds.opml")
	os.WriteFile(opml, []byte(`<opml><body><outline text="Go" xmlUrl="https://go.dev/blog/feed.atom"/></body></opml>`), 0o644)

	files := []File{{Path: zp, Name: "export.zip"}, {Path: opml, Name: "feeds.opml"}}
	rep := im.Run(ctx, files, Options{Tags: []string{"migracao"}})
	if rep.State != StateDone || rep.Failed != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Created != 3 || rep.Updated != 1 || rep.Feeds != 1 || rep.Ignored != 4 {
		t.Fatalf("counts: created=%d updated=%d feeds=%d ignored=%d (%+v)", rep.Created, rep.Updated, rep.Feeds, rep.Ignored, rep)
	}
	if rep.Links < 2 || rep.Queued != 4 {
		t.Fatalf("links=%d queued=%d", rep.Links, rep.Queued)
	}
	people, _ := db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypePerson}})
	if len(people) != 1 || !strings.Contains(people[0].Content, "maria@exemplo.com") {
		t.Fatalf("person not merged: %+v", people)
	}
	atlas, err := db.GetNodeBySource(ctx, "import:markdown", "Vault/Projetos/Atlas.md") // "Takeout/" also at root: no common folder
	if err != nil {
		t.Fatal(err)
	}
	if atlas.Meta[agent.MetaNoLLM] != true || !contains(atlas.Tags, "migracao") || !contains(atlas.Tags, "trabalho") {
		t.Fatalf("atlas node: %+v", atlas)
	}
	links, _ := db.Neighbors(ctx, atlas.ID)
	var targets []string
	for _, l := range links {
		targets = append(targets, l.Node.Title)
	}
	if !contains(targets, "Orçamento 2026") || !contains(targets, "Maria Souza") {
		t.Fatalf("wiki-links by file name not resolved: %v", targets)
	}
	if !strings.Contains(cfg.Get("RSS_FEEDS"), "https://go.dev/blog/feed.atom") {
		t.Fatalf("feed not added: %q", cfg.Get("RSS_FEEDS"))
	}
	if st, _ := db.QueueStats(ctx); st["pending"] != 4 {
		t.Fatalf("enrichment not queued: %+v", st)
	}
	if err := im.ag.Enrich(ctx, atlas.ID); err != nil {
		t.Fatal(err)
	}

	// Re-import: idempotent (nothing new, nothing changed).
	again := im.Run(ctx, files, Options{Tags: []string{"migracao"}})
	if again.Created != 0 || again.Updated != 0 || again.Skipped != 4 || again.Feeds != 0 {
		t.Fatalf("re-import not idempotent: %+v", again)
	}
	if n, _ := db.CountByType(ctx); n[database.TypeNote] != 3 {
		t.Fatalf("duplicated notes: %+v", n)
	}
}

func TestImportJobAsync(t *testing.T) {
	im, _, _, dir := setupImporter(t)
	os.MkdirAll(im.UploadDir(), 0o755)
	src := filepath.Join(im.UploadDir(), "up.csv")
	os.WriteFile(src, []byte("title,content\nA,um\nB,dois\nC,três\n"), 0o600)
	bad := filepath.Join(dir, "x.bin")
	os.WriteFile(bad, []byte{0, 1, 2}, 0o600)

	j := im.Start([]File{{Path: src, Name: "notas.csv"}}, Options{}, true)
	failed := im.Start([]File{{Path: bad, Name: "x.bin"}}, Options{}, false)
	deadline := time.Now().Add(10 * time.Second)
	for (j.Report().Running() || failed.Report().Running()) && time.Now().Before(deadline) {
		_ = im.Jobs() // concurrent readers while the jobs run (race detector)
		time.Sleep(10 * time.Millisecond)
	}
	rep := j.Report()
	if rep.State != StateDone || rep.Created != 3 || rep.Percent() != 100 || rep.FormatList() != "csv (3)" {
		t.Fatalf("job: %+v", rep)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("upload not removed after job")
	}
	if f := failed.Report(); f.State != StateFailed || len(f.Errors) == 0 {
		t.Fatalf("unknown format should fail: %+v", f)
	}
	if got, ok := im.Job(rep.ID); !ok || got != j || len(im.Jobs()) != 2 {
		t.Fatal("job registry")
	}
}

func TestZipBudget(t *testing.T) {
	big := strings.Repeat("a", 4<<10)
	archive := zipBytes(t, map[string]string{"a.md": big, "b.md": big, "c.md": big})
	p := newParser(time.UTC, 1<<20)
	p.budget.Store(6 << 10)
	if _, err := p.parseZip(context.Background(), bytes.NewReader(archive), int64(len(archive)), 0); err == nil || !strings.Contains(err.Error(), "limite") {
		t.Fatalf("zip bomb guard: %v", err)
	}
}

func contains(list []string, v string) bool { return has(list, v) }

func TestZipRootFolderStripped(t *testing.T) {
	archive := zipBytes(t, map[string]string{"MyVault/a.md": "um", "MyVault/sub/b.md": "dois"})
	b, err := newTestParser().parseZip(context.Background(), bytes.NewReader(archive), int64(len(archive)), 0)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, it := range b.Items {
		refs = append(refs, it.Ref)
	}
	if !has(refs, "a.md") || !has(refs, "sub/b.md") {
		t.Fatalf("refs: %v", refs)
	}
}
