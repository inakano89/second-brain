package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

// TestNotifyRetryIsEncrypted: a notification that fails is queued encrypted and the
// retry delivers the original text.
func TestNotifyRetryIsEncrypted(t *testing.T) {
	var (
		mu   sync.Mutex
		fail = true
		got  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`)
			return
		}
		got = append(got, body["text"].(string))
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("TELEGRAM_BOT_TOKEN=t\nALLOWED_TELEGRAM_USER_IDS=42\n"), 0o600)
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, db, agent.New(cfg, db, llm.NewManager(cfg, nil), log), log)
	ctx := context.Background()

	if err := s.Notify(ctx, "⏰ Lembrete: 💊 Losartana 50 mg"); err == nil {
		t.Fatal("expected send error")
	}
	var payload string
	if err := db.QueryRowContext(ctx, `SELECT payload FROM task_queue WHERE kind = ?`, TaskSend).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "Losartana") || !strings.Contains(payload, "enc1:") {
		t.Fatalf("queued in clear: %s", payload)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := s.sendQueued(ctx, json.RawMessage(payload)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "⏰ Lembrete: 💊 Losartana 50 mg" {
		t.Fatalf("retry sent %q", got)
	}
}
