package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"
)

// helpAnchors maps settings groups to guide sections.
var helpAnchors = map[string]string{
	"LLM": "llm", "Telegram": "telegram", "Google": "google", "Zepp": "zepp", "RSS": "rss",
	"Backup": "backup", "Automação": "actions", "Atualizações": "updates", "Agendamentos": "schedules", "Geral": "install",
}

type helpView struct {
	PublicURL   string
	RedirectURL string
	LoggedIn    bool
	SetupDone   bool
	Port        int
}

// helpPage renders the step-by-step guides (public: contains no secrets).
func (s *Server) helpPage(w http.ResponseWriter, r *http.Request) {
	_, logged := s.currentUser(r)
	v := helpView{PublicURL: s.Cfg.PublicURL(), RedirectURL: s.Cfg.PublicURL() + "/google/callback", LoggedIn: logged,
		SetupDone: s.Cfg.SetupCompleted(), Port: s.Cfg.GetInt("HTTP_PORT", 8080)}
	s.render(w, "help", s.page(r, "Guia de configuração", "help", v))
}

func (s *Server) telegramAuthorize(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		redirectFlash(w, r, "/settings#telegram", "ID inválido", true)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.Telegram.Authorize(ctx, id); err != nil {
		redirectFlash(w, r, "/settings#telegram", err.Error(), true)
		return
	}
	redirectFlash(w, r, "/settings#telegram", fmt.Sprintf("Usuário %d autorizado.", id), false)
}

func (s *Server) telegramRevoke(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err := s.Telegram.Revoke(id); err != nil {
		redirectFlash(w, r, "/settings#telegram", err.Error(), true)
		return
	}
	s.log.Info("usuário do Telegram removido", "user_id", id)
	redirectFlash(w, r, "/settings#telegram", fmt.Sprintf("Usuário %d removido.", id), false)
}

func (s *Server) telegramCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	u, err := s.Telegram.Check(ctx)
	if err != nil {
		fmt.Fprintf(w, `<span class="pill err">❌ %s</span>`, html.EscapeString(err.Error()))
		return
	}
	name := html.EscapeString(u.Username)
	fmt.Fprintf(w, `<span class="pill ok">✅ Bot @%s ativo</span> — abra <a href="https://t.me/%s" target="_blank" rel="noopener">t.me/%s</a>, envie <code>/start</code> e recarregue esta página para autorizar.`, name, name, name)
}
