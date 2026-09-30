// Package web serves the HTMX interface, onboarding wizard, SSE chat,
// graph API, dashboards and REST endpoints (clipper, health webhook).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/importer"
	"github.com/inakano89/second-brain/internal/integrations/google"
	"github.com/inakano89/second-brain/internal/integrations/zepp"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/scheduler"
	"github.com/inakano89/second-brain/internal/telegram"
	"github.com/inakano89/second-brain/internal/updater"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Hooks connect the web layer to the runtime supervisor.
type Hooks struct {
	Reload func()
	Rebind func(addr string)
	Jobs   func() []scheduler.JobInfo
	RunJob func(name string) error
	// Shutdown stops the server gracefully (desktop installs without a console window).
	Shutdown func()
}

// Deps are the components the web layer needs.
type Deps struct {
	Cfg      *config.Config
	DB       *database.DB
	LLM      *llm.Manager
	Catalog  *llm.CatalogSync
	Agent    *agent.Agent
	Google   *google.Client
	Zepp     *zepp.Client
	Telegram *telegram.Service
	Updater  *updater.Updater
	Importer *importer.Importer // created by New when nil
	Hooks    Hooks
	Log      *slog.Logger
	Version  string
}

// Server is the HTTP application.
type Server struct {
	Deps
	log     *slog.Logger
	pages   map[string]*template.Template
	frags   *template.Template
	limiter *loginLimiter
	static  http.Handler
}

var pageNames = []string{"setup", "login", "graph", "chat", "dashboard", "logs", "settings", "clip", "models", "help", "import", "content", "profile", "financas"}

// New parses templates and builds the server.
func New(d Deps) (*Server, error) {
	if d.Importer == nil {
		d.Importer = importer.New(d.Cfg, d.Agent, d.Log)
	}
	s := &Server{Deps: d, log: d.Log.With("component", "web"), pages: map[string]*template.Template{}, limiter: newLoginLimiter()}
	funcs := s.funcs()
	for _, p := range pageNames {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", p, err)
		}
		s.pages[p] = t
	}
	frags, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/partials.html")
	if err != nil {
		return nil, err
	}
	s.frags = frags
	sub, _ := fs.Sub(staticFS, "static")
	s.static = http.StripPrefix("/static/", cacheStatic(http.FileServerFS(sub)))
	return s, nil
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// Handler returns the root handler with middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setupSubmit)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /{$}", s.auth(s.graphPage))
	mux.HandleFunc("GET /api/graph", s.auth(s.apiGraph))
	mux.HandleFunc("GET /api/overview", s.auth(s.apiOverview))
	mux.HandleFunc("GET /overview/nodes", s.auth(s.overviewNodes))
	mux.HandleFunc("GET /search", s.auth(s.searchPartial))
	mux.HandleFunc("GET /api/search", s.auth(s.apiSearch))
	mux.HandleFunc("POST /nodes", s.auth(s.createNode))
	mux.HandleFunc("GET /nodes/{id}", s.auth(s.nodeDetail))
	mux.HandleFunc("GET /nodes/{id}/edit", s.auth(s.nodeEdit))
	mux.HandleFunc("POST /nodes/{id}", s.auth(s.nodeUpdate))
	mux.HandleFunc("POST /nodes/{id}/delete", s.auth(s.nodeDelete))
	mux.HandleFunc("POST /nodes/restore", s.auth(s.nodeRestore))
	mux.HandleFunc("POST /nodes/{id}/toggle", s.auth(s.nodeToggle))
	mux.HandleFunc("POST /nodes/{id}/enrich", s.auth(s.nodeEnrich))
	mux.HandleFunc("GET /media/{file}", s.auth(s.media))

	mux.HandleFunc("GET /chat", s.auth(s.chatPage))
	mux.HandleFunc("POST /api/chat", s.auth(s.apiChat))
	mux.HandleFunc("POST /chat/reset", s.auth(s.chatReset))

	mux.HandleFunc("GET /models", s.auth(s.modelsPage))
	mux.HandleFunc("POST /models/add", s.auth(s.modelAdd))
	mux.HandleFunc("POST /models/remove", s.auth(s.modelRemove))
	mux.HandleFunc("POST /models/default", s.auth(s.modelDefault))
	mux.HandleFunc("POST /models/routes", s.auth(s.modelRoutes))
	mux.HandleFunc("POST /models/council", s.auth(s.modelCouncil))
	mux.HandleFunc("POST /models/test", s.auth(s.modelTest))
	mux.HandleFunc("GET /models/list", s.auth(s.modelList))
	mux.HandleFunc("POST /models/sync", s.auth(s.modelSync))
	mux.HandleFunc("GET /help", s.helpPage)
	mux.HandleFunc("POST /telegram/authorize", s.auth(s.telegramAuthorize))
	mux.HandleFunc("POST /telegram/revoke", s.auth(s.telegramRevoke))
	mux.HandleFunc("POST /telegram/check", s.auth(s.telegramCheck))

	mux.HandleFunc("GET /profile", s.auth(s.profilePage))
	mux.HandleFunc("GET /profile/suggestions", s.auth(s.profileSuggestions))
	mux.HandleFunc("POST /profile/items", s.auth(s.profileSave))
	mux.HandleFunc("GET /profile/items/{id}", s.auth(s.profileItem))
	mux.HandleFunc("POST /profile/items/{id}", s.auth(s.profileSave))
	mux.HandleFunc("POST /profile/items/{id}/delete", s.auth(s.profileDelete))
	mux.HandleFunc("POST /profile/check", s.auth(s.profileCheck))
	mux.HandleFunc("POST /profile/import", s.auth(s.profileImport))
	mux.HandleFunc("POST /profile/dismiss", s.auth(s.profileDismiss))

	mux.HandleFunc("GET /financas", s.auth(s.financePage))
	mux.HandleFunc("POST /financas/recategorize", s.auth(s.financeRecategorize))
	mux.HandleFunc("POST /financas/rules", s.auth(s.financeRules))
	mux.HandleFunc("POST /financas/clear", s.auth(s.financeClear))
	mux.HandleFunc("GET /dashboard", s.auth(s.dashboardPage))
	mux.HandleFunc("POST /dashboard/tasks/{id}/{action}", s.auth(s.dashboardTask))
	mux.HandleFunc("POST /jobs/{name}/run", s.auth(s.runJob))
	mux.HandleFunc("POST /queue/retry", s.auth(s.queueRetry))
	mux.HandleFunc("GET /logs", s.auth(s.logsPage))
	mux.HandleFunc("GET /logs/rows", s.auth(s.logRows))

	mux.HandleFunc("GET /settings", s.auth(s.settingsPage))
	mux.HandleFunc("POST /settings/env", s.auth(s.settingsEnv))
	mux.HandleFunc("POST /settings/password", s.auth(s.settingsPassword))
	mux.HandleFunc("POST /settings/actions", s.auth(s.settingsActions))
	mux.HandleFunc("POST /settings/telegram-test", s.auth(s.telegramTest))
	mux.HandleFunc("POST /settings/api-token", s.auth(s.rotateToken))
	mux.HandleFunc("GET /export/obsidian", s.auth(s.exportObsidian))
	mux.HandleFunc("GET /export/garden", s.auth(s.exportGarden))
	mux.HandleFunc("GET /content/garden", s.auth(s.contentGarden))
	mux.HandleFunc("POST /content/garden/publish", s.auth(s.contentGardenPublish))
	mux.HandleFunc("GET /content", s.auth(s.contentPage))
	mux.HandleFunc("GET /content/rows", s.auth(s.contentRowsPartial))
	mux.HandleFunc("POST /content/bulk", s.auth(s.contentBulk))
	mux.HandleFunc("POST /content/undo", s.auth(s.contentUndo))
	mux.HandleFunc("POST /content/export", s.auth(s.contentExport))
	mux.HandleFunc("GET /content/sends", s.auth(s.contentSends))
	mux.HandleFunc("POST /content/sends/trash", s.auth(s.contentSendTrash))
	mux.HandleFunc("GET /content/cleanup", s.auth(s.contentCleanup))
	mux.HandleFunc("POST /content/cleanup/run", s.auth(s.contentCleanupRun))
	mux.HandleFunc("POST /content/cleanup/apply-all", s.auth(s.contentCleanupApplyAll))
	mux.HandleFunc("POST /content/cleanup/{id}", s.auth(s.contentCleanupApply))
	mux.HandleFunc("GET /content/trash", s.auth(s.contentTrash))
	mux.HandleFunc("POST /content/trash/action", s.auth(s.contentTrashAction))
	mux.HandleFunc("GET /import", s.auth(s.importPage))
	mux.HandleFunc("POST /import", s.auth(s.importUpload))
	mux.HandleFunc("GET /import/jobs/{id}", s.auth(s.importJob))
	mux.HandleFunc("POST /backup/now", s.auth(s.backupNow))
	mux.HandleFunc("GET /backups/{file}", s.auth(s.backupDownload))

	mux.HandleFunc("POST /settings/autostart", s.auth(s.autostartToggle))
	mux.HandleFunc("POST /settings/shutdown", s.auth(s.shutdown))
	mux.HandleFunc("POST /settings/updates/toggle", s.auth(s.updateToggle))
	mux.HandleFunc("POST /settings/updates/check", s.auth(s.updateCheck))
	mux.HandleFunc("POST /settings/updates/install", s.auth(s.updateInstall))

	mux.HandleFunc("GET /google/connect", s.auth(s.googleConnect))
	mux.HandleFunc("GET /google/callback", s.auth(s.googleCallback))
	mux.HandleFunc("POST /google/disconnect", s.auth(s.googleDisconnect))
	mux.HandleFunc("POST /google/sync", s.auth(s.googleSync))

	mux.HandleFunc("OPTIONS /api/clip", s.cors(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("POST /api/clip", s.cors(s.tokenOrSession(s.apiClip)))
	mux.HandleFunc("POST /api/health/webhook", s.tokenOrSession(s.apiHealthWebhook))
	mux.HandleFunc("POST /api/nodes", s.tokenOrSession(s.apiCreateNode))
	mux.HandleFunc("POST /api/import", s.tokenOrSession(s.apiImport))

	return s.recoverer(s.securityHeaders(s.setupGate(s.csrf(mux))))
}

// ---- middleware ----

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic em handler HTTP", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				http.Error(w, "erro interno", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; form-action 'self' https://accounts.google.com")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !s.Cfg.SetupCompleted() && p != "/setup" && p != "/help" && !strings.HasPrefix(p, "/static/") && p != "/healthz" {
			if strings.HasPrefix(p, "/api/") {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "setup pendente"})
				return
			}
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf rejects cross-origin state-changing requests authenticated by cookie.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path != "/api/clip" && r.URL.Path != "/api/health/webhook" && r.URL.Path != "/api/nodes" {
			if o := r.Header.Get("Origin"); o != "" && o != "null" {
				if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "origem inválida", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		next(w, r)
	}
}

// ---- rendering ----

// Page is the template envelope.
type Page struct {
	Title   string
	Active  string
	Brain   string
	User    string
	Version string
	Update  string // newer release tag, when available
	Flash   string
	Error   string
	Data    any
}

func (s *Server) page(r *http.Request, title, active string, data any) Page {
	p := Page{Title: title, Active: active, Brain: s.Cfg.Get("BRAIN_NAME"), Version: s.Version, Data: data}
	p.User, _ = s.currentUser(r)
	if s.Updater != nil && p.User != "" {
		if st := s.Updater.Status(); st.Available {
			p.Update = st.Latest
		}
	}
	if f := r.URL.Query().Get("flash"); f != "" {
		p.Flash = f
	}
	if e := r.URL.Query().Get("error"); e != "" {
		p.Error = e
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, name string, p Page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "template não encontrado", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", p); err != nil {
		s.log.Error("erro ao renderizar", "page", name, "err", err)
	}
}

func (s *Server) fragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.frags.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("erro ao renderizar fragmento", "name", name, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func redirectFlash(w http.ResponseWriter, r *http.Request, path, flash string, isErr bool) {
	key := "flash"
	if isErr {
		key = "error"
	}
	base, frag, _ := strings.Cut(path, "#")
	target := base + "?" + key + "=" + url.QueryEscape(flash)
	if frag != "" {
		target += "#" + frag
	}
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func bg(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
}
