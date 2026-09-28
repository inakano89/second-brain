// Package llm is a provider-agnostic client for OpenAI, Anthropic Claude,
// Google Gemini and local OpenAI-compatible servers (Ollama / llama.cpp),
// with streaming, function calling and multimodal input.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Part kinds.
const (
	PartText  = "text"
	PartImage = "image"
	PartAudio = "audio"
	PartFile  = "file" // PDF or other document
)

// Part is one multimodal content element.
type Part struct {
	Type string
	Text string
	MIME string
	Data []byte
	Name string
}

// ToolCall is a function invocation requested by the model.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Signature string          `json:"signature,omitempty"` // Gemini thoughtSignature
}

// ThinkingBlock preserves provider reasoning blocks that must be echoed back (Anthropic).
type ThinkingBlock struct {
	Type      string `json:"type"` // thinking | redacted_thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// Message is a conversation turn.
type Message struct {
	Role       string
	Content    string
	Parts      []Part
	ToolCalls  []ToolCall
	ToolCallID string
	ToolName   string
	Thinking   []ThinkingBlock
	// Raw holds the provider-native assistant content (e.g. Anthropic content
	// blocks) so it can be replayed verbatim to the same provider.
	Raw         json.RawMessage
	RawProvider string
}

// Tool declares a callable function (JSON-schema parameters).
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Request is a provider-neutral completion request.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
	JSON      bool
	Effort    string // low|medium|high (Anthropic output_config.effort when supported)
	Purpose   string // accounting label
}

// Usage is token accounting for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the aggregated result of a (streamed) completion.
type Response struct {
	Provider   string
	Model      string
	Text       string
	ToolCalls  []ToolCall
	Thinking   []ThinkingBlock
	Usage      Usage
	StopReason string
	Raw        json.RawMessage
	Council    []Opinion // set when the answer came from a council deliberation
}

// AssistantMessage converts a response into a replayable assistant turn.
func (r *Response) AssistantMessage() Message {
	return Message{Role: RoleAssistant, Content: r.Text, ToolCalls: r.ToolCalls, Thinking: r.Thinking, Raw: r.Raw, RawProvider: r.Provider}
}

// StreamFunc receives incremental text deltas.
type StreamFunc func(delta string) error

// Provider is implemented by every backend.
type Provider interface {
	Name() string
	Model() string
	Stream(ctx context.Context, req Request, onText StreamFunc) (*Response, error)
}

// Embedder produces vector embeddings.
type Embedder interface {
	EmbedModel() string
	Embed(ctx context.Context, texts []string) ([][]float32, Usage, error)
}

// Transcriber converts speech audio to text.
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, filename, mime string) (text string, cost float64, err error)
}

// APIError is a non-2xx provider response.
type APIError struct {
	Provider string
	Status   int
	Body     string
}

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 500 {
		b = b[:500] + "…"
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, b)
}

// ErrNoProvider is returned when no LLM is configured.
var ErrNoProvider = errors.New("llm: nenhum provedor configurado")

// ErrNotConfigured is returned when a specific provider has no credentials.
var ErrNotConfigured = errors.New("llm: provedor não configurado")

// IsRetryable reports whether err is transient (network, 429, 5xx).
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoProvider) || errors.Is(err, ErrNotConfigured) || errors.Is(err, context.Canceled) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 408 || ae.Status == 409 || ae.Status == 429 || ae.Status >= 500
	}
	return true // network errors
}

// TextOf returns the concatenated text of a message (Content + text parts).
func (m Message) TextOf() string {
	var b strings.Builder
	b.WriteString(m.Content)
	for _, p := range m.Parts {
		if p.Type == PartText {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ExtractJSON finds the first balanced JSON object/array in s.
func ExtractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		if j := strings.Index(rest, "```"); j >= 0 {
			s = strings.TrimSpace(rest[:j])
		}
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return s
	}
	open, close := s[start], byte('}')
	if open == '[' {
		close = ']'
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}

func estimateTokens(s string) int { return len(s)/4 + 1 }

func defaultMax(n int) int {
	if n <= 0 {
		return 16000
	}
	return n
}
