package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/profile"
)

// ChatRequest is one user turn.
type ChatRequest struct {
	Channel  string // "web", "tg:<chat>"
	Provider string // "provider[:model]", "council", "auto" or empty (task route)
	Text     string
	Parts    []llm.Part
	NoTools  bool
	History  int // turns of history to load (default 16)
}

// ChatEvent notifies the UI about retrieval and tool activity.
type ChatEvent struct {
	Type string `json:"type"` // context | council_start | council | tool_call | tool_result
	Data any    `json:"data"`
}

// ChatResult summarises a completed turn.
type ChatResult struct {
	Text     string
	Provider string
	Model    string
	Usage    llm.Usage
	Context  []database.Node
	Council  []llm.Opinion
}

const maxToolRounds = 6

func (a *Agent) systemPrompt(ctxNodes []SearchResult) string {
	loc := a.cfg.Location()
	var b strings.Builder
	fmt.Fprintf(&b, `Você é o assistente pessoal do Second Brain "%s".
Agora: %s (%s).
Responda de forma direta e acionável, no idioma do usuário. Use o contexto recuperado quando relevante e cite notas como [[Título]].
Use as ferramentas para buscar mais informações, criar ou editar notas/tarefas/pessoas/eventos ou executar ações quando o usuário pedir. Para editar, busque o nó antes (search_brain/get_node) e use update_node; nunca invente IDs.
Você pode criar, editar e apagar (lixeira) notas, tarefas, pessoas, eventos, ligações e itens do Perfil, mas SÓ com autorização. Se a mensagem atual do usuário pede explicitamente a alteração ("adicione o telefone da Ana", "apague a nota X"), faça e envie user_requested=true. Se a alteração for ideia sua ou o pedido for ambíguo (qual pessoa? qual valor?), não altere: pergunte em uma frase o que mudaria e espere o "sim"; só então chame a ferramenta com user_requested=true. Nunca use user_requested=true por conta própria.`,
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
		memory  string
	)
	wg.Add(3)
	go func() { defer wg.Done(); memory = a.memoryPrompt(ctx) }()
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

	purpose := "chat"
	provider := strings.TrimSpace(req.Provider)
	if strings.HasPrefix(req.Channel, "tg:") {
		purpose = "telegram"
	}
	if provider == "" {
		provider = a.llm.Route(llm.TaskForPurpose(purpose))
	}

	// Turns that read the personal profile are encrypted and only replayed to a model
	// allowed to see them (local, or PROFILE_AI_ACCESS=full).
	access := a.profile.Access()
	replayPrivate := access == profile.AccessFull || (access == profile.AccessBasic && a.llm.IsLocalSpec(provider))
	msgs := make([]llm.Message, 0, len(history)+1)
	for _, h := range history {
		if h.Role != llm.RoleUser && h.Role != llm.RoleAssistant {
			continue
		}
		if h.Private {
			if !replayPrivate {
				continue
			}
			text, err := a.profile.Open(ctx, h.Content)
			if err != nil {
				continue
			}
			h.Content = text
		}
		msgs = append(msgs, llm.Message{Role: h.Role, Content: h.Content})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: req.Text, Parts: req.Parts})

	var tools []llm.Tool
	if !req.NoTools {
		tools = a.Tools()
	}
	system := a.systemPrompt(hits) + memory
	if provider == llm.SpecCouncil {
		// Members debate without tools; the moderator synthesizes and may call tools.
		cc := a.llm.CouncilSetup()
		onEvent(ChatEvent{Type: "council_start", Data: map[string]any{"members": cc.Members, "judge": cc.Judge, "rounds": cc.Rounds}})
		ops, _, err := a.llm.Deliberate(ctx, llm.Request{Purpose: purpose, System: system, Messages: msgs},
			func(op llm.Opinion) { onEvent(ChatEvent{Type: "council", Data: op}) })
		switch {
		case err == nil:
			system += "\n\n" + llm.SynthesisPrompt(ops)
			res.Council = ops
		case !errors.Is(err, llm.ErrCouncilTooSmall):
			return nil, err
		}
		provider = cc.Judge
	}
	var full strings.Builder
	private := false
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
		resp, err := a.llm.Stream(ctx, provider, llm.Request{Purpose: purpose, System: system, Messages: msgs, Tools: useTools}, emit)
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
			if isProfileTool(call.Name) {
				private = true
			}
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
		a.saveTurn(ctx, req.Channel, userText, res.Text, private)
	}
	return res, nil
}

// saveTurn persists a chat turn; private turns are encrypted (and dropped if that fails).
func (a *Agent) saveTurn(ctx context.Context, channel, user, answer string, private bool) {
	if private {
		u, err1 := a.profile.Seal(ctx, user)
		v, err2 := a.profile.Seal(ctx, answer)
		if err := errors.Join(err1, err2); err != nil {
			a.log.Warn("conversa com dados do perfil não foi salva (cofre indisponível)", "err", err)
			return
		}
		user, answer = u, v
	}
	_ = a.db.AppendChatTurn(ctx, channel, llm.RoleUser, user, private)
	_ = a.db.AppendChatTurn(ctx, channel, llm.RoleAssistant, answer, private)
}

// ChatHistory returns a channel history for display, private turns decrypted.
func (a *Agent) ChatHistory(ctx context.Context, channel string, limit int) ([]database.ChatMessage, error) {
	hist, err := a.db.ChatHistory(ctx, channel, limit)
	if err != nil {
		return nil, err
	}
	for i := range hist {
		if hist[i].Private {
			if text, err := a.profile.Open(ctx, hist[i].Content); err == nil {
				hist[i].Content = text
			} else {
				hist[i].Content = "🔒 (mensagem protegida: chave do cofre ausente)"
			}
		}
	}
	return hist, nil
}

// Ask is a one-shot, non-persisted question (used by schedulers).
func (a *Agent) Ask(ctx context.Context, purpose, system, prompt string) (string, error) {
	resp, err := a.llm.Complete(ctx, "", llm.Request{Purpose: purpose, System: system, Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}}, MaxTokens: 8000})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}
