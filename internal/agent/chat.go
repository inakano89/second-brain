package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

// ChatRequest is one user turn.
type ChatRequest struct {
	Channel  string // "web", "tg:<chat>"
	Provider string // "provider[:model]" or empty
	Text     string
	Parts    []llm.Part
	NoTools  bool
	History  int // turns of history to load (default 16)
}

// ChatEvent notifies the UI about retrieval and tool activity.
type ChatEvent struct {
	Type string `json:"type"` // context | tool_call | tool_result
	Data any    `json:"data"`
}

// ChatResult summarises a completed turn.
type ChatResult struct {
	Text     string
	Provider string
	Model    string
	Usage    llm.Usage
	Context  []database.Node
}

const maxToolRounds = 6

func (a *Agent) systemPrompt(ctxNodes []SearchResult) string {
	loc := a.cfg.Location()
	var b strings.Builder
	fmt.Fprintf(&b, `Você é o assistente pessoal do Second Brain "%s".
Agora: %s (%s).
Responda de forma direta e acionável, no idioma do usuário. Use o contexto recuperado quando relevante e cite notas como [[Título]].
Use as ferramentas para buscar mais informações, criar notas/tarefas/eventos ou executar ações quando o usuário pedir. Nunca invente IDs.`,
		a.cfg.Get("BRAIN_NAME"), time.Now().In(loc).Format("Monday, 02/01/2006 15:04"), loc.String())
	if len(ctxNodes) > 0 {
		b.WriteString("\n\n<contexto_recuperado>\n")
		for _, r := range ctxNodes {
			n := r.Node
			fmt.Fprintf(&b, "[id=%d tipo=%s criado=%s", n.ID, n.Type, n.CreatedAt.In(loc).Format("2006-01-02"))
			if n.Status != "" {
				fmt.Fprintf(&b, " status=%s", n.Status)
			}
			if len(n.Tags) > 0 {
				fmt.Fprintf(&b, " tags=%s", strings.Join(n.Tags, ","))
			}
			fmt.Fprintf(&b, "] %s\n", n.Title)
			body := n.Summary
			if len([]rune(n.Content)) < 1200 {
				body = n.Content
			} else if body == "" {
				body = extract.Truncate(n.Content, 1200)
			}
			b.WriteString(extract.Truncate(body, 1200) + "\n---\n")
		}
		b.WriteString("</contexto_recuperado>")
	}
	return b.String()
}

// Chat runs a RAG-augmented, tool-using conversation turn with streaming output.
func (a *Agent) Chat(ctx context.Context, req ChatRequest, onText llm.StreamFunc, onEvent func(ChatEvent)) (*ChatResult, error) {
	if onEvent == nil {
		onEvent = func(ChatEvent) {}
	}
	if req.History <= 0 {
		req.History = 16
	}
	// Retrieval and history load in parallel.
	var (
		wg      sync.WaitGroup
		hits    []SearchResult
		history []database.ChatMessage
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if strings.TrimSpace(req.Text) != "" {
			hits, _ = a.Search(ctx, req.Text, database.NodeFilter{}, 6)
		}
	}()
	go func() {
		defer wg.Done()
		history, _ = a.db.ChatHistory(ctx, req.Channel, req.History)
	}()
	wg.Wait()

	res := &ChatResult{}
	if len(hits) > 0 {
		var refs []map[string]any
		for _, h := range hits {
			res.Context = append(res.Context, h.Node)
			refs = append(refs, map[string]any{"id": h.Node.ID, "title": h.Node.Title, "type": h.Node.Type})
		}
		onEvent(ChatEvent{Type: "context", Data: refs})
	}

	msgs := make([]llm.Message, 0, len(history)+1)
	for _, h := range history {
		if h.Role == llm.RoleUser || h.Role == llm.RoleAssistant {
			msgs = append(msgs, llm.Message{Role: h.Role, Content: h.Content})
		}
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: req.Text, Parts: req.Parts})

	var tools []llm.Tool
	if !req.NoTools {
		tools = a.Tools()
	}
	system := a.systemPrompt(hits)
	var full strings.Builder
	emit := func(s string) error {
		full.WriteString(s)
		if onText != nil {
			return onText(s)
		}
		return nil
	}
	for round := 0; ; round++ {
		useTools := tools
		if round >= maxToolRounds {
			useTools = nil
		}
		resp, err := a.llm.Stream(ctx, req.Provider, llm.Request{Purpose: "chat", System: system, Messages: msgs, Tools: useTools}, emit)
		if err != nil {
			return nil, err
		}
		res.Provider, res.Model = resp.Provider, resp.Model
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		if len(resp.ToolCalls) == 0 || len(useTools) == 0 {
			break
		}
		if full.Len() > 0 && !strings.HasSuffix(full.String(), "\n") {
			_ = emit("\n")
		}
		msgs = append(msgs, resp.AssistantMessage())
		// Execute independent tool calls concurrently, preserving order.
		results := make([]string, len(resp.ToolCalls))
		var twg sync.WaitGroup
		for i, call := range resp.ToolCalls {
			onEvent(ChatEvent{Type: "tool_call", Data: map[string]any{"name": call.Name, "arguments": string(call.Arguments)}})
			twg.Add(1)
			go func(i int, call llm.ToolCall) {
				defer twg.Done()
				results[i] = a.ExecuteTool(ctx, call)
			}(i, call)
		}
		twg.Wait()
		for i, call := range resp.ToolCalls {
			onEvent(ChatEvent{Type: "tool_result", Data: map[string]any{"name": call.Name, "result": extract.Truncate(results[i], 400)}})
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: results[i]})
		}
	}
	res.Text = strings.TrimSpace(full.String())
	if req.Channel != "" {
		userText := req.Text
		if len(req.Parts) > 0 {
			userText += fmt.Sprintf(" [+%d anexo(s)]", len(req.Parts))
		}
		_ = a.db.AppendChat(ctx, req.Channel, llm.RoleUser, userText)
		_ = a.db.AppendChat(ctx, req.Channel, llm.RoleAssistant, res.Text)
	}
	return res, nil
}

// Ask is a one-shot, non-persisted question (used by schedulers).
func (a *Agent) Ask(ctx context.Context, purpose, system, prompt string) (string, error) {
	resp, err := a.llm.Complete(ctx, "", llm.Request{Purpose: purpose, System: system, Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}}, MaxTokens: 8000})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}
