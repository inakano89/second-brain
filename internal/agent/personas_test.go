package agent

import (
	"context"
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

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

// chatEnv is an agent wired to a fake OpenAI endpoint that records every request body.
func chatEnv(t *testing.T) (*Agent, *database.DB, func() []string) {
	t.Helper()
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
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Certo."}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
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
	t.Cleanup(func() { db.Close() })
	a := New(cfg, db, llm.NewManager(cfg, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return a, db, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func TestPersonaPresets(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Personas {
		if p.Key == "" || p.Key == PersonaCustom || seen[p.Key] {
			t.Errorf("bad or duplicate key %q", p.Key)
		}
		seen[p.Key] = true
		if p.Name == "" || p.Icon == "" || p.Theme == "" || p.Blurb == "" || p.Greeting == "" || p.Role == "" || p.Topics == "" {
			t.Errorf("persona %s incomplete: %+v", p.Key, p)
		}
		for _, w := range strings.Fields(strings.NewReplacer(",", " ").Replace(p.Topics)) {
			if len([]rune(w)) < 3 { // short words match half the brain in the prefix search
				t.Errorf("persona %s: topic word %q too short for the full-text query", p.Key, w)
			}
		}
		if !ValidPersona(p.Key) || PersonaFor(p.Key, "").Key != p.Key {
			t.Errorf("persona %s not resolvable", p.Key)
		}
	}
	if !ValidPersona("") || !ValidPersona(PersonaCustom) || ValidPersona("mago") {
		t.Fatal("ValidPersona wrong")
	}
	if PersonaFor("mago", "").Key != "" {
		t.Fatal("unknown key must fall back to the plain assistant")
	}
	groups := PersonaGroups()
	total := 0
	for _, g := range groups {
		total += len(g.Personas)
	}
	if len(groups) < 4 || total != len(Personas) {
		t.Fatalf("groups: %d groups, %d personas of %d", len(groups), total, len(Personas))
	}
	if rolePrompt(GeneralPersona, "") != "" {
		t.Fatal("plain assistant has no role block")
	}
	if got := rolePrompt(GeneralPersona, "seja breve"); !strings.Contains(got, "seja breve") || strings.Contains(got, "Neste chat você atua") {
		t.Fatalf("plain assistant with extra instructions: %q", got)
	}
	if got := topicWords("Você é o meu coach de corrida e maratonas, fale sobre corrida"); got != "coach corrida maratonas" {
		t.Fatalf("topicWords: %q", got)
	}
	c := customPersona("Você é meu coach de corrida.")
	if got := rolePrompt(c, "Você é meu coach de corrida."); strings.Count(got, "coach de corrida") != 1 || !strings.Contains(got, "Personalizado") {
		t.Fatalf("custom persona duplicated or missing: %q", got)
	}
}

// TestPersonaChatKnowsSources: a themed chat gets its role and the user's notes on the theme,
// even when the question does not mention them; the plain chat gets neither block.
func TestPersonaChatKnowsSources(t *testing.T) {
	a, db, bodies := chatEnv(t)
	lastBody := func() string { // the request is JSON: "<" and ">" travel escaped
		b := bodies()
		return strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(b[len(b)-1])
	}
	ctx := context.Background()
	for _, n := range []*database.Node{
		{Type: database.TypeNote, Title: "Exames de sangue março", Content: "Colesterol LDL 162 mg/dL, acima do desejável. Glicemia 90."},
		{Type: database.TypeNote, Title: "Ideias de logo", Content: "Cores quentes para o projeto Atlas."},
	} {
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	res, err := a.Chat(ctx, ChatRequest{Channel: "web:1", Persona: "medico", Text: "Como está minha saúde?"}, nil, nil)
	if err != nil || res.Text != "Certo." {
		t.Fatalf("chat: %+v %v", res, err)
	}
	body := lastBody()
	for _, want := range []string{"<papel>", "MÉDICO", "<fontes_do_tema>", "Colesterol LDL 162"} {
		if !strings.Contains(body, want) {
			t.Errorf("themed request lacks %q", want)
		}
	}
	if strings.Contains(body, "Cores quentes") {
		t.Error("theme sources must be about the theme")
	}
	if len(res.Context) == 0 || res.Context[0].Title != "Exames de sangue março" {
		t.Errorf("sources not reported to the UI: %+v", res.Context)
	}

	if _, err := a.Chat(ctx, ChatRequest{Channel: "web:2", Text: "Como está minha saúde?"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if plain := lastBody(); strings.Contains(plain, "<papel>") || strings.Contains(plain, "<fontes_do_tema>") {
		t.Error("plain chat must not carry a persona")
	}

	// Custom persona: the instructions are the role and also say what to look for.
	if _, err := a.Chat(ctx, ChatRequest{Channel: "web:3", Persona: PersonaCustom, Instructions: "Você é meu revisor de logotipos e cores.", Text: "Oi"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if c := lastBody(); !strings.Contains(c, "revisor de logotipos") || !strings.Contains(c, "Cores quentes") {
		t.Errorf("custom persona request: %s", c)
	}

	// Each chat keeps its own history.
	if h, _ := a.ChatHistory(ctx, "web:1", 10); len(h) != 2 {
		t.Fatalf("web:1 history: %+v", h)
	}
	if h, _ := a.ChatHistory(ctx, "web:9", 10); len(h) != 0 {
		t.Fatalf("web:9 history: %+v", h)
	}
}

// TestChatsRunConcurrently: tabs answer at the same time without mixing histories (-race).
func TestChatsRunConcurrently(t *testing.T) {
	a, _, _ := chatEnv(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			persona := Personas[i%len(Personas)].Key
			ch := database.ChatChannel(int64(i))
			for n := 0; n < 3; n++ {
				if _, err := a.Chat(ctx, ChatRequest{Channel: ch, Persona: persona, Text: fmt.Sprintf("mensagem %d-%d", i, n)}, nil, nil); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i := 1; i <= 8; i++ {
		h, _ := a.ChatHistory(ctx, database.ChatChannel(int64(i)), 20)
		if len(h) != 6 {
			t.Fatalf("chat %d has %d messages", i, len(h))
		}
		for _, m := range h {
			if m.Role == "user" && !strings.HasPrefix(m.Content, fmt.Sprintf("mensagem %d-", i)) {
				t.Fatalf("chat %d got a message from another chat: %q", i, m.Content)
			}
		}
	}
}
