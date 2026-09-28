package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/inakano89/second-brain/internal/config"
)

// fakeOpenAI answers every chat request with name + call counter and records system prompts.
func fakeOpenAI(t *testing.T, name string, calls *atomic.Int32, systems *[]string, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		n := calls.Add(1)
		if len(body.Messages) > 0 && body.Messages[0].Role == "system" {
			mu.Lock()
			*systems = append(*systems, fmt.Sprint(body.Messages[0].Content))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": fmt.Sprintf("%s resposta %d (%d msgs)", name, n, len(body.Messages))}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
}

func TestCouncilAndRouting(t *testing.T) {
	var ca, cb atomic.Int32
	var mu sync.Mutex
	var sysA, sysB []string
	a := fakeOpenAI(t, "A", &ca, &sysA, &mu)
	b := fakeOpenAI(t, "B", &cb, &sysB, &mu)
	defer a.Close()
	defer b.Close()

	env := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(env, []byte(fmt.Sprintf(`OPENAI_API_KEY=k
OPENAI_BASE_URL=%s
OPENAI_MODEL=gpt-x
OLLAMA_BASE_URL=%s/v1
OLLAMA_MODEL=local-x
DEFAULT_LLM_PROVIDER=openai
LLM_COUNCIL_MEMBERS=openai,ollama
LLM_COUNCIL_JUDGE=openai
LLM_COUNCIL_ROUNDS=1
LLM_ROUTE_RSS=ollama
LLM_ROUTE_ENRICH=anthropic
`, a.URL, b.URL)), 0o600)
	cfg, _ := config.Load(env)
	m := NewManager(cfg, nil)

	// Routing: rss → ollama (B); enrich → anthropic (no key) degrades to auto (A).
	if _, err := m.Complete(context.Background(), "", Request{Purpose: "rss", Messages: []Message{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if cb.Load() != 1 || ca.Load() != 0 {
		t.Fatalf("rss route: A=%d B=%d", ca.Load(), cb.Load())
	}
	if _, err := m.Complete(context.Background(), "", Request{Purpose: "enrich", Messages: []Message{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if ca.Load() != 1 {
		t.Fatalf("unconfigured route should fall back to auto (A=%d)", ca.Load())
	}
	ca.Store(0)
	cb.Store(0)

	var ops []Opinion
	var opsMu sync.Mutex
	resp, err := m.Council(context.Background(), Request{Purpose: "briefing", System: "base", Messages: []Message{{Role: RoleUser, Content: "decida"}}}, nil,
		func(o Opinion) { opsMu.Lock(); ops = append(ops, o); opsMu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	// 2 members × (1 initial + 1 critique) + 1 judge (A).
	if ca.Load() != 3 || cb.Load() != 2 || len(ops) != 4 || len(resp.Council) != 4 {
		t.Fatalf("calls A=%d B=%d opinions=%d", ca.Load(), cb.Load(), len(ops))
	}
	judgeSys := sysA[len(sysA)-1]
	if !strings.Contains(judgeSys, "posicoes_do_conselho") || !strings.Contains(judgeSys, "Modelo B") {
		t.Fatalf("judge prompt missing positions: %s", judgeSys)
	}
	finals := FinalOpinions(resp.Council)
	if len(finals) != 2 || finals[0].Round != 1 {
		t.Fatalf("final opinions: %+v", finals)
	}
	if m.CouncilSetup().Members[1] != "ollama:local-x" {
		t.Fatalf("members: %v", m.CouncilSetup().Members)
	}
	// Catalogue always includes provider defaults.
	found := false
	for _, e := range m.Catalog() {
		if e.Spec() == "openai:gpt-x" && e.Default && e.Configured {
			found = true
		}
	}
	if !found {
		t.Fatal("default model missing from catalogue")
	}
}
