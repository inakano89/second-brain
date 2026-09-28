package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/integrations/google"
	"github.com/inakano89/second-brain/internal/integrations/zepp"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/telegram"
)

type env struct {
	t      *testing.T
	h      http.Handler
	cookie *http.Cookie
	cfg    *config.Config
	db     *database.DB
	ag     *agent.Agent
}

func (e *env) do(method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if e.cookie != nil {
		req.AddCookie(e.cookie)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) form(path string, v url.Values) *httptest.ResponseRecorder {
	return e.do("POST", path, strings.NewReader(v.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func (e *env) expect(rec *httptest.ResponseRecorder, code int, contains ...string) {
	e.t.Helper()
	if rec.Code != code {
		e.t.Fatalf("status %d (want %d): %s", rec.Code, code, truncate(rec.Body.String()))
	}
	for _, c := range contains {
		if !strings.Contains(rec.Body.String(), c) {
			e.t.Fatalf("body missing %q: %s", c, truncate(rec.Body.String()))
		}
	}
}

func truncate(s string) string {
	if len(s) > 1500 {
		return s[:1500]
	}
	return s
}

func setup(t *testing.T) *env {
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
	m := llm.NewManager(cfg, nil)
	ag := agent.New(cfg, db, m, log)
	gc := google.New(cfg, db, log)
	ag.SetCalendar(gc)
	srv, err := New(Deps{Cfg: cfg, DB: db, LLM: m, Agent: ag, Google: gc, Zepp: zepp.New(cfg, db, ag, log), Telegram: telegram.New(cfg, db, ag, log), Log: log, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, h: srv.Handler(), cfg: cfg, db: db, ag: ag}
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	rec := e.do("GET", "/", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("expected setup redirect, got %d %s", rec.Code, rec.Header().Get("Location"))
	}
	e.expect(e.do("GET", "/setup", nil, nil), 200, "Primeiro acesso")
	e.expect(e.form("/setup", url.Values{"username": {"admin"}, "password": {"curta"}, "password2": {"curta"}, "http_port": {"8080"}}), 400, "ao menos 8")
	rec = e.form("/setup", url.Values{
		"brain_name": {"Cérebro Teste"}, "username": {"admin"}, "password": {"senha-forte-1"}, "password2": {"senha-forte-1"},
		"http_port": {"8080"}, "timezone": {"America/Sao_Paulo"}, "telegram_ids": {"123, 456"},
	})
	e.expect(rec, 200, "Tudo pronto")
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			e.cookie = c
		}
	}
	if e.cookie == nil || !e.cfg.SetupCompleted() || e.cfg.Get("BACKUP_ENCRYPTION_KEY") == "" {
		t.Fatal("setup did not persist session/config")
	}
	token := e.cfg.Get("API_TOKEN")

	// Login flow.
	anon := *e
	anon.cookie = nil
	e.expect(anon.form("/login", url.Values{"username": {"admin"}, "password": {"errada"}}), 401, "inválidos")
	if r := anon.form("/login", url.Values{"username": {"admin"}, "password": {"senha-forte-1"}, "next": {"/chat"}}); r.Code != 303 || r.Header().Get("Location") != "/chat" {
		t.Fatalf("login failed: %d", r.Code)
	}

	e.expect(e.do("GET", "/", nil, nil), 200, "Cérebro Teste", "graph.js")
	e.expect(e.form("/nodes", url.Values{"type": {"note"}, "title": {"Projeto Atlas"}, "content": {"Reunião com [[Maria Souza]] sobre orçamento do projeto Atlas. #planejamento\n- [ ] enviar proposta revisada"}, "tags": {"trabalho"}}), 200, "Projeto Atlas")

	rec = e.do("POST", "/api/nodes", strings.NewReader(`{"type":"person","title":"Maria Souza","content":"Gerente financeira"}`), map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
	e.expect(rec, 201)
	e.expect(anon.do("POST", "/api/nodes", strings.NewReader(`{}`), map[string]string{"Authorization": "Bearer nope"}), 401)

	// Run enrichment synchronously (offline heuristics + local embeddings).
	if err := e.ag.Enrich(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.ag.Enrich(ctx, 2); err != nil {
		t.Fatal(err)
	}
	n, _ := e.db.GetNode(ctx, 1)
	if len(n.Tags) == 0 || n.Summary == "" {
		t.Fatalf("enrichment missing: %+v", n)
	}
	links, _ := e.db.Neighbors(ctx, 1)
	if len(links) == 0 {
		t.Fatal("wiki link not created")
	}
	tasks, _ := e.db.OpenTasks(ctx, 10)
	if len(tasks) != 1 || !strings.Contains(tasks[0].Title, "proposta") {
		t.Fatalf("task extraction: %+v", tasks)
	}

	e.expect(e.do("GET", "/search?q=orcamento", nil, nil), 200, "Projeto Atlas")
	rec = e.do("GET", "/api/graph", nil, nil)
	e.expect(rec, 200)
	var g struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	json.Unmarshal(rec.Body.Bytes(), &g)
	if len(g.Nodes) < 3 || len(g.Edges) < 2 {
		t.Fatalf("graph too small: %d nodes %d edges", len(g.Nodes), len(g.Edges))
	}
	e.expect(e.do("GET", "/api/graph?q=atlas", nil, nil), 200, "Projeto Atlas")
	e.expect(e.do("GET", "/nodes/1", nil, nil), 200, "Projeto Atlas", "Conexões", `class="wikilink"`)
	e.expect(e.do("GET", "/nodes/1/edit", nil, nil), 200, "Editar #1")
	e.expect(e.form("/nodes/1", url.Values{"type": {"note"}, "title": {"Projeto Atlas v2"}, "content": {"novo **conteúdo**"}, "tags": {"a, b"}}), 200, "Projeto Atlas v2", "<strong>conteúdo</strong>")
	e.expect(e.form("/nodes/3/toggle", url.Values{"status": {"done"}}), 200, "Reabrir")

	e.expect(e.do("POST", "/api/health/webhook", strings.NewReader(`{"date":"2026-09-27","metrics":{"sleep_minutes":450,"deep_sleep_minutes":90,"resting_hr":55,"steps":9000}}`), map[string]string{"Authorization": "Bearer " + token}), 200, `"stored":1`)
	ms, _ := e.db.MetricsRange(ctx, "2026-09-27", "2026-09-27")
	if len(ms) < 5 { // includes estimated recovery score
		t.Fatalf("metrics: %+v", ms)
	}
	e.expect(e.do("POST", "/api/clip", strings.NewReader(`{"url":"https://exemplo.com/a","title":"Artigo","html":"<p>Trecho <b>importante</b></p>"}`), map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}), 201)
	e.expect(anon.form("/api/clip", url.Values{"url": {"https://exemplo.com/b"}, "title": {"Outro"}, "html": {"<p>x</p>"}, "token": {token}}), 200, "Salvo no")

	e.expect(e.do("GET", "/dashboard", nil, nil), 200, "Consumo de LLM", "Integrações")
	e.expect(e.do("GET", "/logs", nil, nil), 200, "Audit log")
	e.expect(e.do("GET", "/logs/rows?level=INFO", nil, nil), 200)
	e.expect(e.do("GET", "/chat", nil, nil), 200, "Nenhum provedor LLM")
	rec = e.do("GET", "/settings", nil, nil)
	e.expect(rec, 200, `href="javascript:%28function`, "Editor do .env") // browsers percent-decode javascript: URLs
	if strings.Contains(rec.Body.String(), "ZgotmplZ") {
		t.Fatal("template sanitized a value (ZgotmplZ)")
	}
	e.expect(e.form("/settings/env", url.Values{"BRAIN_NAME": {"Renomeado"}, "CRON_RSS": {"invalid"}}), 303)
	if e.cfg.Get("BRAIN_NAME") == "Renomeado" {
		t.Fatal("invalid cron should abort save")
	}
	e.expect(e.form("/settings/env", url.Values{"BRAIN_NAME": {"Renomeado"}, "__extra": {"MY_VAR=1"}}), 303)
	if e.cfg.Get("BRAIN_NAME") != "Renomeado" || e.cfg.Get("MY_VAR") != "1" || e.cfg.Get("SESSION_SECRET") == "" {
		t.Fatal("settings not saved")
	}

	rec = e.do("GET", "/export/obsidian", nil, nil)
	e.expect(rec, 200)
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "Notas/Projeto Atlas v2.md" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			found = strings.Contains(string(b), "[[Maria Souza]]") && strings.HasPrefix(string(b), "---\n")
		}
	}
	if !found {
		t.Fatal("obsidian export missing note with wiki-links")
	}
	e.expect(e.form("/nodes/2/delete", url.Values{}), 200, "removido")
	if r := e.do("POST", "/nodes", strings.NewReader("title=x"), map[string]string{"Origin": "https://evil.example", "Content-Type": "application/x-www-form-urlencoded"}); r.Code != 403 {
		t.Fatalf("csrf not blocked: %d", r.Code)
	}
}
