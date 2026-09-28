package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/inakano89/second-brain/internal/config"
)

// UsageFunc receives accounting data for each call.
type UsageFunc func(provider, model, purpose string, u Usage, costUSD float64)

// ProviderInfo describes a configured provider for the UI selector.
type ProviderInfo struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Default bool   `json:"default"`
}

// Manager routes requests to configured providers and records usage.
type Manager struct {
	mu          sync.RWMutex
	providers   map[string]Provider
	order       []string
	def         string
	multimodal  string
	embedder    Embedder
	transcriber Transcriber
	pricing     Pricing
	record      UsageFunc
}

// NewManager builds a Manager from configuration.
func NewManager(cfg *config.Config, rec UsageFunc) *Manager {
	m := &Manager{record: rec}
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
	mm := cfg.Get("MULTIMODAL_PROVIDER")
	if _, ok := providers[mm]; !ok {
		mm = ""
		for _, n := range []string{"gemini", "anthropic", "openai", "ollama"} {
			if _, ok := providers[n]; ok {
				mm = n
				break
			}
		}
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
	m.providers, m.order, m.def, m.multimodal = providers, order, def, mm
	m.embedder, m.transcriber = emb, tr
	m.pricing = NewPricing(cfg.Get("LLM_PRICING"))
	m.mu.Unlock()
}

// Enabled reports whether at least one chat provider exists.
func (m *Manager) Enabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.providers) > 0
}

// Providers lists configured providers.
func (m *Manager) Providers() []ProviderInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ProviderInfo
	for _, n := range m.order {
		out = append(out, ProviderInfo{Name: n, Model: m.providers[n].Model(), Default: n == m.def})
	}
	return out
}

// Resolve parses "provider[:model]" (empty = default).
func (m *Manager) Resolve(spec string) (Provider, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, model, _ := strings.Cut(strings.TrimSpace(spec), ":")
	if name == "" || name == "auto" {
		name = m.def
	}
	p, ok := m.providers[name]
	if !ok {
		if len(m.providers) == 0 {
			return nil, "", ErrNoProvider
		}
		return nil, "", fmt.Errorf("llm: provedor %q não configurado", name)
	}
	return p, model, nil
}

func (m *Manager) candidates(spec string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if spec != "" && spec != "auto" {
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

// Stream runs a streaming completion on one provider (no fallback: text may already be emitted).
func (m *Manager) Stream(ctx context.Context, spec string, req Request, onText StreamFunc) (*Response, error) {
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

// Complete runs a non-streamed completion; with empty spec it fails over across providers.
func (m *Manager) Complete(ctx context.Context, spec string, req Request) (*Response, error) {
	var lastErr error = ErrNoProvider
	for _, c := range m.candidates(spec) {
		if c == "" {
			continue
		}
		resp, err := m.Stream(ctx, c, req, nil)
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

// Multimodal runs a request with images/audio/documents on the multimodal provider.
func (m *Manager) Multimodal(ctx context.Context, req Request) (*Response, error) {
	m.mu.RLock()
	spec := m.multimodal
	m.mu.RUnlock()
	if spec == "" {
		return nil, ErrNoProvider
	}
	return m.Complete(ctx, spec, req)
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

// Transcribe converts audio to text (OpenAI Whisper, else Gemini audio understanding).
func (m *Manager) Transcribe(ctx context.Context, audio []byte, filename, mime string) (string, error) {
	m.mu.RLock()
	tr := m.transcriber
	_, hasGemini := m.providers["gemini"]
	m.mu.RUnlock()
	if tr != nil {
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
	resp, err := m.Complete(ctx, "gemini", Request{
		Purpose:  "transcription",
		System:   "Você é um transcritor. Transcreva fielmente o áudio no idioma original. Responda apenas com a transcrição, sem comentários.",
		Messages: []Message{{Role: RoleUser, Content: "Transcreva este áudio.", Parts: []Part{{Type: PartAudio, MIME: mime, Data: audio, Name: filename}}}},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}
