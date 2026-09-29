package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/profile"
)

// TestPrivateChatTurns: a turn that reads the profile is stored encrypted, stays out of
// the memory routine and is not replayed to a cloud model under PROFILE_AI_ACCESS=basic.
func TestPrivateChatTurns(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(b, &req)
		last := req.Messages[len(req.Messages)-1]
		var delta map[string]any
		switch {
		case last.Role == "tool":
			delta = map[string]any{"content": "Sua academia é a Academia Forte, às 18h."}
		case strings.Contains(fmt.Sprint(last.Content), "academia"):
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "function": map[string]any{"name": toolProfile, "arguments": `{"query":"academia"}`}}}}
		default:
			delta = map[string]any{"content": "Olá!"}
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", out)
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte(fmt.Sprintf("OPENAI_API_KEY=k\nOPENAI_BASE_URL=%s\nOPENAI_MODEL=gpt-x\nDEFAULT_LLM_PROVIDER=openai\nEMBEDDING_PROVIDER=local\nLLM_ROUTE_CHAT=openai\n", srv.URL)), 0o600)
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := New(cfg, db, llm.NewManager(cfg, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	gym := profile.Item{Kind: "enrollment", Title: "Academia Forte", Values: map[string]string{"time": "18:00"}}
	if err := a.Profile().Save(ctx, &gym); err != nil {
		t.Fatal(err)
	}

	res, err := a.Chat(ctx, ChatRequest{Channel: "web", Text: "Que horas é a academia?"}, nil, nil)
	if err != nil || !strings.Contains(res.Text, "Academia Forte") {
		t.Fatalf("chat: %+v %v", res, err)
	}
	var raw []string
	rows, _ := db.QueryContext(ctx, `SELECT content FROM chat_messages WHERE private = 1`)
	for rows.Next() {
		var c string
		rows.Scan(&c)
		raw = append(raw, c)
	}
	rows.Close()
	if len(raw) != 2 || strings.Contains(strings.Join(raw, ""), "Academia") || !strings.HasPrefix(raw[1], "enc1:") {
		t.Fatalf("private turn stored in clear: %q", raw)
	}
	if since, _ := db.ChatSince(ctx, time.Now().Add(-time.Hour), 100); len(since) != 0 {
		t.Fatalf("memory would read private turns: %+v", since)
	}
	if hist, _ := a.ChatHistory(ctx, "web", 10); len(hist) != 2 || !strings.Contains(hist[1].Content, "Academia Forte") {
		t.Fatalf("display history: %+v", hist)
	}

	// Next turn on a cloud model: the private turn is not replayed.
	if _, err := a.Chat(ctx, ChatRequest{Channel: "web", Text: "Oi"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	if strings.Contains(last, "Academia Forte") || strings.Contains(last, "Que horas") {
		t.Fatalf("private turn replayed to cloud model: %s", last)
	}
	// With PROFILE_AI_ACCESS=full it is.
	cfg.Update(map[string]string{"PROFILE_AI_ACCESS": profile.AccessFull})
	if _, err := a.Chat(ctx, ChatRequest{Channel: "web", Text: "Oi de novo"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	last = bodies[len(bodies)-1]
	mu.Unlock()
	if !strings.Contains(last, "Academia Forte") {
		t.Fatalf("private turn should be replayed with full access: %s", last)
	}
}

// TestProfileSaveTool: the write tool exists only with PROFILE_AI_WRITE, fills the profile
// and never echoes stored data back to the model.
func TestProfileSaveTool(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("EMBEDDING_PROVIDER=local\n"), 0o600)
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := New(cfg, db, llm.NewManager(cfg, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	has := func() bool {
		for _, tl := range a.Tools() {
			if tl.Name == toolProfileSave {
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("save tool offered without PROFILE_AI_WRITE")
	}
	cfg.Update(map[string]string{"PROFILE_AI_WRITE": "true"})
	if !has() {
		t.Fatal("save tool missing with PROFILE_AI_WRITE")
	}

	call := llm.ToolCall{ID: "c1", Name: toolProfileSave, Arguments: json.RawMessage(`{"kind":"medication","title":"Losartana","fields":[{"key":"dose","value":"50 mg"},{"key":"times","value":"08:00, 20:00"},{"key":"stock","value":"30"}]}`)}
	res := a.ExecuteTool(ctx, call)
	if !strings.Contains(res, `"created":true`) || strings.Contains(res, "error") {
		t.Fatalf("save: %s", res)
	}
	// Update by title: the answer only repeats what the model sent (no stored fields).
	call.Arguments = json.RawMessage(`{"kind":"medication","title":"losartana","fields":[{"key":"stock","value":"28"}]}`)
	res = a.ExecuteTool(ctx, call)
	if !strings.Contains(res, `"created":false`) || strings.Contains(res, "50 mg") {
		t.Fatalf("update leaked or failed: %s", res)
	}
	items, _ := a.Profile().List(ctx, false)
	if len(items) != 1 || items[0].Get("dose") != "50 mg" || items[0].Get("stock") != "28" || !items[0].Sensitive {
		t.Fatalf("stored: %+v", items)
	}
	bad := a.ExecuteTool(ctx, llm.ToolCall{Name: toolProfileSave, Arguments: json.RawMessage(`{"kind":"medication","title":"X","fields":[{"key":"cor","value":"azul"}]}`)})
	if !strings.Contains(bad, "error") {
		t.Fatalf("unknown field accepted: %s", bad)
	}
}
