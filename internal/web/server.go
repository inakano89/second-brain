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
	"github.com/inakano89/second-brain/internal/integrations/google"
	"github.com/inakano89/second-brain/internal/integrations/zepp"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/scheduler"
	"github.com/inakano89/second-brain/internal/telegram"
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
}

// Deps are the components the web layer needs.
type Deps struct {
	Cfg      *config.Config
	DB       *database.DB
	LLM      *llm.Manager
	Agent    *agent.Agent
	Google   *google.Client
	Zepp     *zepp.Client
	Telegram *telegram.Service
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

var pageNames = []string{"setup", "login", "graph", "chat", "dashboard", "logs", "settings", "clip"}

// New parses templates and builds the server.
func New(d Deps) (*Server, error) {
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
	mux.HandleFunc("GET /search", s.auth(s.searchPartial))
	mux.HandleFunc("GET /api/search", s.auth(s.apiSearch))
	mux.HandleFunc("POST /nodes", s.auth(s.createNode))
	mux.HandleFunc("GET /nodes/{id}", s.auth(s.nodeDetail))
	mux.HandleFunc("GET /nodes/{id}/edit", s.auth(s.nodeEdit))
	mux.HandleFunc("POST /nodes/{id}", s.auth(s.nodeUpdate))
	mux.HandleFunc("POST /nodes/{id}/delete", s.auth(s.nodeDelete))
	mux.HandleFunc("POST /nodes/{id}/toggle", s.auth(s.nodeToggle))
	mux.HandleFunc("POST /nodes/{id}/enrich", s.auth(s.nodeEnrich))
	mux.HandleFunc("GET /media/{file}", s.auth(s.media))

	mux.HandleFunc("GET /chat", s.auth(s.chatPage))
	mux.HandleFunc("POST /api/chat", s.auth(s.apiChat))
	mux.HandleFunc("POST /chat/reset", s.auth(s.chatReset))

	mux.HandleFunc("GET /dashboard", s.auth(s.dashboardPage))
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
	mux.HandleFunc("POST /backup/now", s.auth(s.backupNow))
	mux.HandleFunc("GET /backups/{file}", s.auth(s.backupDownload))

	mux.HandleFunc("GET /google/connect", s.auth(s.googleConnect))
	mux.HandleFunc("GET /google/callback", s.auth(s.googleCallback))
	mux.HandleFunc("POST /google/disconnect", s.auth(s.googleDisconnect))

	mux.HandleFunc("OPTIONS /api/clip", s.cors(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("POST /api/clip", s.cors(s.tokenOrSession(s.apiClip)))
	mux.HandleFunc("POST /api/health/webhook", s.tokenOrSession(s.apiHealthWebhook))
	mux.HandleFunc("POST /api/nodes", s.tokenOrSession(s.apiCreateNode))

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
		if !s.Cfg.SetupCompleted() && p != "/setup" && !strings.HasPrefix(p, "/static/") && p != "/healthz" {
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
	Flash   string
	Error   string
	Data    any
}

func (s *Server) page(r *http.Request, title, active string, data any) Page {
	p := Page{Title: title, Active: active, Brain: s.Cfg.Get("BRAIN_NAME"), Version: s.Version, Data: data}
	p.User, _ = s.currentUser(r)
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
	target := path + "?" + key + "=" + url.QueryEscape(flash)
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func bg(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
}
