package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Gemini implements Provider/Embedder for the Google Generative Language API.
type Gemini struct {
	apiKey     string
	model      string
	embedModel string
	baseURL    string
}

// NewGemini builds a Gemini client.
func NewGemini(apiKey, model, embedModel string) *Gemini {
	return &Gemini{apiKey: apiKey, model: model, embedModel: embedModel, baseURL: "https://generativelanguage.googleapis.com/v1beta"}
}

// Name implements Provider.
func (g *Gemini) Name() string { return "gemini" }

// Model implements Provider.
func (g *Gemini) Model() string { return g.model }

// EmbedModel implements Embedder.
func (g *Gemini) EmbedModel() string { return "gemini:" + g.embedModel }

func (g *Gemini) headers() map[string]string { return map[string]string{"x-goog-api-key": g.apiKey} }

func inline(mime string, data []byte) map[string]any {
	return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(data)}}
}

func (g *Gemini) convert(req Request) []map[string]any {
	var contents []map[string]any
	push := func(role string, parts ...any) {
		if len(parts) == 0 {
			return
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == role {
			contents[n-1]["parts"] = append(contents[n-1]["parts"].([]any), parts...)
			return
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			var result any = m.Content
			var parsed any
			if json.Unmarshal([]byte(m.Content), &parsed) == nil {
				result = parsed
			}
			fr := map[string]any{"name": m.ToolName, "response": map[string]any{"result": result}}
			push("user", map[string]any{"functionResponse": fr})
		case RoleAssistant:
			if m.RawProvider == "gemini" && len(m.Raw) > 0 {
				var raw []any
				if json.Unmarshal(m.Raw, &raw) == nil && len(raw) > 0 {
					push("model", raw...)
					continue
				}
			}
			var parts []any
			if m.Content != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, c := range m.ToolCalls {
				var args any = map[string]any{}
				_ = json.Unmarshal(c.Arguments, &args)
				p := map[string]any{"functionCall": map[string]any{"name": c.Name, "args": args}}
				if c.Signature != "" {
					p["thoughtSignature"] = c.Signature
				}
				parts = append(parts, p)
			}
			push("model", parts...)
		default:
			var parts []any
			for _, p := range m.Parts {
				switch p.Type {
				case PartImage, PartAudio, PartFile:
					parts = append(parts, inline(p.MIME, p.Data))
				case PartText:
					parts = append(parts, map[string]any{"text": p.Text})
				}
			}
			if t := m.Content; t != "" {
				parts = append(parts, map[string]any{"text": t})
			}
			push("user", parts...)
		}
	}
	return contents
}

type gemChunk struct {
	Candidates []struct {
		Content struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	ModelVersion string `json:"modelVersion"`
	Error        *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type gemPart struct {
	Text             string `json:"text"`
	Thought          bool   `json:"thought"`
	ThoughtSignature string `json:"thoughtSignature"`
	FunctionCall     *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
}

// Stream implements Provider.
func (g *Gemini) Stream(ctx context.Context, req Request, onText StreamFunc) (*Response, error) {
	model := req.Model
	if model == "" {
		model = g.model
	}
	gen := map[string]any{"maxOutputTokens": defaultMax(req.MaxTokens)}
	body := map[string]any{"contents": g.convert(req), "generationConfig": gen}
	if req.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": req.System}}}
	}
	if len(req.Tools) > 0 {
		var decls []map[string]any
		for _, t := range req.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if props, ok := t.Parameters["properties"].(map[string]any); ok && len(props) > 0 {
				d["parameters"] = t.Parameters
			}
			decls = append(decls, d)
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	} else if req.JSON {
		gen["responseMimeType"] = "application/json"
	}
	u := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", g.baseURL, url.PathEscape(model))
	resp, err := post(ctx, "gemini", u, g.headers(), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{Provider: "gemini", Model: model}
	var text strings.Builder
	var raw []map[string]any
	var pendingText strings.Builder
	flushText := func() {
		if pendingText.Len() > 0 {
			raw = append(raw, map[string]any{"text": pendingText.String()})
			pendingText.Reset()
		}
	}
	callN := 0
	err = readSSE(resp.Body, func(_, data string) error {
		var ch gemChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil
		}
		if ch.Error != nil {
			return &APIError{Provider: "gemini", Status: ch.Error.Code, Body: ch.Error.Message}
		}
		if ch.PromptFeedback != nil && ch.PromptFeedback.BlockReason != "" {
			out.StopReason = "blocked:" + ch.PromptFeedback.BlockReason
		}
		if ch.UsageMetadata != nil {
			out.Usage = Usage{InputTokens: ch.UsageMetadata.PromptTokenCount, OutputTokens: ch.UsageMetadata.CandidatesTokenCount + ch.UsageMetadata.ThoughtsTokenCount}
		}
		if ch.ModelVersion != "" {
			out.Model = ch.ModelVersion
		}
		for _, c := range ch.Candidates {
			if c.FinishReason != "" {
				out.StopReason = c.FinishReason
			}
			for _, rp := range c.Content.Parts {
				var p gemPart
				if json.Unmarshal(rp, &p) != nil || p.Thought {
					continue
				}
				if p.FunctionCall != nil {
					flushText()
					callN++
					id := p.FunctionCall.ID
					if id == "" {
						id = fmt.Sprintf("gem_call_%d", callN)
					}
					args := p.FunctionCall.Args
					if len(args) == 0 || !json.Valid(args) {
						args = json.RawMessage("{}")
					}
					out.ToolCalls = append(out.ToolCalls, ToolCall{ID: id, Name: p.FunctionCall.Name, Arguments: args, Signature: p.ThoughtSignature})
					var generic map[string]any
					_ = json.Unmarshal(rp, &generic)
					raw = append(raw, generic)
					continue
				}
				if p.Text != "" || p.ThoughtSignature != "" {
					if p.ThoughtSignature != "" {
						flushText()
						raw = append(raw, map[string]any{"text": p.Text, "thoughtSignature": p.ThoughtSignature})
					} else {
						pendingText.WriteString(p.Text)
					}
					if p.Text != "" {
						text.WriteString(p.Text)
						if onText != nil {
							if err := onText(p.Text); err != nil {
								return err
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	flushText()
	out.Text = text.String()
	if len(raw) > 0 {
		out.Raw, _ = json.Marshal(raw)
	}
	return out, nil
}

// Embed implements Embedder via batchEmbedContents.
func (g *Gemini) Embed(ctx context.Context, texts []string) ([][]float32, Usage, error) {
	var all [][]float32
	var usage Usage
	for start := 0; start < len(texts); start += 100 {
		end := min(start+100, len(texts))
		var reqs []map[string]any
		for _, t := range texts[start:end] {
			reqs = append(reqs, map[string]any{
				"model":                "models/" + g.embedModel,
				"content":              map[string]any{"parts": []any{map[string]any{"text": t}}},
				"outputDimensionality": 768,
			})
			usage.InputTokens += estimateTokens(t)
		}
		var resp struct {
			Embeddings []struct {
				Values []float32 `json:"values"`
			} `json:"embeddings"`
		}
		u := fmt.Sprintf("%s/models/%s:batchEmbedContents", g.baseURL, url.PathEscape(g.embedModel))
		if err := postJSON(ctx, "gemini", u, g.headers(), map[string]any{"requests": reqs}, &resp); err != nil {
			return nil, usage, err
		}
		if len(resp.Embeddings) != end-start {
			return nil, usage, fmt.Errorf("gemini: expected %d embeddings, got %d", end-start, len(resp.Embeddings))
		}
		for _, e := range resp.Embeddings {
			all = append(all, e.Values)
		}
	}
	return all, usage, nil
}
