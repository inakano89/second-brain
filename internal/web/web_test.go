package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/integrations/google"
	"github.com/inakano89/second-brain/internal/integrations/zepp"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/telegram"
	"github.com/inakano89/second-brain/internal/updater"
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
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m := llm.NewManager(cfg, nil)
	ag := agent.New(cfg, db, m, log)
	gc := google.New(cfg, db, log)
	ag.SetGoogle(gc)
	cat := llm.NewCatalogSync(cfg, m, db, log)
	cat.URL = func() string { return "" } // offline: remote sync reports an error
	cat.Init(context.Background())
	srv, err := New(Deps{Cfg: cfg, DB: db, LLM: m, Catalog: cat, Agent: ag, Google: gc, Zepp: zepp.New(cfg, db, ag, log), Telegram: telegram.New(cfg, db, ag, log), Updater: updater.New(cfg, db, log, "dev", "", ""), Log: log, Version: "test"})
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
	e.expect(e.do("GET", "/setup", nil, nil), 200, "Primeiro acesso", "/help#telegram")
	e.expect(e.do("GET", "/help", nil, nil), 200, "@BotFather", "Voltar ao setup")
	e.expect(e.form("/setup", url.Values{"username": {"admin"}, "password": {"curta"}, "password2": {"curta"}, "http_port": {"8080"}}), 400, "ao menos 8")
	rec = e.form("/setup", url.Values{
		"brain_name": {"Cérebro Teste"}, "username": {"admin"}, "password": {"senha-forte-1"}, "password2": {"senha-forte-1"},
		"http_port": {"8080"}, "timezone": {"America/Sao_Paulo"}, "telegram_ids": {"123, 456"}, "auto_update": {"true"},
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
	// Overview (orbit view): summary API and drill-down list.
	page := e.do("GET", "/", nil, nil)
	e.expect(page, 200, "orbit.js", `data-view="network"`, "--c: var(--t-note)")
	if strings.Contains(page.Body.String(), "ZgotmplZ") {
		t.Fatal("type colour sanitized in template")
	}
	rec = e.do("GET", "/api/overview", nil, nil)
	e.expect(rec, 200)
	var ov struct {
		Total int `json:"total"`
		Types []struct {
			Key, Color string
			Count      int
		}
		Topics []struct {
			Key   string
			Count int
		}
		Sources []struct {
			Key     string
			Count   int
			Sources []string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil || ov.Total < 2 || len(ov.Types) != len(database.NodeTypes) || len(ov.Sources) == 0 {
		t.Fatalf("overview = %s (%v)", rec.Body.String(), err)
	}
	e.expect(e.do("GET", "/overview/nodes?types=note&label=Notas&count=1", nil, nil), 200, "Notas", "Projeto Atlas", `data-network="types=note"`)
	e.expect(e.do("GET", "/overview/nodes?source=web,api&label=Web&icon=%F0%9F%96%A5", nil, nil), 200, "Maria Souza", "Projeto Atlas")

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
	e.expect(rec, 200, `href="javascript:%28function`, "Editor do .env", "Google Takeout", "People API", "aguardando conexão") // browsers percent-decode javascript: URLs
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

	// Windows-only controls are hidden and refused elsewhere.
	if rec := e.do("GET", "/settings", nil, nil); strings.Contains(rec.Body.String(), `id="desktop"`) {
		t.Fatal("desktop card shown outside Windows")
	}
	for _, p := range []string{"/settings/autostart", "/settings/shutdown"} {
		if r := e.form(p, url.Values{}); r.Code != 303 || !strings.Contains(r.Header().Get("Location"), "error=") {
			t.Fatalf("%s: %d %s", p, r.Code, r.Header().Get("Location"))
		}
	}

	if !e.cfg.GetBool("AUTO_UPDATE_ENABLED") {
		t.Fatal("setup should enable auto-update")
	}
	e.expect(e.do("GET", "/settings", nil, nil), 200, `id="updates"`, "build de desenvolvimento")
	if r := e.form("/settings/updates/toggle", url.Values{}); r.Code != 303 || !strings.Contains(r.Header().Get("Location"), "#updates") || e.cfg.GetBool("AUTO_UPDATE_ENABLED") {
		t.Fatalf("toggle failed: %d %s", r.Code, r.Header().Get("Location"))
	}
	if r := e.form("/settings/updates/install", url.Values{}); r.Code != 303 || !strings.Contains(r.Header().Get("Location"), "error=") {
		t.Fatalf("dev build install should be refused: %s", r.Header().Get("Location"))
	}

	// Models page: catalogue, defaults, routes, council.
	e.expect(e.do("GET", "/models", nil, nil), 200, "Catálogo", "Conselho", "LLM_ROUTE_BRIEFING", "Lista recomendada", "claude-opus-5-5", "US$ 4 / 20", "gpt-6-astra", "US$ 10 / 50", "gemini-3.1-pro-preview", "prompts acima de 200k tokens: US$ 4 / 18", "US$ 0.75 / 3.75")
	if r := e.form("/models/sync", url.Values{}); r.Code != 303 || !strings.Contains(r.Header().Get("Location"), "error=") {
		t.Fatalf("offline sync should report an error: %s", r.Header().Get("Location"))
	}
	e.expect(e.form("/models/add", url.Values{"provider": {"gemini"}, "model": {"gemini-9-ultra"}}), 303)
	if !strings.Contains(e.cfg.Get("LLM_MODELS"), "gemini:gemini-9-ultra") {
		t.Fatal("model not added")
	}
	e.expect(e.form("/models/default", url.Values{"spec": {"gemini:gemini-9-ultra"}}), 303)
	if e.cfg.Get("GEMINI_MODEL") != "gemini-9-ultra" {
		t.Fatal("default not set")
	}
	if r := e.form("/models/remove", url.Values{"spec": {"gemini:gemini-9-ultra"}}); !strings.Contains(r.Header().Get("Location"), "error=") {
		t.Fatal("removing a default model must be refused")
	}
	e.expect(e.form("/models/routes", url.Values{"LLM_ROUTE_ENRICH": {"gemini:gemini-9-ultra"}, "LLM_ROUTE_TRANSCRIPTION": {"anthropic"}}), 303)
	if e.cfg.Get("LLM_ROUTE_ENRICH") == "gemini:gemini-9-ultra" {
		t.Fatal("invalid transcription route should abort the save")
	}
	e.expect(e.form("/models/routes", url.Values{"LLM_ROUTE_ENRICH": {"gemini:gemini-9-ultra"}, "LLM_ROUTE_CHAT": {"council"}}), 303)
	if e.cfg.Get("LLM_ROUTE_ENRICH") != "gemini:gemini-9-ultra" || e.cfg.Get("LLM_ROUTE_CHAT") != "council" {
		t.Fatal("routes not saved")
	}
	e.expect(e.form("/models/council", url.Values{"member": {"anthropic", "gemini:gemini-9-ultra"}, "judge": {"anthropic"}, "rounds": {"2"}}), 303)
	if e.cfg.Get("LLM_COUNCIL_MEMBERS") != "anthropic,gemini:gemini-9-ultra" || e.cfg.Get("LLM_COUNCIL_ROUNDS") != "2" {
		t.Fatal("council not saved")
	}
	e.expect(e.form("/models/default", url.Values{"spec": {"gemini:gemini-3.8-flash"}}), 303)
	e.expect(e.form("/models/remove", url.Values{"spec": {"gemini:gemini-9-ultra"}}), 303)
	if strings.Contains(e.cfg.Get("LLM_MODELS"), "gemini-9-ultra") || e.cfg.Get("LLM_ROUTE_ENRICH") != "gemini" || strings.Contains(e.cfg.Get("LLM_COUNCIL_MEMBERS"), "ultra") {
		t.Fatal("remove did not clean routes/council")
	}
	e.expect(e.do("GET", "/chat", nil, nil), 200, `value="council"`)

	// Telegram pairing: pending user → authorize.
	e.cfg.Update(map[string]string{"TELEGRAM_BOT_TOKEN": "123:fake"})
	e.db.KVSetJSON(ctx, "telegram.pending", []map[string]any{{"id": 777, "username": "fulano", "first_name": "Fulano"}})
	e.expect(e.do("GET", "/settings", nil, nil), 200, "Aguardando autorização", "Fulano")
	e.expect(e.form("/telegram/authorize", url.Values{"id": {"777"}}), 303)
	if !strings.Contains(e.cfg.Get("ALLOWED_TELEGRAM_USER_IDS"), "777") {
		t.Fatal("telegram user not authorized")
	}
	e.expect(e.form("/telegram/revoke", url.Values{"id": {"777"}}), 303)
	if strings.Contains(e.cfg.Get("ALLOWED_TELEGRAM_USER_IDS"), "777") {
		t.Fatal("telegram user not revoked")
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
	e.expect(e.form("/nodes/2/delete", url.Values{}), 200, "lixeira")
	if r := e.do("POST", "/nodes", strings.NewReader("title=x"), map[string]string{"Origin": "https://evil.example", "Content-Type": "application/x-www-form-urlencoded"}); r.Code != 403 {
		t.Fatalf("csrf not blocked: %d", r.Code)
	}
}

func (e *env) completeSetup() {
	e.t.Helper()
	rec := e.form("/setup", url.Values{"brain_name": {"Teste"}, "username": {"admin"}, "password": {"senha-forte-1"}, "password2": {"senha-forte-1"}, "http_port": {"8080"}, "timezone": {"America/Sao_Paulo"}})
	e.expect(rec, 200, "Tudo pronto")
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			e.cookie = c
		}
	}
}

func multipartBody(t *testing.T, fields map[string]string, files map[string]string) (io.Reader, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for name, body := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, body)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestImport(t *testing.T) {
	e := setup(t)
	e.completeSetup()
	token := e.cfg.Get("API_TOKEN")

	e.expect(e.do("GET", "/import", nil, nil), 200, "Importar dados", "Evernote", "Google Keep", "My Clippings.txt", `name="llm"`, `enctype="multipart/form-data"`)
	e.expect(e.do("GET", "/settings", nil, nil), 200, `href="/import"`)
	e.expect(e.do("GET", "/help", nil, nil), 200, `id="import"`, "My Clippings.txt")

	body, ct := multipartBody(t, map[string]string{"format": "auto", "tags": "migracao"}, map[string]string{
		"contatos.vcf": "BEGIN:VCARD\nFN:Ana Lima\nEMAIL:ana@exemplo.com\nEND:VCARD\n",
		"notas.csv":    "titulo;conteudo\nPrimeira;texto um\nSegunda;texto dois\n",
	})
	rec := e.do("POST", "/import", body, map[string]string{"Content-Type": ct})
	loc := rec.Header().Get("Location")
	if rec.Code != 303 || !strings.Contains(loc, "flash=") || !strings.Contains(loc, "#job-") {
		t.Fatalf("upload: %d %s %s", rec.Code, loc, rec.Body.String())
	}
	id := loc[strings.Index(loc, "#job-")+5:]
	var frag string
	for i := 0; i < 200; i++ {
		rec = e.do("GET", "/import/jobs/"+id, nil, nil)
		e.expect(rec, 200)
		if frag = rec.Body.String(); !strings.Contains(frag, "hx-trigger") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(frag, "concluída") || !strings.Contains(frag, "<b>3</b><span>novos</span>") {
		t.Fatalf("job fragment: %s", frag)
	}
	e.expect(e.do("GET", "/import", nil, nil), 200, "Importações recentes", "contatos.vcf")
	e.expect(e.do("GET", "/import/jobs/nope", nil, nil), 404)

	// REST: raw body with ?filename=, token auth, synchronous JSON report.
	anon := *e
	anon.cookie = nil
	rec = anon.do("POST", "/api/import?filename=favoritos.html&tags=web", strings.NewReader(`<!DOCTYPE NETSCAPE-Bookmark-file-1><DL><DT><A HREF="https://go.dev/">Go</A></DL>`),
		map[string]string{"Authorization": "Bearer " + token, "Content-Type": "text/html"})
	e.expect(rec, 200, `"state":"done"`, `"created":1`, `"bookmarks":1`)
	e.expect(anon.do("POST", "/api/import?filename=x.csv", strings.NewReader("a"), map[string]string{"Authorization": "Bearer nope"}), 401)
	e.expect(anon.do("POST", "/api/import", strings.NewReader("a"), map[string]string{"Authorization": "Bearer " + token}), 400, "filename")
	e.expect(anon.do("POST", "/api/import?filename=x.bin", strings.NewReader("\x00\x01"), map[string]string{"Authorization": "Bearer " + token}), 422, "reconhecido")
	body, ct = multipartBody(t, map[string]string{"llm": "true"}, map[string]string{"a.md": "# Nota A\ncorpo"})
	e.expect(anon.do("POST", "/api/import", body, map[string]string{"Authorization": "Bearer " + token, "Content-Type": ct}), 200, `"created":1`)

	// Google Takeout: activity history becomes monthly digests; Keep goes to its own reader.
	var tz bytes.Buffer
	zw := zip.NewWriter(&tz)
	for name, content := range map[string]string{
		"Takeout/YouTube e YouTube Music/histórico/histórico-de-visualização.json": `[{"header":"YouTube","title":"Assistiu a Aula de Go","titleUrl":"https://www.youtube.com/watch?v=abc","subtitles":[{"name":"Canal Dev"}],"time":"2026-08-10T15:04:05Z","products":["YouTube"]}]`,
		"Takeout/YouTube e YouTube Music/histórico/histórico-de-pesquisa.html":     "<html><body>histórico</body></html>",
		"Takeout/Keep/Ideia.json": `{"title":"Ideia","textContent":"Plano de estudos","isTrashed":false,"userEditedTimestampUsec":1700000000000000,"createdTimestampUsec":1700000000000000}`,
	} {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	rec = anon.do("POST", "/api/import?filename=takeout-20260928T000000Z-001.zip", &tz, map[string]string{"Authorization": "Bearer " + token})
	e.expect(rec, 200, `"takeout":1`, `"keep":1`, "escolha JSON")
	if n, err := e.db.GetNodeBySource(context.Background(), "import:takeout", "youtube-watch:2026-08"); err != nil || !strings.Contains(n.Content, "Aula de Go") || n.Title != "YouTube — vídeos assistidos — agosto de 2026" {
		t.Fatalf("takeout digest = %+v, %v", n, err)
	}

	people, _ := e.db.ListNodes(context.Background(), database.NodeFilter{Types: []string{database.TypePerson}, Source: "import:vcard"})
	if len(people) != 1 || people[0].Title != "Ana Lima" || !strings.Contains(strings.Join(people[0].Tags, ","), "migracao") {
		t.Fatalf("imported contact: %+v", people)
	}
	e.cfg.Update(map[string]string{"IMPORT_MAX_MB": "1"})
	body, ct = multipartBody(t, nil, map[string]string{"grande.md": strings.Repeat("x", 3<<20)})
	if r := e.do("POST", "/import", body, map[string]string{"Content-Type": ct}); !strings.Contains(r.Header().Get("Location"), "IMPORT_MAX_MB") {
		t.Fatalf("oversized upload accepted: %d %s", r.Code, r.Header().Get("Location"))
	}
}

func TestContentManager(t *testing.T) {
	e := setup(t)
	e.completeSetup()
	ctx := context.Background()
	token := e.cfg.Get("API_TOKEN")
	anon := *e
	anon.cookie = nil
	var bm strings.Builder
	bm.WriteString(`<!DOCTYPE NETSCAPE-Bookmark-file-1><DL>`)
	for i := range 60 {
		fmt.Fprintf(&bm, `<DT><A HREF="https://site%d.example/">Favorito %02d</A>`, i, i)
	}
	bm.WriteString(`</DL>`)
	rec := anon.do("POST", "/api/import?filename=favoritos.html", strings.NewReader(bm.String()), map[string]string{"Authorization": "Bearer " + token, "Content-Type": "text/html"})
	e.expect(rec, 200, `"created":60`)
	var rep struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &rep)
	note, _, err := e.ag.Ingest(ctx, agent.IngestInput{Title: "Nota pessoal", Content: "texto", Source: "web"})
	if err != nil {
		t.Fatal(err)
	}

	e.expect(e.do("GET", "/content", nil, nil), 200, `href="/content" class="active"`, "Conteúdo", "1–50 de 61", "📥 Importação: Favoritos (60)", `data-special="dup"`, "/static/content.js")
	rows := e.do("GET", "/content/rows?source=import:bookmarks&order=title&offset=50", nil, map[string]string{"HX-Request": "true"})
	e.expect(rows, 200, "51–60 de 60", "Favorito 59", `id="content-rows"`)
	if u := rows.Header().Get("HX-Push-Url"); u != "/content?offset=50&order=title&source=import%3Abookmarks" {
		t.Errorf("push url = %q", u)
	}
	e.expect(e.do("GET", "/content?batch="+rep.ID, nil, nil), 200, "📥 favoritos.html ✕", "1–50 de 60")
	e.expect(e.do("GET", "/content/sends", nil, nil), 200, "favoritos.html", "/content?batch="+rep.ID, "Apagar envio")
	e.expect(e.form("/content/bulk", url.Values{"action": {"trash"}}), 200, "Nenhum item selecionado")

	// Tag, retype and delete a hand-picked selection.
	ids, _ := e.db.NodeIDs(ctx, database.NodeFilter{Batch: rep.ID, Order: "title"}, 3)
	sel := url.Values{"source": {"import:bookmarks"}}
	for _, id := range ids {
		sel.Add("id", fmt.Sprint(id))
	}
	with := func(kv ...string) url.Values {
		v := url.Values{}
		for k, vals := range sel {
			v[k] = append([]string(nil), vals...)
		}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	e.expect(e.form("/content/bulk", with("action", "tag_add", "set_tag", "#Ler Depois")), 200, "Tag #ler-depois adicionada a 3 itens")
	if n, _ := e.db.CountNodes(ctx, database.NodeFilter{Tag: "ler-depois"}); n != 3 {
		t.Fatalf("tagged = %d", n)
	}
	e.expect(e.form("/content/bulk", with("action", "retype", "to_type", "task")), 200, "3 itens movido(s) para Tarefas")
	e.expect(e.form("/content/bulk", with("action", "done")), 200, "3 tarefas atualizadas")
	e.expect(e.form("/content/bulk", with("action", "tag_remove", "set_tag", "ler-depois")), 200, "removida de 3 itens")
	rec = e.form("/content/bulk", with("action", "trash", "forget", "1"))
	e.expect(rec, 200, "3 itens movidos para a lixeira", "Não voltam", "Desfazer", "1–50 de 57")
	between := func(s, start, end string) string {
		t.Helper()
		i := strings.Index(s, start)
		if i < 0 {
			t.Fatalf("%q not found", start)
		}
		s = s[i+len(start):]
		return s[:strings.Index(s, end)]
	}
	undo := between(rec.Body.String(), `{"undo":"`, `"`)
	e.expect(e.form("/content/undo", url.Values{"undo": {undo}, "source": {"import:bookmarks"}}), 200, "3 itens restaurados", "1–50 de 60")
	if n, _ := e.db.DeletedRefCount(ctx); n != 0 {
		t.Fatalf("tombstones after undo = %d", n)
	}

	// Export the selection, then delete every result of a filter.
	rec = e.form("/content/export", with())
	e.expect(rec, 200)
	if zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err != nil || len(zr.File) != 4 {
		t.Fatalf("export = %v, %v", zr, err)
	}
	e.expect(e.form("/content/bulk", url.Values{"action": {"trash"}, "all": {"1"}, "batch": {rep.ID}, "forget": {"1"}}), 200, "60 itens movidos", "Nada encontrado")
	if n, _ := e.db.CountNodes(ctx, database.NodeFilter{}); n != 1 {
		t.Fatalf("left %d nodes", n)
	}

	// Re-importing the same file does not bring them back.
	rec = anon.do("POST", "/api/import?filename=favoritos.html", strings.NewReader(bm.String()), map[string]string{"Authorization": "Bearer " + token, "Content-Type": "text/html"})
	e.expect(rec, 200, `"created":0`, `"deleted":60`)

	// Trash: list, restore a batch, purge, unblock.
	trash := e.do("GET", "/content/trash", nil, nil)
	e.expect(trash, 200, "Lixeira", "60</b> itens", "Restaurar lote", "60 item(ns) apagado(s)", "Permitir que voltem")
	items, _, _ := e.db.ListTrash(ctx, "", "", 1, 0)
	rec = e.form("/content/trash/action", url.Values{"action": {"restore"}, "batch": {items[0].Batch}})
	if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "60+itens+restaurados") {
		t.Fatalf("restore batch: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	e.form("/content/bulk", url.Values{"action": {"trash"}, "id": {fmt.Sprint(note.ID)}})
	rec = e.form("/content/trash/action", url.Values{"action": {"purge"}, "id": {fmt.Sprint(note.ID)}})
	if !strings.Contains(rec.Header().Get("Location"), "1+item+apagado+definitivamente") {
		t.Fatalf("purge: %s", rec.Header().Get("Location"))
	}
	rec = e.form("/content/trash/action", url.Values{"action": {"unblock"}})
	if !strings.Contains(rec.Header().Get("Location"), "flash=") {
		t.Fatalf("unblock: %s", rec.Header().Get("Location"))
	}
	if n, _ := e.db.DeletedRefCount(ctx); n != 0 {
		t.Fatalf("tombstones = %d", n)
	}

	// Whole source from the Envios tab, then empty the trash.
	rec = e.form("/content/sends/trash", url.Values{"source": {"import:bookmarks"}, "forget": {"1"}})
	if !strings.Contains(rec.Header().Get("Location"), "/content/trash?flash=60+itens") {
		t.Fatalf("send trash: %s", rec.Header().Get("Location"))
	}
	e.form("/content/trash/action", url.Values{"action": {"empty"}})
	if n, _ := e.db.TrashCount(ctx); n != 0 {
		t.Fatalf("trash = %d", n)
	}

	// Detail panel delete goes to the trash and can be undone.
	n2, _, _ := e.ag.Ingest(ctx, agent.IngestInput{Title: "Outra", Source: "web"})
	rec = e.form(fmt.Sprintf("/nodes/%d/delete", n2.ID), url.Values{})
	e.expect(rec, 200, "lixeira", "Desfazer")
	batch := between(rec.Body.String(), `{"batch":"`, `"`)
	e.expect(e.form("/nodes/restore", url.Values{"batch": {batch}, "id": {fmt.Sprint(n2.ID)}}), 200, "Restaurado", "Outra")
}

func TestCleanupTabAndDashboard(t *testing.T) {
	e := setup(t)
	e.completeSetup()
	ctx := context.Background()
	long := time.Now().AddDate(0, 0, -40)
	mk := func(in agent.IngestInput) *database.Node {
		t.Helper()
		n, _, err := e.ag.Ingest(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	a := mk(agent.IngestInput{Type: database.TypeArticle, Title: "Go Docs", Content: "https://go.dev", Source: "import:bookmarks", SourceRef: "1", CreatedAt: long})
	b := mk(agent.IngestInput{Type: database.TypeArticle, Title: "Go Docs", Content: "https://go.dev", Source: "import:bookmarks", SourceRef: "2"})
	stale := mk(agent.IngestInput{Type: database.TypeTask, Title: "Tarefa esquecida", Source: "web", CreatedAt: long})
	e.db.ExecContext(ctx, `UPDATE nodes SET updated_at = created_at WHERE id = ?`, stale.ID)

	e.expect(e.do("GET", "/content/cleanup", nil, nil), 200, "Faxina semanal", "Nada para limpar agora")
	rec := e.form("/content/cleanup/run", url.Values{})
	if !strings.Contains(rec.Header().Get("Location"), "2+sugest") {
		t.Fatalf("run: %s", rec.Header().Get("Location"))
	}
	page := e.do("GET", "/content/cleanup", nil, nil)
	e.expect(page, 200, "Duplicados", "Tarefas paradas", "🔗 Juntar", "✓ Concluir", `<span class="count">2</span>`, "Go Docs", ">fica<")
	e.expect(e.do("GET", "/content", nil, nil), 200, `href="/content/cleanup"`, `<span class="count">2</span>`)
	list, _ := e.db.ListCleanup(ctx, database.CleanupPending, 10)
	var dup, task database.CleanupSuggestion
	for _, c := range list {
		switch c.Kind {
		case database.CleanupDuplicate:
			dup = c
		case database.CleanupStaleTask:
			task = c
		}
	}
	rec = e.do("POST", fmt.Sprintf("/content/cleanup/%d", dup.ID), strings.NewReader("action=merge"),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true"})
	e.expect(rec, 200, "juntados", fmt.Sprintf(`id="cl-%d"`, dup.ID))
	if _, err := e.db.GetNode(ctx, a.ID); err != database.ErrNotFound { // the newest copy (b) is the one kept
		t.Fatal("copy still there")
	}
	rec = e.form("/content/cleanup/apply-all", url.Values{"kind": {database.CleanupStaleTask}})
	if !strings.Contains(rec.Header().Get("Location"), "1+sugest") {
		t.Fatalf("apply all: %s", rec.Header().Get("Location"))
	}
	if n, _ := e.db.GetNode(ctx, stale.ID); n.Status != database.StatusDone {
		t.Fatalf("task = %+v", n)
	}
	_ = task
	_ = b

	// Dashboard: KPIs, to-do with AI tasks, memory, charts with a table view.
	due := time.Now().AddDate(0, 0, -2)
	late := mk(agent.IngestInput{Type: database.TypeTask, Title: "Pagar boleto", DueAt: &due, Source: "web"})
	meeting := mk(agent.IngestInput{Title: "Reunião de kickoff", Content: "notas", Source: "web"})
	aiTask := mk(agent.IngestInput{Type: database.TypeTask, Title: "Enviar cronograma", Source: "agent", SourceRef: "meeting:1:x"})
	e.db.AddEdge(ctx, aiTask.ID, meeting.ID, "derived_from", 1)
	mk(agent.IngestInput{Type: database.TypeInsight, Title: "Prioridades atuais", Content: "1. **Lançar a v0.5**", Source: agent.MemorySource, SourceRef: "priorities"})
	mk(agent.IngestInput{Type: database.TypeInsight, Title: "Usar SQLite", Content: "x", Source: agent.MemorySource, SourceRef: "decision:1", Tags: []string{agent.TagMemory, agent.TagDecision}})
	dash := e.do("GET", "/dashboard", nil, nil)
	e.expect(dash, 200, "Capturas (7 dias)", "Tarefas abertas", "⚠ 1 atrasada(s)", "Novas da IA (24 h)", "Faxina", "Custo de IA (7 dias)",
		"Pagar boleto", "⚠ atrasada", "Enviar cronograma", "← Reunião de kickoff", "Lançar a v0.5", "Usar SQLite", "✔ decisão",
		"Capturas por dia", "Últimos 7 dias", "Ver em tabela", "/static/dashboard.js", "Consumo de LLM", "Integrações")
	rec = e.do("POST", fmt.Sprintf("/dashboard/tasks/%d/done", late.ID), nil, map[string]string{"HX-Request": "true"})
	e.expect(rec, 200, "Concluída", "Pagar boleto")
	rec = e.do("POST", fmt.Sprintf("/dashboard/tasks/%d/discard", aiTask.ID), nil, map[string]string{"HX-Request": "true"})
	e.expect(rec, 200, "Descartada")
	if gone, _ := e.db.IsDeletedRef(ctx, "agent", "meeting:1:x"); !gone {
		t.Error("discarded AI task must not come back")
	}
	e.expect(e.do("POST", fmt.Sprintf("/dashboard/tasks/%d/nope", meeting.ID), nil, nil), 404)
}

func TestProfilePages(t *testing.T) {
	e := setup(t)
	e.completeSetup()
	ctx := context.Background()

	e.expect(e.do("GET", "/profile", nil, nil), 200, "Visão geral", "Nada agendado para hoje", "Privacidade")
	rec := e.form("/profile/items", url.Values{"kind": {"medication"}, "title": {"Losartana"}, "dose": {"37 mg"}, "times": {"8h"}, "stock": {"30"}, "sensitive": {"on"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "tab=saude") {
		t.Fatalf("create: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = e.form("/profile/items", url.Values{"kind": {"enrollment"}, "title": {"Academia Forte"}, "weekdays": {"seg", "qua"}, "time": {"18:00"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create enrollment: %d", rec.Code)
	}
	items, err := e.ag.Profile().List(ctx, false)
	if err != nil || len(items) != 2 {
		t.Fatalf("items: %+v %v", items, err)
	}
	var med, gym int64
	for _, it := range items {
		if it.Kind == "medication" {
			med = it.ID
		} else {
			gym = it.ID
		}
	}

	// Sensitive details stay out of the list until revealed; the reveal is audited.
	rec = e.do("GET", "/profile?tab=saude", nil, nil)
	e.expect(rec, 200, "Losartana", "🔒 sensível", "👁 Mostrar")
	if strings.Contains(rec.Body.String(), "37 mg") {
		t.Fatal("sensitive detail rendered before reveal")
	}
	e.expect(e.do("GET", fmt.Sprintf("/profile/items/%d", med), nil, nil), 200, "37 mg", "08:00")
	e.expect(e.do("GET", "/profile?tab=rotina", nil, nil), 200, "Academia Forte", "seg, qua")

	// Edit, check a dose (stock goes down), then delete.
	e.expect(e.form(fmt.Sprintf("/profile/items/%d", gym), url.Values{"title": {"Academia Forte"}, "time": {"07:00"}}), 303)
	day := time.Now().In(e.cfg.Location()).Format("2006-01-02")
	e.expect(e.form("/profile/check", url.Values{"id": {fmt.Sprint(med)}, "day": {day}, "slot": {"08:00"}}), 303)
	if it, _ := e.ag.Profile().Get(ctx, med); it.Get("stock") != "29" {
		t.Fatalf("stock after check: %s", it.Get("stock"))
	}
	e.expect(e.do("GET", "/profile", nil, nil), 200, "Losartana", "✔️ feito")
	e.expect(e.form(fmt.Sprintf("/profile/items/%d/delete", gym), url.Values{}), 303)
	if items, _ := e.ag.Profile().List(ctx, false); len(items) != 1 {
		t.Fatalf("delete: %d items", len(items))
	}
	e.expect(e.form("/profile/items", url.Values{"kind": {"bill"}, "title": {"Luz"}, "due_day": {"45"}}), 303)
	e.expect(e.do("GET", "/profile/suggestions", nil, nil), 200)
}

func TestContentDatesGroupsAndOrigin(t *testing.T) {
	e := setup(t)
	e.completeSetup()
	ctx := context.Background()
	mk := func(in agent.IngestInput) *database.Node {
		t.Helper()
		n, _, err := e.ag.Ingest(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	d := func(y int, m time.Month) time.Time { return time.Date(y, m, 10, 12, 0, 0, 0, time.UTC) }
	stamp := map[string]any{database.MetaImportAt: "2026-09-01T10:00:00.000000Z"}
	mk(agent.IngestInput{Title: "Nota antiga importada", Content: "a", Source: "import:markdown", SourceRef: "a", CreatedAt: d(2015, time.March), Meta: stamp})
	mk(agent.IngestInput{Title: "Item sem data", Content: "b", Source: "import:opml", SourceRef: "b", DateUnknown: true, Meta: map[string]any{database.MetaImportAt: "2026-09-01T10:00:00.000000Z"}})
	mk(agent.IngestInput{Title: "Nota minha", Content: "c", Source: "web", CreatedAt: d(2026, time.September)})
	ev := d(2031, time.January)
	mk(agent.IngestInput{Type: database.TypeEvent, Title: "Viagem futura", Content: "d", Source: "calendar", SourceRef: "e", CreatedAt: d(2026, time.August), DueAt: &ev})

	page := e.do("GET", "/content", nil, nil)
	e.expect(page, 200, `name="origin"`, "Por data (eventos: quando acontecem)", `class="month-row"`, "2031 · janeiro", "2026 · setembro", "2015 · março", "Sem data",
		"sem data", "importado em 01/09/2026")
	body := page.Body.String()
	pos := func(s string) int { return strings.Index(body, s) }
	if !(pos("Viagem futura") < pos("Nota minha") && pos("Nota minha") < pos("Nota antiga importada") && pos("Nota antiga importada") < pos("Item sem data")) {
		t.Fatalf("order wrong: event by its date first, undated last: %s", truncate(body))
	}
	created := e.do("GET", "/content/rows?order=created", nil, map[string]string{"HX-Request": "true"}).Body.String()
	if strings.Contains(created, "2031 · janeiro") || !strings.Contains(created, "2026 · agosto") {
		t.Fatalf("order=created must group by creation date: %s", truncate(created))
	}
	imp := e.do("GET", "/content/rows?origin=imported", nil, map[string]string{"HX-Request": "true"})
	e.expect(imp, 200, "Nota antiga importada", "Item sem data", "1–2 de 2")
	mine := e.do("GET", "/content/rows?origin=mine", nil, map[string]string{"HX-Request": "true"})
	e.expect(mine, 200, "Nota minha", "1–1 de 1")
	if u := mine.Header().Get("HX-Push-Url"); u != "/content?origin=mine" {
		t.Errorf("push url = %q", u)
	}
	e.expect(e.do("GET", "/content/rows?origin=auto", nil, map[string]string{"HX-Request": "true"}), 200, "Viagem futura", "1–1 de 1")
}
