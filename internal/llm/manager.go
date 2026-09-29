package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/inakano89/second-brain/internal/config"
)

// UsageFunc receives accounting data for each call.
type UsageFunc func(provider, model, purpose string, u Usage, costUSD float64)

// ProviderInfo describes a configured provider for the UI selector.
type ProviderInfo struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Model   string `json:"model"`
	Default bool   `json:"default"`
}

// Manager routes requests to configured providers and records usage.
type Manager struct {
	mu          sync.RWMutex
	cfg         *config.Config
	providers   map[string]Provider
	order       []string
	def         string
	embedder    Embedder
	transcriber Transcriber
	pricing     Pricing
	curated     *CuratedCatalog
	record      UsageFunc
}

// NewManager builds a Manager from configuration.
func NewManager(cfg *config.Config, rec UsageFunc) *Manager {
	m := &Manager{record: rec, cfg: cfg, curated: BuiltinCatalog()}
	m.Reload(cfg)
	return m
}

// Reload rebuilds providers after a configuration change.
func (m *Manager) Reload(cfg *config.Config) {
	providers := map[string]Provider{}
	var order []string
	var openai *OpenAI
	var gemini *Gemini
	var local *OpenAI
	if k := cfg.Get("ANTHROPIC_API_KEY"); k != "" {
		providers["anthropic"] = NewAnthropic(k, cfg.Get("ANTHROPIC_MODEL"))
		order = append(order, "anthropic")
	}
	if k := cfg.Get("OPENAI_API_KEY"); k != "" {
		openai = NewOpenAI(cfg.Get("OPENAI_BASE_URL"), k, cfg.Get("OPENAI_MODEL"), cfg.Get("OPENAI_EMBED_MODEL"), cfg.Get("OPENAI_TRANSCRIBE_MODEL"))
		providers["openai"] = openai
		order = append(order, "openai")
	}
	if k := cfg.Get("GEMINI_API_KEY"); k != "" {
		gemini = NewGemini(k, cfg.Get("GEMINI_MODEL"), cfg.Get("GEMINI_EMBED_MODEL"))
		providers["gemini"] = gemini
		order = append(order, "gemini")
	}
	if u := cfg.Get("OLLAMA_BASE_URL"); u != "" {
		local = NewLocal(u, cfg.Get("OLLAMA_MODEL"), cfg.Get("OLLAMA_EMBED_MODEL"))
		providers["ollama"] = local
		order = append(order, "ollama")
	}
	def := cfg.Get("DEFAULT_LLM_PROVIDER")
	if _, ok := providers[def]; !ok && len(order) > 0 {
		def = order[0]
	}
	var emb Embedder = LocalEmbedder{Dim: 512}
	switch cfg.Get("EMBEDDING_PROVIDER") {
	case "openai":
		if openai != nil {
			emb = openai
		}
	case "gemini":
		if gemini != nil {
			emb = gemini
		}
	case "ollama":
		if local != nil {
			emb = local
		}
	case "local":
	default:
		if openai != nil {
			emb = openai
		} else if gemini != nil {
			emb = gemini
		}
	}
	var tr Transcriber
	if openai != nil {
		tr = openai
	}
	m.mu.Lock()
	m.cfg = cfg
	m.providers, m.order, m.def = providers, order, def
	m.embedder, m.transcriber = emb, tr
	m.pricing = NewPricing(cfg.Get("LLM_PRICING"), m.curated)
	m.mu.Unlock()
}

// SetCurated installs the curated catalogue used for prices, names and suggestions.
func (m *Manager) SetCurated(c *CuratedCatalog) {
	if c == nil {
		return
	}
	m.mu.Lock()
	m.curated = c
	if m.cfg != nil {
		m.pricing = NewPricing(m.cfg.Get("LLM_PRICING"), c)
	}
	m.mu.Unlock()
}

// Curated returns the active curated catalogue.
func (m *Manager) Curated() *CuratedCatalog {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.curated
}

// PriceOf returns the known price of a model (USD per 1M tokens).
func (m *Manager) PriceOf(provider, model string) (Price, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pricing.Lookup(provider, model)
}

// Enabled reports whether at least one chat provider exists.
func (m *Manager) Enabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.providers) > 0
}

// Configured reports whether a provider has credentials.
func (m *Manager) Configured(provider string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.providers[provider]
	return ok
}

// IsLocal reports whether every model a request with this purpose may reach runs locally
// (Ollama/llama.cpp), so private data never leaves the machine.
func (m *Manager) IsLocal(purpose string) bool { return m.IsLocalSpec(m.routeSpec("", purpose)) }

// IsLocalSpec reports whether a spec ("auto", "council", "provider[:model]") can only reach
// local models.
func (m *Manager) IsLocalSpec(spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == SpecCouncil {
		return false
	}
	for _, c := range m.candidates(spec) {
		if name, _, _ := strings.Cut(c, ":"); name != "ollama" {
			return false
		}
	}
	return true
}

// Providers lists configured providers.
func (m *Manager) Providers() []ProviderInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ProviderInfo
	for _, n := range m.order {
		out = append(out, ProviderInfo{Name: n, Label: ProviderLabel(n), Model: m.providers[n].Model(), Default: n == m.def})
	}
	return out
}

// DefaultProvider returns the global fallback provider name.
func (m *Manager) DefaultProvider() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.def
}

// Resolve parses "provider[:model]" (empty/"auto" = default provider).
func (m *Manager) Resolve(spec string) (Provider, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, model, _ := strings.Cut(strings.TrimSpace(spec), ":")
	if name == "" || name == SpecAuto {
		name = m.def
	}
	p, ok := m.providers[name]
	if !ok {
		if len(m.providers) == 0 {
			return nil, "", ErrNoProvider
		}
		return nil, "", fmt.Errorf("%w: %q", ErrNotConfigured, name)
	}
	return p, model, nil
}

// routeSpec turns an empty spec into the configured route for the request purpose,
// degrading to "auto" when the routed provider has no credentials.
func (m *Manager) routeSpec(spec, purpose string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		if task := TaskForPurpose(purpose); task != "" {
			spec = m.Route(task)
		}
	}
	if spec == "" || spec == SpecAuto || spec == SpecCouncil {
		return spec
	}
	name, _, _ := strings.Cut(spec, ":")
	if !m.Configured(name) {
		slog.Warn("modelo roteado sem credenciais; usando automático", "component", "llm", "spec", spec, "purpose", purpose)
		return SpecAuto
	}
	return spec
}

func (m *Manager) candidates(spec string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if spec != "" && spec != SpecAuto {
		return []string{spec}
	}
	out := []string{m.def}
	for _, n := range m.order {
		if n != m.def {
			out = append(out, n)
		}
	}
	return out
}

func (m *Manager) account(resp *Response, purpose string) {
	if resp == nil || m.record == nil {
		return
	}
	m.mu.RLock()
	cost := m.pricing.Cost(resp.Provider, resp.Model, resp.Usage)
	m.mu.RUnlock()
	m.record(resp.Provider, resp.Model, purpose, resp.Usage, cost)
}

// streamOne runs a streaming completion on exactly one provider.
func (m *Manager) streamOne(ctx context.Context, spec string, req Request, onText StreamFunc) (*Response, error) {
	p, model, err := m.Resolve(spec)
	if err != nil {
		return nil, err
	}
	if model != "" {
		req.Model = model
	}
	resp, err := p.Stream(ctx, req, onText)
	if err != nil {
		return nil, err
	}
	m.account(resp, req.Purpose)
	return resp, nil
}

// Stream runs a streaming completion. An empty spec follows the task route for
// req.Purpose; "council" runs a multi-model deliberation and streams the synthesis.
func (m *Manager) Stream(ctx context.Context, spec string, req Request, onText StreamFunc) (*Response, error) {
	spec = m.routeSpec(spec, req.Purpose)
	if spec == SpecCouncil {
		return m.Council(ctx, req, onText, nil)
	}
	return m.streamOne(ctx, spec, req, onText)
}

// Complete runs a non-streamed completion; "auto" fails over across providers.
func (m *Manager) Complete(ctx context.Context, spec string, req Request) (*Response, error) {
	spec = m.routeSpec(spec, req.Purpose)
	if spec == SpecCouncil {
		return m.Council(ctx, req, nil, nil)
	}
	var lastErr error = ErrNoProvider
	for _, c := range m.candidates(spec) {
		if c == "" {
			continue
		}
		resp, err := m.streamOne(ctx, c, req, nil)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !IsRetryable(err) || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// CompleteJSON requests JSON output and decodes it into out.
func (m *Manager) CompleteJSON(ctx context.Context, spec string, req Request, out any) error {
	req.JSON = true
	resp, err := m.Complete(ctx, spec, req)
	if err != nil {
		return err
	}
	raw := ExtractJSON(resp.Text)
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("llm: resposta JSON inválida: %w", err)
	}
	return nil
}

var visionOrder = []string{"gemini", "anthropic", "openai", "ollama"}

// Multimodal runs a request with images/audio/documents using the "vision" route.
func (m *Manager) Multimodal(ctx context.Context, req Request) (*Response, error) {
	spec := m.routeSpec(m.Route(TaskVision), "")
	if spec == "" || spec == SpecAuto || spec == SpecCouncil {
		spec = ""
		for _, n := range visionOrder {
			if m.Configured(n) {
				spec = n
				break
			}
		}
	}
	if spec == "" {
		return nil, ErrNoProvider
	}
	return m.streamOne(ctx, spec, req, nil)
}

// EmbedModel returns the active embedding model id.
func (m *Manager) EmbedModel() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.embedder.EmbedModel()
}

// Embed embeds texts with the active embedder, recording usage.
func (m *Manager) Embed(ctx context.Context, texts []string) ([][]float32, string, error) {
	m.mu.RLock()
	e := m.embedder
	m.mu.RUnlock()
	vecs, u, err := e.Embed(ctx, texts)
	if err != nil {
		return nil, "", err
	}
	if len(vecs) != len(texts) {
		return nil, "", errors.New("llm: embedding count mismatch")
	}
	if m.record != nil && (u.InputTokens > 0) {
		prov, model, _ := strings.Cut(e.EmbedModel(), ":")
		m.mu.RLock()
		cost := m.pricing.Cost(prov, model, u)
		m.mu.RUnlock()
		m.record(prov, model, "embedding", u, cost)
	}
	return vecs, e.EmbedModel(), nil
}

// Transcribe converts audio to text following the "transcription" route
// (OpenAI Whisper or Gemini audio understanding).
func (m *Manager) Transcribe(ctx context.Context, audio []byte, filename, mime string) (string, error) {
	m.mu.RLock()
	tr := m.transcriber
	m.mu.RUnlock()
	hasGemini := m.Configured("gemini")
	route, _, _ := strings.Cut(m.Route(TaskTranscription), ":")
	useGemini := route == "gemini" && hasGemini
	if tr != nil && !useGemini {
		text, cost, err := tr.Transcribe(ctx, audio, filename, mime)
		if err == nil {
			if m.record != nil {
				m.record("openai", "whisper", "transcription", Usage{}, cost)
			}
			return text, nil
		}
		if !hasGemini {
			return "", err
		}
	}
	if !hasGemini {
		return "", errors.New("llm: transcrição requer OpenAI (Whisper) ou Gemini")
	}
	resp, err := m.streamOne(ctx, "gemini", Request{
		Purpose:  "transcription",
		System:   "Você é um transcritor. Transcreva fielmente o áudio no idioma original. Responda apenas com a transcrição, sem comentários.",
		Messages: []Message{{Role: RoleUser, Content: "Transcreva este áudio.", Parts: []Part{{Type: PartAudio, MIME: mime, Data: audio, Name: filename}}}},
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}
