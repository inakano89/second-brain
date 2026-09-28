package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/llm"
)

type option struct {
	Value    string
	Label    string
	Disabled bool
}

type providerView struct {
	Name        string
	Label       string
	Configured  bool
	Default     string
	Models      []llm.ModelEntry
	Suggestions []string
	KeyHelp     string
}

type taskView struct {
	llm.TaskInfo
	Value   string
	Options []option
}

type modelsView struct {
	Providers     []providerView
	Tasks         []taskView
	GlobalDefault string
	GlobalOptions []option
	Members       map[string]bool
	MemberOpts    []option
	Judge         string
	JudgeOpts     []option
	Rounds        int
	Effective     llm.CouncilConfig
	Enabled       bool
}

var keyHelp = map[string]string{"anthropic": "claude", "openai": "openai", "gemini": "gemini", "ollama": "ollama"}

func (s *Server) modelOptions(allowCouncil bool, providers []string) []option {
	opts := []option{{Value: llm.SpecAuto, Label: "Automático (provedor padrão global)"}}
	if allowCouncil {
		opts = append(opts, option{Value: llm.SpecCouncil, Label: "🤝 Conselho (modelos debatem e um moderador decide)"})
	}
	cat := s.LLM.Catalog()
	for _, p := range llm.ProviderNames {
		if providers != nil && !slices.Contains(providers, p) {
			continue
		}
		ok := s.LLM.Configured(p)
		suffix := ""
		if !ok {
			suffix = " — sem chave"
		}
		opts = append(opts, option{Value: p, Label: fmt.Sprintf("%s — modelo padrão (%s)%s", llm.ProviderLabel(p), s.Cfg.Get(llm.DefaultModelKey(p)), suffix), Disabled: !ok})
		if providers != nil {
			continue // restricted tasks (transcription) choose provider only
		}
		for _, e := range cat {
			if e.Provider == p {
				opts = append(opts, option{Value: e.Spec(), Label: "   " + e.Spec() + suffix, Disabled: !ok})
			}
		}
	}
	return opts
}

func (s *Server) modelsPage(w http.ResponseWriter, r *http.Request) {
	v := modelsView{Enabled: s.LLM.Enabled()}
	cat := s.LLM.Catalog()
	for _, p := range llm.ProviderNames {
		pv := providerView{Name: p, Label: llm.ProviderLabel(p), Configured: s.LLM.Configured(p), Default: s.Cfg.Get(llm.DefaultModelKey(p)), KeyHelp: keyHelp[p]}
		for _, e := range cat {
			if e.Provider == p {
				pv.Models = append(pv.Models, e)
			}
		}
		for _, sug := range llm.Suggestions[p] {
			if !slices.ContainsFunc(pv.Models, func(e llm.ModelEntry) bool { return e.Model == sug }) {
				pv.Suggestions = append(pv.Suggestions, sug)
			}
		}
		v.Providers = append(v.Providers, pv)
	}
	for _, t := range llm.Tasks {
		v.Tasks = append(v.Tasks, taskView{TaskInfo: t, Value: s.LLM.Route(t.Key), Options: s.modelOptions(t.AllowCouncil, t.Providers)})
	}
	v.GlobalDefault = s.Cfg.Get("DEFAULT_LLM_PROVIDER")
	v.GlobalOptions = []option{{Value: llm.SpecAuto, Label: "Automático (primeiro com chave: Claude → GPT → Gemini → Local)"}}
	for _, p := range llm.ProviderNames {
		v.GlobalOptions = append(v.GlobalOptions, option{Value: p, Label: llm.ProviderLabel(p), Disabled: !s.LLM.Configured(p)})
	}
	v.Members = map[string]bool{}
	for _, m := range s.Cfg.GetList("LLM_COUNCIL_MEMBERS") {
		v.Members[m] = true
	}
	for _, p := range llm.ProviderNames {
		ok := s.LLM.Configured(p)
		v.MemberOpts = append(v.MemberOpts, option{Value: p, Label: llm.ProviderLabel(p) + " — padrão", Disabled: !ok})
		for _, e := range cat {
			if e.Provider == p && !e.Default {
				v.MemberOpts = append(v.MemberOpts, option{Value: e.Spec(), Label: e.Spec(), Disabled: !ok})
			}
		}
	}
	v.Judge = s.Cfg.Get("LLM_COUNCIL_JUDGE")
	v.JudgeOpts = v.MemberOpts
	v.Rounds = s.Cfg.GetInt("LLM_COUNCIL_ROUNDS", 1)
	v.Effective = s.LLM.CouncilSetup()
	s.render(w, "models", s.page(r, "Modelos", "models", v))
}

func validModelID(m string) bool {
	return m != "" && len(m) <= 120 && !strings.ContainsAny(m, " ,;:\n\t\"'<>")
}

func (s *Server) saveModels(w http.ResponseWriter, r *http.Request, changes map[string]string, msg string) {
	if err := s.Cfg.Update(changes); err != nil {
		redirectFlash(w, r, "/models", err.Error(), true)
		return
	}
	s.LLM.Reload(s.Cfg)
	s.log.Info("configuração de modelos alterada", "keys", len(changes))
	redirectFlash(w, r, "/models", msg, false)
}

func (s *Server) modelAdd(w http.ResponseWriter, r *http.Request) {
	p, m := r.FormValue("provider"), strings.TrimSpace(r.FormValue("model"))
	if llm.DefaultModelKey(p) == "" || !validModelID(m) {
		redirectFlash(w, r, "/models", "provedor ou nome de modelo inválido", true)
		return
	}
	cat := llm.ParseCatalog(s.Cfg.Get("LLM_MODELS"))
	spec := p + ":" + m
	if slices.Contains(cat, spec) {
		redirectFlash(w, r, "/models", spec+" já está no catálogo", false)
		return
	}
	changes := map[string]string{"LLM_MODELS": strings.Join(append(cat, spec), ",")}
	if r.FormValue("default") == "true" {
		changes[llm.DefaultModelKey(p)] = m
	}
	s.saveModels(w, r, changes, "Modelo "+spec+" adicionado.")
}

func (s *Server) modelRemove(w http.ResponseWriter, r *http.Request) {
	spec := r.FormValue("spec")
	p, m, _ := strings.Cut(spec, ":")
	if s.Cfg.Get(llm.DefaultModelKey(p)) == m {
		redirectFlash(w, r, "/models", "Defina outro modelo padrão para "+llm.ProviderLabel(p)+" antes de remover este.", true)
		return
	}
	var kept []string
	for _, c := range llm.ParseCatalog(s.Cfg.Get("LLM_MODELS")) {
		if c != spec {
			kept = append(kept, c)
		}
	}
	changes := map[string]string{"LLM_MODELS": strings.Join(kept, ",")}
	for _, t := range llm.Tasks {
		if s.LLM.Route(t.Key) == spec {
			changes[llm.RouteKey(t.Key)] = p // fall back to the provider default
		}
	}
	var members []string
	for _, mem := range s.Cfg.GetList("LLM_COUNCIL_MEMBERS") {
		if mem != spec {
			members = append(members, mem)
		}
	}
	changes["LLM_COUNCIL_MEMBERS"] = strings.Join(members, ",")
	if s.Cfg.Get("LLM_COUNCIL_JUDGE") == spec {
		changes["LLM_COUNCIL_JUDGE"] = p
	}
	s.saveModels(w, r, changes, "Modelo "+spec+" removido.")
}

func (s *Server) modelDefault(w http.ResponseWriter, r *http.Request) {
	spec := r.FormValue("spec")
	p, m, _ := strings.Cut(spec, ":")
	if llm.DefaultModelKey(p) == "" || !validModelID(m) {
		redirectFlash(w, r, "/models", "modelo inválido", true)
		return
	}
	changes := map[string]string{llm.DefaultModelKey(p): m}
	cat := llm.ParseCatalog(s.Cfg.Get("LLM_MODELS"))
	if !slices.Contains(cat, spec) {
		changes["LLM_MODELS"] = strings.Join(append(cat, spec), ",")
	}
	s.saveModels(w, r, changes, fmt.Sprintf("%s agora é o padrão de %s.", m, llm.ProviderLabel(p)))
}

func (s *Server) validRoute(v string, t llm.TaskInfo) bool {
	if v == llm.SpecAuto {
		return true
	}
	if v == llm.SpecCouncil {
		return t.AllowCouncil
	}
	p, m, hasModel := strings.Cut(v, ":")
	if llm.DefaultModelKey(p) == "" || (t.Providers != nil && !slices.Contains(t.Providers, p)) {
		return false
	}
	return !hasModel || validModelID(m)
}

func (s *Server) modelRoutes(w http.ResponseWriter, r *http.Request) {
	changes := map[string]string{}
	for _, t := range llm.Tasks {
		v := strings.TrimSpace(r.FormValue(llm.RouteKey(t.Key)))
		if v == "" {
			continue
		}
		if !s.validRoute(v, t) {
			redirectFlash(w, r, "/models", "opção inválida para "+t.Label, true)
			return
		}
		changes[llm.RouteKey(t.Key)] = v
	}
	if g := r.FormValue("DEFAULT_LLM_PROVIDER"); g == llm.SpecAuto || llm.DefaultModelKey(g) != "" {
		changes["DEFAULT_LLM_PROVIDER"] = g
	}
	s.saveModels(w, r, changes, "Modelos por tarefa salvos.")
}

func (s *Server) modelCouncil(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/models", err.Error(), true)
		return
	}
	var members []string
	for _, m := range r.PostForm["member"] {
		p, model, hasModel := strings.Cut(m, ":")
		if llm.DefaultModelKey(p) != "" && (!hasModel || validModelID(model)) {
			members = append(members, m)
		}
	}
	if len(members) < 2 {
		redirectFlash(w, r, "/models#council", "Escolha ao menos 2 membros para o Conselho.", true)
		return
	}
	rounds, _ := strconv.Atoi(r.FormValue("rounds"))
	changes := map[string]string{
		"LLM_COUNCIL_MEMBERS": strings.Join(members, ","),
		"LLM_COUNCIL_ROUNDS":  strconv.Itoa(min(max(rounds, 0), 3)),
	}
	if j := r.FormValue("judge"); llm.DefaultModelKey(strings.SplitN(j, ":", 2)[0]) != "" {
		changes["LLM_COUNCIL_JUDGE"] = j
	}
	s.saveModels(w, r, changes, "Conselho atualizado.")
}

// modelTest sends a tiny prompt and returns an HTML snippet (htmx).
func (s *Server) modelTest(w http.ResponseWriter, r *http.Request) {
	spec := r.FormValue("spec")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := s.LLM.Complete(ctx, spec, llm.Request{Purpose: "test", MaxTokens: 1024, Effort: "low",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "Responda apenas com a palavra OK."}}})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		fmt.Fprintf(w, `<span class="pill err" title="%s">❌ %s</span>`, html.EscapeString(err.Error()), html.EscapeString(friendlyLLMError(err)))
		return
	}
	fmt.Fprintf(w, `<span class="pill ok">✅ %s · %d ms</span>`, html.EscapeString(resp.Model), time.Since(start).Milliseconds())
}

// modelList fetches available models from the provider API (htmx fragment).
func (s *Server) modelList(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("provider")
	ids, err := s.LLM.ListModels(r.Context(), p)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		fmt.Fprintf(w, `<p class="warn">%s</p>`, html.EscapeString(friendlyLLMError(err)))
		return
	}
	s.fragment(w, "model_list", map[string]any{"Provider": p, "IDs": ids})
}

func friendlyLLMError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "HTTP 401"), strings.Contains(msg, "HTTP 403"), strings.Contains(msg, "API key"):
		return "Chave de API inválida ou sem permissão."
	case strings.Contains(msg, "HTTP 402"), strings.Contains(msg, "credit"), strings.Contains(msg, "quota"), strings.Contains(msg, "billing"):
		return "Sem créditos/cota na conta do provedor (verifique o faturamento)."
	case strings.Contains(msg, "HTTP 404"), strings.Contains(msg, "not_found"), strings.Contains(msg, "does not exist"):
		return "Modelo não encontrado — confira o nome exato."
	case strings.Contains(msg, "HTTP 429"):
		return "Limite de requisições atingido; tente em instantes."
	case strings.Contains(msg, "não configurado"), strings.Contains(msg, "nenhum provedor"):
		return "Provedor sem chave de API configurada."
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"), strings.Contains(msg, "timeout"):
		return "Não foi possível conectar ao servidor do modelo."
	}
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}
