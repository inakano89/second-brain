package web

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/crypto"
)

const sessionCookie = "sb_session"
const sessionTTL = 30 * 24 * time.Hour

func (s *Server) secure(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.Cfg.Get("PUBLIC_URL"), "https://")
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, user string) {
	exp := time.Now().Add(sessionTTL).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(user + "|" + strconv.FormatInt(exp, 10)))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: payload + "." + crypto.Sign(s.Cfg.Get("SESSION_SECRET"), payload),
		Path: "/", HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode, Expires: time.Unix(exp, 0),
	})
}

func (s *Server) currentUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	secret := s.Cfg.Get("SESSION_SECRET")
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || secret == "" || !crypto.Equal(sig, crypto.Sign(secret, payload)) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	user, expStr, ok := strings.Cut(string(raw), "|")
	exp, _ := strconv.ParseInt(expStr, 10, 64)
	if !ok || time.Now().Unix() > exp || user != s.Cfg.Get("ADMIN_USER") {
		return "", false
	}
	return user, true
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); ok {
			next(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "não autenticado"})
			return
		}
		target := "/login?next=" + r.URL.RequestURI()
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", target)
			return
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

func (s *Server) validToken(r *http.Request) bool {
	want := s.Cfg.Get("API_TOKEN")
	if want == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	if got == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		got = r.PostFormValue("token")
	}
	return got != "" && crypto.Equal(got, want)
}

func (s *Server) tokenOrSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.validToken(r) {
			next(w, r)
			return
		}
		if _, ok := s.currentUser(r); ok {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token inválido"})
	}
}

// ---- login rate limiting ----

type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string][]time.Time{}} }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-10 * time.Minute)
	var recent []time.Time
	for _, t := range l.fails[ip] {
		if t.After(cut) {
			recent = append(recent, t)
		}
	}
	l.fails[ip] = recent
	return len(recent) >= 5
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	l.fails[ip] = append(l.fails[ip], time.Now())
	l.mu.Unlock()
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

func safeNext(n string) string {
	if n == "" || !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.HasPrefix(n, "/\\") {
		return "/"
	}
	return n
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", s.page(r, "Entrar", "", map[string]string{"Next": safeNext(r.URL.Query().Get("next"))}))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	next := safeNext(r.FormValue("next"))
	p := s.page(r, "Entrar", "", map[string]string{"Next": next})
	if s.limiter.blocked(ip) {
		p.Error = "Muitas tentativas. Aguarde 10 minutos."
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login", p)
		return
	}
	user := strings.TrimSpace(r.FormValue("username"))
	okUser := crypto.Equal(user, s.Cfg.Get("ADMIN_USER"))
	okPass := crypto.VerifyPassword(r.FormValue("password"), s.Cfg.Get("ADMIN_PASSWORD_HASH"))
	if !okUser || !okPass {
		s.limiter.fail(ip)
		s.log.Warn("falha de login", "ip", ip, "user", user)
		p.Error = "Usuário ou senha inválidos."
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", p)
		return
	}
	s.limiter.reset(ip)
	s.setSession(w, r, user)
	s.log.Info("login realizado", "ip", ip)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func requireLen(v string, n int, field string) error {
	if len([]rune(v)) < n {
		return fmt.Errorf("%s deve ter ao menos %d caracteres", field, n)
	}
	return nil
}
