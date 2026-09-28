package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) KVGet(_ context.Context, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *memKV) KVSet(_ context.Context, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

func TestBuiltinCatalogMatchesSchemaDefaults(t *testing.T) {
	b := BuiltinCatalog()
	f, _ := config.Lookup("LLM_MODELS")
	want := strings.Join(append(b.Specs(), "ollama:llama3.1"), ",")
	if f.Default != want {
		t.Fatalf("schema LLM_MODELS default drifted from models.json:\n got %s\nwant %s", f.Default, want)
	}
	for p := range b.Providers {
		f, _ := config.Lookup(DefaultModelKey(p))
		if f.Default != b.DefaultFor(p) {
			t.Fatalf("schema %s default = %q, models.json = %q", DefaultModelKey(p), f.Default, b.DefaultFor(p))
		}
	}
	for _, spec := range b.Specs() {
		if m, _ := b.Lookup(spec); len(m.Price) != 2 {
			t.Errorf("%s has no price in models.json", spec)
		}
	}
}

func TestParseCuratedCatalogRejectsBadDocs(t *testing.T) {
	for _, doc := range []string{
		`{}`,
		`{"revision":2,"providers":{"foo":{"default":"a","models":[{"id":"a"}]}}}`,
		`{"revision":2,"providers":{"openai":{"default":"b","models":[{"id":"a"}]}}}`,
		`{"revision":2,"providers":{"openai":{"default":"a","models":[{"id":"a b"}]}}}`,
		`{"revision":2,"providers":{"openai":{"default":"a","models":[{"id":"a","price":[-1,2]}]}}}`,
		`{"revision":2,"providers":{"openai":{"default":"a","models":[{"id":"a"},{"id":"a"}]}}}`,
	} {
		if _, err := ParseCuratedCatalog([]byte(doc)); err == nil {
			t.Fatalf("accepted invalid catalogue %s", doc)
		}
	}
}

func TestPlanCatalogFromLegacy(t *testing.T) {
	env := map[string]string{
		"LLM_MODELS":          "anthropic:claude-opus-5,anthropic:claude-sonnet-5,anthropic:claude-haiku-4-5,openai:gpt-4o-mini,openai:gpt-4.1,openai:gpt-4o,gemini:gemini-2.5-flash,gemini:gemini-2.5-pro,ollama:llama3.1",
		"ANTHROPIC_MODEL":     "claude-opus-5",
		"OPENAI_MODEL":        "gpt-4o", // user's own pick: kept
		"GEMINI_MODEL":        "gemini-2.5-flash",
		"LLM_ROUTE_RSS":       "gemini:gemini-2.5-pro",
		"LLM_ROUTE_CHAT":      "council",
		"LLM_ROUTE_ENRICH":    "anthropic:claude-haiku-4-5", // cheap model: must go to its successor, not to Opus
		"LLM_ROUTE_EMAIL":     "anthropic:claude-sonnet-5",  // declared successor (replaces)
		"LLM_COUNCIL_MEMBERS": "anthropic:claude-opus-5,anthropic,openai,gemini:gemini-2.5-pro",
		"LLM_COUNCIL_JUDGE":   "anthropic:claude-opus-5",
	}
	plan := PlanCatalog(func(k string) string { return env[k] }, legacyCatalog, BuiltinCatalog())
	c := plan.Changes
	wantModels := strings.Join(append(BuiltinCatalog().Specs(), "openai:gpt-4o", "ollama:llama3.1"), ",")
	if c["LLM_MODELS"] != wantModels {
		t.Fatalf("LLM_MODELS = %s", c["LLM_MODELS"])
	}
	if c["ANTHROPIC_MODEL"] != "claude-opus-5-5" || c["GEMINI_MODEL"] != "gemini-3.1-pro-preview" {
		t.Fatalf("defaults not moved: %v", c)
	}
	if _, ok := c["OPENAI_MODEL"]; ok {
		t.Fatal("user-chosen default must be kept")
	}
	if c["LLM_ROUTE_RSS"] != "gemini" || c["LLM_COUNCIL_JUDGE"] != "anthropic" || c["LLM_COUNCIL_MEMBERS"] != "anthropic,openai,gemini" {
		t.Fatalf("routes/council not detached: %v", c)
	}
	if c["LLM_ROUTE_ENRICH"] != "anthropic:claude-haiku-4-5-20251001" || c["LLM_ROUTE_EMAIL"] != "anthropic:claude-sonnet-5-5" {
		t.Fatalf("successor not used: %q / %q", c["LLM_ROUTE_ENRICH"], c["LLM_ROUTE_EMAIL"])
	}
	if _, ok := c["LLM_ROUTE_CHAT"]; ok {
		t.Fatal("untouched route rewritten")
	}
	if len(plan.Removed) != 7 || len(plan.Summary()) == 0 {
		t.Fatalf("removed = %v", plan.Removed)
	}
}

func TestPlanCatalogKeepsUserRemovals(t *testing.T) {
	prev := BuiltinCatalog()
	var next CuratedCatalog
	raw, _ := json.Marshal(prev)
	_ = json.Unmarshal(raw, &next)
	next.Revision++
	op := next.Providers["openai"]
	op.Models = append(op.Models, CatalogModel{ID: "gpt-6-nova"})
	next.Providers["openai"] = op

	var cur []string
	for _, s := range prev.Specs() {
		if s != "openai:gpt-6-luna" { // removed by the user
			cur = append(cur, s)
		}
	}
	env := map[string]string{"LLM_MODELS": strings.Join(cur, ",")}
	for p := range prev.Providers {
		env[DefaultModelKey(p)] = prev.DefaultFor(p)
	}
	plan := PlanCatalog(func(k string) string { return env[k] }, prev, &next)
	got := plan.Changes["LLM_MODELS"]
	if strings.Contains(got, "gpt-6-luna") || !strings.Contains(got, "openai:gpt-6-nova") {
		t.Fatalf("LLM_MODELS = %s", got)
	}
	if len(plan.Changes) != 1 || len(plan.Added) != 1 {
		t.Fatalf("unexpected changes: %v", plan.Changes)
	}
}

func TestCatalogSync(t *testing.T) {
	var next CuratedCatalog
	raw, _ := json.Marshal(BuiltinCatalog())
	_ = json.Unmarshal(raw, &next)
	next.Revision = BuiltinCatalog().Revision + 1
	next.Updated = "2099-01-01"
	g := next.Providers["gemini"]
	g.Models = append(g.Models, CatalogModel{ID: "gemini-9-ultra", Price: []float64{7, 70}})
	g.Default = "gemini-9-ultra"
	next.Providers["gemini"] = g
	doc, _ := json.Marshal(next)
	var hits atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "nope", http.StatusBadGateway)
			return
		}
		w.Write(doc)
	}))
	defer srv.Close()

	env := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(env, []byte("GEMINI_API_KEY=k\nLLM_MODELS=gemini:gemini-2.5-flash,anthropic:claude-opus-5\nANTHROPIC_MODEL=claude-opus-5\n"), 0o600)
	cfg, _ := config.Load(env)
	m := NewManager(cfg, nil)
	kv := &memKV{m: map[string]string{}}
	s := NewCatalogSync(cfg, m, kv, slog.New(slog.DiscardHandler))
	s.URL = func() string { return srv.URL }
	var notified atomic.Int32
	s.Notify = func(context.Context, string) error { notified.Add(1); return nil }

	// Startup applies the embedded catalogue without touching the network.
	s.Init(context.Background())
	if hits.Load() != 0 {
		t.Fatal("Init must not fetch")
	}
	if cfg.Get("ANTHROPIC_MODEL") != "claude-opus-5-5" || strings.Contains(cfg.Get("LLM_MODELS"), "claude-opus-5,") {
		t.Fatalf("builtin not applied: %s / %s", cfg.Get("ANTHROPIC_MODEL"), cfg.Get("LLM_MODELS"))
	}
	if st := s.Status(); st.Revision != BuiltinCatalog().Revision || len(st.Changes) == 0 {
		t.Fatalf("status after init: %+v", st)
	}

	// Remote revision wins and brings prices and a new default.
	st, err := s.Sync(context.Background(), true)
	if err != nil || st.Revision != next.Revision || st.Source != "GitHub" {
		t.Fatalf("sync: %+v %v", st, err)
	}
	if cfg.Get("GEMINI_MODEL") != "gemini-9-ultra" || !strings.Contains(cfg.Get("LLM_MODELS"), "gemini:gemini-9-ultra") {
		t.Fatalf("remote not applied: %s", cfg.Get("LLM_MODELS"))
	}
	if p, ok := m.PriceOf("gemini", "gemini-9-ultra"); !ok || p.Out != 70 {
		t.Fatalf("curated price missing: %v %v", p, ok)
	}
	if fmt.Sprint(m.Suggestions("gemini")) == "" || !strings.Contains(fmt.Sprint(m.Suggestions("gemini")), "gemini-9-ultra") {
		t.Fatal("suggestions not refreshed")
	}

	// Same revision again: no changes; a failing remote reports the error but keeps state.
	if st, _ := s.Sync(context.Background(), true); len(st.Changes) != 0 {
		t.Fatalf("idempotent sync changed: %v", st.Changes)
	}
	fail.Store(true)
	st, err = s.Sync(context.Background(), true)
	if err == nil || st.Err == "" || st.Revision != next.Revision {
		t.Fatalf("failed fetch: %+v %v", st, err)
	}

	// A restart re-reads the applied revision from the store.
	s2 := NewCatalogSync(cfg, NewManager(cfg, nil), kv, slog.New(slog.DiscardHandler))
	s2.Init(context.Background())
	if s2.Status().Revision != next.Revision {
		t.Fatalf("applied revision not persisted: %+v", s2.Status())
	}
}
