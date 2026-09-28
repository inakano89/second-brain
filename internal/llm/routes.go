package llm

import (
	"sort"
	"strings"
	"time"
)

// Special route values.
const (
	SpecAuto    = "auto"
	SpecCouncil = "council"
)

// Task keys that can be routed to a specific model.
const (
	TaskChat          = "chat"
	TaskTelegram      = "telegram"
	TaskEnrich        = "enrich"
	TaskVision        = "vision"
	TaskTranscription = "transcription"
	TaskEvents        = "events"
	TaskEmail         = "email"
	TaskRSS           = "rss"
	TaskBriefing      = "briefing"
	TaskReview        = "review"
)

// TaskInfo describes a routable task for the UI.
type TaskInfo struct {
	Key          string
	Label        string
	Help         string
	Purposes     []string
	AllowCouncil bool
	Providers    []string // restrict choices (nil = any)
}

// Tasks is the catalogue of routable tasks.
var Tasks = []TaskInfo{
	{Key: TaskChat, Label: "Chat (web)", Help: "Modelo padrão da página de Chat.", Purposes: []string{"chat"}, AllowCouncil: true},
	{Key: TaskTelegram, Label: "Conversa no Telegram", Help: "Respostas às mensagens de texto do bot.", Purposes: []string{"telegram"}, AllowCouncil: true},
	{Key: TaskEnrich, Label: "Auto-tagging e auto-linking", Help: "Resumo, tags, pessoas e tarefas de cada nota nova. Roda muitas vezes: prefira um modelo barato.", Purposes: []string{"enrich"}, AllowCouncil: true},
	{Key: TaskVision, Label: "Imagens e PDFs (OCR)", Help: "Leitura de fotos, prints, recibos e PDFs digitalizados.", Purposes: []string{"vision", "pdf-ocr"}},
	{Key: TaskTranscription, Label: "Transcrição de voz", Help: "Áudios do Telegram e da pasta inbox.", Purposes: []string{"transcription"}, Providers: []string{"openai", "gemini"}},
	{Key: TaskEvents, Label: "Eventos por linguagem natural", Help: "Converte “amanhã 15h reunião” em evento do Google Calendar.", Purposes: []string{"event-parse"}, AllowCouncil: true},
	{Key: TaskEmail, Label: "E-mails e newsletters", Help: "Triagem do Gmail e resumo de newsletters.", Purposes: []string{"gmail", "newsletter"}, AllowCouncil: true},
	{Key: TaskRSS, Label: "Curadoria de RSS", Help: "Nota de relevância e resumo das notícias.", Purposes: []string{"rss"}, AllowCouncil: true},
	{Key: TaskBriefing, Label: "Briefing matinal", Help: "Cruza sono, agenda e pendências. Ideal para o Conselho.", Purposes: []string{"briefing"}, AllowCouncil: true},
	{Key: TaskReview, Label: "Balanço noturno e weekly review", Help: "Revisões diária e semanal. Ideal para o Conselho.", Purposes: []string{"evening", "weekly"}, AllowCouncil: true},
}

var purposeTask = func() map[string]string {
	m := map[string]string{}
	for _, t := range Tasks {
		for _, p := range t.Purposes {
			m[p] = t.Key
		}
	}
	return m
}()

// TaskForPurpose maps a request purpose (usage label) to its routable task.
func TaskForPurpose(purpose string) string {
	base, _, _ := strings.Cut(purpose, ":")
	return purposeTask[base]
}

// RouteKey is the .env key storing the route for task.
func RouteKey(task string) string { return "LLM_ROUTE_" + strings.ToUpper(task) }

// Route returns the configured spec for a task ("auto", "council", "provider[:model]").
func (m *Manager) Route(task string) string {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg == nil {
		return SpecAuto
	}
	v := strings.TrimSpace(cfg.Get(RouteKey(task)))
	if v == "" {
		return SpecAuto
	}
	return v
}

// ---- model catalogue ----

// ProviderNames lists supported providers in display order.
var ProviderNames = []string{"anthropic", "openai", "gemini", "ollama"}

// ProviderLabel returns a friendly provider name.
func ProviderLabel(p string) string {
	switch p {
	case "anthropic":
		return "Claude (Anthropic)"
	case "openai":
		return "GPT (OpenAI)"
	case "gemini":
		return "Gemini (Google)"
	case "ollama":
		return "Local (Ollama/llama.cpp)"
	}
	return p
}

// DefaultModelKey is the .env key holding the provider's default model.
func DefaultModelKey(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_MODEL"
	case "openai":
		return "OPENAI_MODEL"
	case "gemini":
		return "GEMINI_MODEL"
	case "ollama":
		return "OLLAMA_MODEL"
	}
	return ""
}

var localSuggestions = []string{"llama3.1", "qwen2.5", "gemma3", "mistral"}

// Suggestions lists model ids offered when adding to the catalogue: the curated
// list for cloud providers, common local models for Ollama.
func (m *Manager) Suggestions(provider string) []string {
	if provider == "ollama" {
		return localSuggestions
	}
	var out []string
	for _, e := range m.Curated().Providers[provider].Models {
		out = append(out, e.ID)
	}
	return out
}

// ModelEntry is one catalogue item.
type ModelEntry struct {
	Provider   string
	Model      string
	Name       string // friendly name from the curated catalogue
	Note       string
	Price      *Price // today's price; nil when unknown
	PriceNote  string // upcoming changes / long-prompt tier
	Default    bool
	Configured bool
}

// Spec returns "provider:model".
func (e ModelEntry) Spec() string { return e.Provider + ":" + e.Model }

// ParseCatalog splits LLM_MODELS ("provider:model,...") into normalized specs.
func ParseCatalog(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		s = strings.TrimSpace(s)
		p, mdl, ok := strings.Cut(s, ":")
		if !ok || DefaultModelKey(p) == "" || strings.TrimSpace(mdl) == "" {
			continue
		}
		s = p + ":" + strings.TrimSpace(mdl)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Catalog lists catalogue models (defaults always included), grouped by provider.
func (m *Manager) Catalog() []ModelEntry {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	specs := ParseCatalog(cfg.Get("LLM_MODELS"))
	has := map[string]bool{}
	for _, s := range specs {
		has[s] = true
	}
	for _, p := range ProviderNames {
		if d := cfg.Get(DefaultModelKey(p)); d != "" && !has[p+":"+d] {
			specs = append(specs, p+":"+d)
		}
	}
	order := map[string]int{}
	for i, p := range ProviderNames {
		order[p] = i
	}
	cur := m.Curated()
	out := make([]ModelEntry, 0, len(specs))
	for _, s := range specs {
		p, mdl, _ := strings.Cut(s, ":")
		e := ModelEntry{Provider: p, Model: mdl, Default: cfg.Get(DefaultModelKey(p)) == mdl, Configured: m.Configured(p)}
		if cm, ok := cur.Lookup(s); ok {
			e.Name, e.Note = cm.Name, cm.Note
			e.PriceNote = strings.Join(cm.PriceNotes(time.Now()), " · ")
		}
		if pr, ok := m.PriceOf(p, mdl); ok && p != "ollama" {
			e.Price = &pr
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Provider] < order[out[j].Provider] })
	return out
}
