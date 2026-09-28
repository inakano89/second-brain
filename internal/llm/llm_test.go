package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprint(w, e+"\n\n")
		w.(http.Flusher).Flush()
	}
}

var toolReq = Request{
	System:   "sys",
	Messages: []Message{{Role: RoleUser, Content: "oi"}},
	Tools:    []Tool{{Name: "search_brain", Description: "busca", Parameters: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}},
}

func collect(t *testing.T, p Provider) (*Response, string) {
	t.Helper()
	var b strings.Builder
	resp, err := p.Stream(context.Background(), toolReq, func(d string) error { b.WriteString(d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return resp, b.String()
}

func TestOpenAIStream(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		sse(w,
			`data: {"choices":[{"delta":{"content":"Olá"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"search_brain","arguments":"{\"que"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ry\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			`data: [DONE]`)
	}))
	defer srv.Close()
	resp, text := collect(t, NewOpenAI(srv.URL, "k", "gpt-4o-mini", "", ""))
	if text != "Olá" || len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Arguments) != `{"query":"x"}` || resp.Usage.OutputTokens != 5 {
		t.Fatalf("resp = %+v text=%q", resp, text)
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Fatal("max_completion_tokens not sent")
	}
}

func TestAnthropicStreamAndReplay(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") == "" || r.Header.Get("x-api-key") != "k" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		sse(w,
			`event: message_start`+"\n"+`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":12,"output_tokens":1}}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Vou buscar"}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu1","name":"search_brain","input":{}}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"y\"}"}}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
	}))
	defer srv.Close()
	a := NewAnthropic("k", "claude-opus-5")
	a.baseURL = srv.URL
	resp, text := collect(t, a)
	if text != "Vou buscar" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "tu1" || len(resp.Thinking) != 1 || resp.Thinking[0].Signature != "sig" || resp.Usage.OutputTokens != 40 {
		t.Fatalf("resp = %+v", resp)
	}
	// Replay: assistant raw blocks must be echoed verbatim, tool results grouped in one user turn.
	req := toolReq
	req.Messages = append(append([]Message{}, toolReq.Messages...), resp.AssistantMessage(),
		Message{Role: RoleTool, ToolCallID: "tu1", ToolName: "search_brain", Content: "[]"})
	if _, err := a.Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	msgs := got[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(msgs))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["type"] != "thinking" || asst[2].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant replay order wrong: %v", asst)
	}
	if got[0]["fallbacks"] != "default" {
		t.Fatal("refusal fallback not enabled for opus-5")
	}
}

func TestGeminiStream(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		sse(w,
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Ok "}]}}]}`,
			`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"search_brain","args":{"query":"z"}},"thoughtSignature":"ts"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"thoughtsTokenCount":2}}`)
	}))
	defer srv.Close()
	g := NewGemini("k", "gemini-2.5-flash", "")
	g.baseURL = srv.URL
	resp, text := collect(t, g)
	if text != "Ok " || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Signature != "ts" || resp.Usage.OutputTokens != 5 {
		t.Fatalf("resp = %+v", resp)
	}
	req := toolReq
	req.Messages = append(append([]Message{}, toolReq.Messages...), resp.AssistantMessage(),
		Message{Role: RoleTool, ToolCallID: resp.ToolCalls[0].ID, ToolName: "search_brain", Content: `{"ok":true}`})
	if _, err := g.Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got[1]["contents"])
	if !strings.Contains(string(b), `"thoughtSignature":"ts"`) || !strings.Contains(string(b), `"functionResponse"`) {
		t.Fatalf("gemini replay missing signature/functionResponse: %s", b)
	}
}

func TestHelpers(t *testing.T) {
	if ExtractJSON("texto ```json\n{\"a\":{\"b\":\"}\"}}\n``` fim") != `{"a":{"b":"}"}}` {
		t.Fatal("ExtractJSON")
	}
	v, _, _ := LocalEmbedder{}.Embed(context.Background(), []string{"orçamento do projeto atlas", "orcamento projeto atlas", "receita de bolo"})
	dot := func(a, b []float32) (s float32) {
		for i := range a {
			s += a[i] * b[i]
		}
		return
	}
	if dot(v[0], v[1]) <= dot(v[0], v[2]) {
		t.Fatal("local embedder similarity ordering")
	}
	p := NewPricing(`{"meu-modelo":[1,2]}`, BuiltinCatalog())
	if c := p.Cost("anthropic", "claude-opus-5-5", Usage{InputTokens: 1e6, OutputTokens: 1e6}); c != 24 {
		t.Fatalf("pricing opus-5-5 = %v", c)
	}
	if c := p.Cost("x", "meu-modelo-v2", Usage{InputTokens: 1e6}); c != 1 {
		t.Fatalf("override = %v", c)
	}
}
