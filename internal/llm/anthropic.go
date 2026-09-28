package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Anthropic implements Provider for the Claude Messages API (raw HTTP, streaming).
type Anthropic struct {
	apiKey  string
	model   string
	baseURL string
}

// NewAnthropic builds a Claude client.
func NewAnthropic(apiKey, model string) *Anthropic {
	return &Anthropic{apiKey: apiKey, model: model, baseURL: "https://api.anthropic.com/v1"}
}

// Name implements Provider.
func (a *Anthropic) Name() string { return "anthropic" }

// Model implements Provider.
func (a *Anthropic) Model() string { return a.model }

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func (a *Anthropic) contentBlocks(m Message) []map[string]any {
	var blocks []map[string]any
	for _, p := range m.Parts {
		switch p.Type {
		case PartImage:
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": p.MIME, "data": base64.StdEncoding.EncodeToString(p.Data)}})
		case PartFile:
			if p.MIME == "application/pdf" {
				blocks = append(blocks, map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": base64.StdEncoding.EncodeToString(p.Data)}})
			} else {
				blocks = append(blocks, map[string]any{"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": string(p.Data)}})
			}
		case PartAudio:
			blocks = append(blocks, map[string]any{"type": "text", "text": "[áudio anexado não suportado por este provedor]"})
		}
	}
	if m.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
	}
	for _, p := range m.Parts {
		if p.Type == PartText && p.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		}
	}
	return blocks
}

func (a *Anthropic) convert(req Request) []map[string]any {
	type turn struct {
		role   string
		blocks []any
	}
	var turns []turn
	push := func(role string, blocks ...any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return
		}
		turns = append(turns, turn{role: role, blocks: blocks})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			push("user", map[string]any{"type": "text", "text": m.TextOf()})
		case RoleTool:
			push("user", map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content})
		case RoleAssistant:
			if m.RawProvider == "anthropic" && len(m.Raw) > 0 {
				var raw []any
				if json.Unmarshal(m.Raw, &raw) == nil && len(raw) > 0 {
					push("assistant", raw...)
					continue
				}
			}
			var blocks []any
			for _, t := range m.Thinking {
				if t.Type == "redacted_thinking" {
					blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": t.Data})
				} else {
					blocks = append(blocks, map[string]any{"type": "thinking", "thinking": t.Thinking, "signature": t.Signature})
				}
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, c := range m.ToolCalls {
				var input any = map[string]any{}
				if len(c.Arguments) > 0 {
					_ = json.Unmarshal(c.Arguments, &input)
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": input})
			}
			push("assistant", blocks...)
		default:
			var bs []any
			for _, b := range a.contentBlocks(m) {
				bs = append(bs, b)
			}
			push("user", bs...)
		}
	}
	for len(turns) > 0 && turns[0].role != "user" {
		turns = turns[1:]
	}
	out := make([]map[string]any, 0, len(turns))
	for _, t := range turns {
		out = append(out, map[string]any{"role": t.role, "content": t.blocks})
	}
	return out
}

type anthEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthBlock struct {
	typ, id, name, data string
	text, json          strings.Builder
	thinking, signature strings.Builder
}

// Stream implements Provider.
func (a *Anthropic) Stream(ctx context.Context, req Request, onText StreamFunc) (*Response, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	max := req.MaxTokens
	if max <= 0 {
		max = 32000
	}
	system := req.System
	if req.JSON {
		system = strings.TrimSpace(system + "\n\nResponda exclusivamente com um objeto JSON válido, sem texto adicional.")
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": max,
		"stream":     true,
		"messages":   a.convert(req),
	}
	if system != "" {
		body["system"] = system
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			schema := t.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schema})
		}
		body["tools"] = tools
	}
	if req.Effort != "" && hasAnyPrefix(model, "claude-opus-5", "claude-fable-5", "claude-mythos", "claude-sonnet-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-6") {
		body["output_config"] = map[string]any{"effort": req.Effort}
	}
	headers := map[string]string{"x-api-key": a.apiKey, "anthropic-version": "2023-06-01"}
	if hasAnyPrefix(model, "claude-opus-5", "claude-fable-5") {
		// Server-side refusal fallback: a classifier decline is re-served by a fallback model.
		body["fallbacks"] = "default"
		headers["anthropic-beta"] = "server-side-fallback-2026-07-01"
	}
	resp, err := post(ctx, "anthropic", a.baseURL+"/messages", headers, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{Provider: "anthropic", Model: model}
	blocks := map[int]*anthBlock{}
	var order []int
	var text strings.Builder
	err = readSSE(resp.Body, func(event, data string) error {
		var ev anthEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				if ev.Message.Model != "" {
					out.Model = ev.Message.Model
				}
				u := ev.Message.Usage
				out.Usage.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				out.Usage.OutputTokens = u.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				return nil
			}
			b := &anthBlock{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, data: ev.ContentBlock.Data}
			b.text.WriteString(ev.ContentBlock.Text)
			b.thinking.WriteString(ev.ContentBlock.Thinking)
			b.signature.WriteString(ev.ContentBlock.Signature)
			blocks[ev.Index] = b
			order = append(order, ev.Index)
			if ev.ContentBlock.Text != "" {
				text.WriteString(ev.ContentBlock.Text)
				if onText != nil {
					return onText(ev.ContentBlock.Text)
				}
			}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil || ev.Delta == nil {
				return nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				text.WriteString(ev.Delta.Text)
				if onText != nil && ev.Delta.Text != "" {
					return onText(ev.Delta.Text)
				}
			case "input_json_delta":
				b.json.WriteString(ev.Delta.PartialJSON)
			case "thinking_delta":
				b.thinking.WriteString(ev.Delta.Thinking)
			case "signature_delta":
				b.signature.WriteString(ev.Delta.Signature)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				out.Usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			msg := data
			if ev.Error != nil {
				msg = ev.Error.Type + ": " + ev.Error.Message
			}
			status := 500
			if ev.Error != nil && ev.Error.Type == "invalid_request_error" {
				status = 400
			}
			return &APIError{Provider: "anthropic", Status: status, Body: msg}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var raw []map[string]any
	for _, i := range order {
		b := blocks[i]
		switch b.typ {
		case "text":
			raw = append(raw, map[string]any{"type": "text", "text": b.text.String()})
		case "thinking":
			tb := ThinkingBlock{Type: "thinking", Thinking: b.thinking.String(), Signature: b.signature.String()}
			out.Thinking = append(out.Thinking, tb)
			raw = append(raw, map[string]any{"type": "thinking", "thinking": tb.Thinking, "signature": tb.Signature})
		case "redacted_thinking":
			out.Thinking = append(out.Thinking, ThinkingBlock{Type: "redacted_thinking", Data: b.data})
			raw = append(raw, map[string]any{"type": "redacted_thinking", "data": b.data})
		case "tool_use":
			args := b.json.String()
			if strings.TrimSpace(args) == "" || !json.Valid([]byte(args)) {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.id, Name: b.name, Arguments: json.RawMessage(args)})
			var input any
			_ = json.Unmarshal([]byte(args), &input)
			raw = append(raw, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": input})
		}
	}
	out.Text = text.String()
	if out.StopReason == "refusal" && out.Text == "" {
		out.Text = "(O modelo recusou esta solicitação.)"
	}
	if len(raw) > 0 {
		out.Raw, _ = json.Marshal(raw)
	}
	return out, nil
}
