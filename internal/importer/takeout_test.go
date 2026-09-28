package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inakano89/second-brain/internal/database"
)

func TestQueuedTakeoutImport(t *testing.T) {
	im, db, _, dir := setupImporter(t)
	ctx := context.Background()
	archive := zipBytes(t, map[string]string{
		"Takeout/Minha atividade/Pesquisa/MinhaAtividade.json": `[{"header":"Pesquisa","title":"Pesquisou por preço do café","titleUrl":"https://www.google.com/search?q=cafe","time":"2026-09-03T13:00:00Z","products":["Pesquisa"]}]`,
		"Takeout/Chrome/History.json":                          `{"Browser History":[{"title":"Go Docs","url":"https://go.dev/doc","time_usec":1788440400000000}]}`,
		"Takeout/Histórico de localização/Records.json":        `{"locations":[{"latitudeE7":1}]}`,
		"Takeout/Minha atividade/YouTube/MinhaAtividade.html":  "<html>histórico</html>",
		"Takeout/Keep/Ideia.json":                              `{"title":"Ideia","textContent":"Plano de estudos","isTrashed":false,"userEditedTimestampUsec":1700000000000000,"createdTimestampUsec":1700000000000000}`,
	})
	path := filepath.Join(dir, "inbox", "takeout-20260928T000000Z-001.zip")
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	var done []Report
	im.Done = func(_ context.Context, ft FileTask, rep Report) {
		if ft.Origin != "watcher" {
			t.Errorf("origin = %q", ft.Origin)
		}
		done = append(done, rep)
	}
	if err := im.Enqueue(ctx, FileTask{Path: path, Origin: "watcher", Remove: true}); err != nil {
		t.Fatal(err)
	}
	task, err := db.ClaimTask(ctx)
	if err != nil || task == nil || task.Kind != TaskImportFile {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if err := im.handleFile(ctx, task.Payload); err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Fatal("Done hook not called")
	}
	rep := done[0]
	if rep.Formats[FormatTakeout] != 2 || rep.Formats[FormatKeep] != 1 || rep.Created != 3 || !strings.Contains(strings.Join(rep.Warnings, " "), "JSON") {
		t.Fatalf("report = %+v", rep)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file not removed")
	}
	for _, ref := range []string{"activity-pesquisa:2026-09", "chrome:2026-09"} {
		n, err := db.GetNodeBySource(ctx, SourcePrefix+FormatTakeout, ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if n.Meta["enriched"] != true || n.Type != database.TypeNote {
			t.Fatalf("%s meta = %v", ref, n.Meta)
		}
	}
	if !strings.Contains(rep.Summary(), "3 novos") {
		t.Fatalf("summary = %q", rep.Summary())
	}

	// Re-importing the same export changes nothing; a missing file is a no-op.
	os.WriteFile(path, archive, 0o644)
	raw, _ := json.Marshal(FileTask{Path: path, Origin: "watcher"})
	if err := im.handleFile(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if last := done[len(done)-1]; last.Created != 0 || last.Updated != 0 || last.Skipped != 3 {
		t.Fatalf("re-import = %+v", last)
	}
	os.Remove(path)
	if err := im.handleFile(ctx, raw); err != nil || len(done) != 2 {
		t.Fatalf("missing file: %v, done=%d", err, len(done))
	}
}
