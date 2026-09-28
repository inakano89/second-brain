package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
)

type setupData struct {
	Values    map[string]string
	Timezones []string
	Done      bool
	AppURL    string
	APIToken  string
	BackupKey string
	Generated bool
}

var commonTZ = []string{
	"America/Sao_Paulo", "America/Manaus", "America/Fortaleza", "America/Recife", "America/Belem", "America/Cuiaba",
	"America/Noronha", "America/Rio_Branco", "Europe/Lisbon", "UTC", "America/New_York", "America/Los_Angeles", "Europe/London", "Europe/Berlin", "Asia/Tokyo",
}

// setupFields maps form names to .env keys.
var setupFields = map[string]string{
	"brain_name": "BRAIN_NAME", "username": "ADMIN_USER", "http_port": "HTTP_PORT", "timezone": "TIMEZONE", "public_url": "PUBLIC_URL",
	"default_provider": "DEFAULT_LLM_PROVIDER", "openai_key": "OPENAI_API_KEY", "anthropic_key": "ANTHROPIC_API_KEY",
	"gemini_key": "GEMINI_API_KEY", "ollama_url": "OLLAMA_BASE_URL", "ollama_model": "OLLAMA_MODEL",
	"telegram_token": "TELEGRAM_BOT_TOKEN", "telegram_ids": "ALLOWED_TELEGRAM_USER_IDS",
	"google_client_id": "GOOGLE_CLIENT_ID", "google_client_secret": "GOOGLE_CLIENT_SECRET",
	"zepp_email": "ZEPP_EMAIL", "zepp_password": "ZEPP_PASSWORD", "zepp_app_token": "ZEPP_APP_TOKEN", "zepp_user_id": "ZEPP_USER_ID",
	"backup_key": "BACKUP_ENCRYPTION_KEY",
}

func (s *Server) setupValues() map[string]string {
	v := map[string]string{}
	for form, key := range setupFields {
		if f, ok := config.Lookup(key); ok && f.Secret {
			continue // never echo secrets
		}
		v[form] = s.Cfg.Get(key)
	}
	v["auto_update"] = s.Cfg.Get("AUTO_UPDATE_ENABLED")
	return v
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.Cfg.SetupCompleted() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "setup", s.page(r, "Configuração inicial", "", setupData{Values: s.setupValues(), Timezones: commonTZ}))
}

func randomKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if s.Cfg.SetupCompleted() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	data := setupData{Values: map[string]string{}, Timezones: commonTZ}
	for form := range setupFields {
		data.Values[form] = strings.TrimSpace(r.PostFormValue(form))
	}
	data.Values["auto_update"] = strconv.FormatBool(r.PostFormValue("auto_update") == "true")
	fail := func(err error) {
		p := s.page(r, "Configuração inicial", "", data)
		p.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "setup", p)
	}
	v := data.Values
	pass := r.PostFormValue("password")
	if v["username"] == "" {
		fail(errors.New("informe o usuário"))
		return
	}
	if err := requireLen(pass, 8, "a senha"); err != nil {
		fail(err)
		return
	}
	if pass != r.PostFormValue("password2") {
		fail(errors.New("as senhas não conferem"))
		return
	}
	port, err := strconv.Atoi(v["http_port"])
	if err != nil || port < 1 || port > 65535 {
		fail(errors.New("porta HTTP inválida"))
		return
	}
	if v["timezone"] == "" {
		v["timezone"] = "America/Sao_Paulo"
	}
	if _, err := time.LoadLocation(v["timezone"]); err != nil {
		fail(fmt.Errorf("timezone inválida: %s", v["timezone"]))
		return
	}
	if v["brain_name"] == "" {
		v["brain_name"] = "Second Brain"
	}
	for _, id := range strings.Split(v["telegram_ids"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			if _, err := strconv.ParseInt(id, 10, 64); err != nil {
				fail(fmt.Errorf("ID do Telegram inválido: %s", id))
				return
			}
		}
	}
	hash, err := crypto.HashPassword(pass)
	if err != nil {
		fail(err)
		return
	}
	generated := false
	if v["backup_key"] == "" {
		v["backup_key"] = randomKey()
		generated = true
	}
	changes := map[string]string{}
	for form, key := range setupFields {
		changes[key] = v[form]
	}
	changes["AUTO_UPDATE_ENABLED"] = strconv.FormatBool(r.PostFormValue("auto_update") == "true")
	if changes["DEFAULT_LLM_PROVIDER"] == "" {
		changes["DEFAULT_LLM_PROVIDER"] = "auto"
	}
	token := crypto.RandomToken(24)
	changes["ADMIN_PASSWORD_HASH"] = hash
	changes["SESSION_SECRET"] = crypto.RandomToken(32)
	changes["API_TOKEN"] = token
	changes["SETUP_COMPLETED"] = "true"
	oldAddr := s.Cfg.Addr()
	if err := s.Cfg.Update(changes); err != nil {
		fail(fmt.Errorf("falha ao gravar .env: %w", err))
		return
	}
	s.log.Info("setup concluído", "user", v["username"], "env", s.Cfg.Path())
	s.setSession(w, r, v["username"])

	appURL := "/"
	if newAddr := s.Cfg.Addr(); newAddr != oldAddr {
		host, _, _ := net.SplitHostPort(r.Host)
		if host == "" {
			host = r.Host
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		appURL = fmt.Sprintf("%s://%s/", scheme, net.JoinHostPort(host, strconv.Itoa(port)))
		if s.Hooks.Rebind != nil {
			go func() {
				time.Sleep(2 * time.Second) // let this response flush first
				s.Hooks.Rebind(newAddr)
			}()
		}
	}
	if s.Hooks.Reload != nil {
		go s.Hooks.Reload()
	}
	data.Done, data.AppURL, data.APIToken, data.Generated = true, appURL, token, generated
	if generated {
		data.BackupKey = v["backup_key"]
	}
	p := s.page(r, "Setup concluído", "", data)
	p.User = v["username"]
	s.render(w, "setup", p)
}
