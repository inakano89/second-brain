package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// Tools returns the function-calling catalogue available in the current configuration.
func (a *Agent) Tools() []llm.Tool {
	tools := []llm.Tool{
		{Name: "search_brain", Description: "Busca híbrida (texto + semântica) nas notas, tarefas, pessoas, eventos, insights, artigos e métricas do Second Brain.",
			Parameters: obj(map[string]any{
				"query": str("Termos ou pergunta"),
				"type":  map[string]any{"type": "string", "description": "Filtro opcional de tipo", "enum": database.NodeTypes},
				"limit": integer("Máximo de resultados (padrão 8)"),
			}, "query")},
		{Name: "get_node", Description: "Lê o conteúdo completo de um nó e seus vizinhos no grafo.",
			Parameters: obj(map[string]any{"id": integer("ID do nó")}, "id")},
		{Name: "create_note", Description: "Cria uma nota no Second Brain (será auto-taggeada e auto-linkada).",
			Parameters: obj(map[string]any{
				"title":   str("Título"),
				"content": str("Conteúdo em Markdown; use [[Título]] para linkar notas"),
				"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags opcionais"},
				"type":    map[string]any{"type": "string", "enum": []string{"note", "insight", "article"}, "description": "Tipo (padrão note)"},
			}, "title", "content")},
		{Name: "create_task", Description: "Cria uma tarefa pendente.",
			Parameters: obj(map[string]any{
				"title":   str("Ação a fazer"),
				"details": str("Detalhes opcionais"),
				"due":     str("Prazo opcional: YYYY-MM-DD ou YYYY-MM-DDTHH:MM"),
			}, "title")},
		{Name: "complete_task", Description: "Marca uma tarefa como concluída.",
			Parameters: obj(map[string]any{"id": integer("ID da tarefa")}, "id")},
		{Name: "list_tasks", Description: "Lista tarefas por status.",
			Parameters: obj(map[string]any{"status": map[string]any{"type": "string", "enum": []string{"open", "done"}}})},
		{Name: "link_nodes", Description: "Cria uma aresta entre dois nós do grafo.",
			Parameters: obj(map[string]any{"source_id": integer("Origem"), "target_id": integer("Destino"), "relation": str("Relação, ex: related, part_of, mentions")}, "source_id", "target_id")},
		{Name: "health_summary", Description: "Retorna métricas de saúde (sono, recuperação, FC repouso, passos) dos últimos N dias.",
			Parameters: obj(map[string]any{"days": integer("Dias (padrão 7)")})},
	}
	if a.calendar() != nil {
		tools = append(tools,
			llm.Tool{Name: "list_calendar_events", Description: "Lista eventos do Google Calendar num intervalo.",
				Parameters: obj(map[string]any{"from": str("Início YYYY-MM-DD (padrão hoje)"), "to": str("Fim YYYY-MM-DD (padrão +7 dias)")})},
			llm.Tool{Name: "create_calendar_event", Description: "Cria evento no Google Calendar.",
				Parameters: obj(map[string]any{
					"summary": str("Título"), "start": str("Início YYYY-MM-DDTHH:MM (hora local)"), "end": str("Fim opcional YYYY-MM-DDTHH:MM"),
					"location": str("Local opcional"), "description": str("Descrição opcional"),
				}, "summary", "start")},
		)
	}
	if a.google(GoogleGmail) != nil {
		tools = append(tools,
			llm.Tool{Name: "search_email", Description: "Busca e-mails no Gmail do usuário (sintaxe de busca do Gmail: from:, to:, subject:, after:AAAA/MM/DD, has:attachment, label:…).",
				Parameters: obj(map[string]any{"query": str("Consulta no formato do Gmail"), "limit": integer("Máximo de resultados (padrão 10, máx. 25)")}, "query")},
			llm.Tool{Name: "read_email", Description: "Lê o conteúdo completo de um e-mail do Gmail pelo ID.",
				Parameters: obj(map[string]any{"id": str("ID do e-mail (de search_email)")}, "id")},
		)
	}
	if a.google(GoogleDrafts) != nil {
		tools = append(tools, llm.Tool{Name: "create_email_draft",
			Description: "Cria um RASCUNHO no Gmail. Nunca envia: o usuário revisa e envia pelo Gmail. Para responder, informe reply_to com o ID do e-mail original.",
			Parameters: obj(map[string]any{
				"to": str("Destinatários (vírgula); opcional ao responder"), "cc": str("Cópia opcional"),
				"subject": str("Assunto; opcional ao responder"), "body": str("Texto do e-mail"), "reply_to": str("ID do e-mail respondido (opcional)"),
			}, "body")})
	}
	if a.google(GoogleDrive) != nil {
		tools = append(tools,
			llm.Tool{Name: "search_drive", Description: "Busca arquivos no Google Drive por nome ou conteúdo.",
				Parameters: obj(map[string]any{"query": str("Termos"), "limit": integer("Máximo de resultados (padrão 10)")}, "query")},
			llm.Tool{Name: "read_drive_file", Description: "Lê o texto de um arquivo do Google Drive (Docs, Planilhas, Apresentações, PDF, DOCX, texto).",
				Parameters: obj(map[string]any{"id": str("ID do arquivo (de search_drive)")}, "id")},
		)
	}
	if a.google(GoogleContacts) != nil {
		tools = append(tools, llm.Tool{Name: "search_contacts", Description: "Busca pessoas nos Contatos do Google (inclui quem já trocou e-mails com o usuário).",
			Parameters: obj(map[string]any{"query": str("Nome, e-mail, telefone ou empresa")}, "query")})
	}
	if acts := a.actions.List(); a.actions.Enabled() && len(acts) > 0 {
		var names []string
		var desc strings.Builder
		desc.WriteString("Executa uma ação de automação permitida no host. Ações disponíveis:\n")
		for _, x := range acts {
			names = append(names, x.Name)
			fmt.Fprintf(&desc, "- %s: %s\n", x.Name, x.Description)
		}
		tools = append(tools, llm.Tool{Name: "run_action", Description: desc.String(),
			Parameters: obj(map[string]any{
				"name":  map[string]any{"type": "string", "enum": names},
				"input": str("Parâmetro opcional substituído em {{input}}"),
			}, "name")})
	}
	return tools
}

type toolArgs map[string]any

func (t toolArgs) str(k string) string {
	if v, ok := t[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (t toolArgs) int(k string) int64 {
	switch v := t[k].(type) {
	case float64:
		return int64(v)
	case string:
		var n int64
		fmt.Sscan(v, &n)
		return n
	}
	return 0
}

func (t toolArgs) strs(k string) []string {
	var out []string
	if arr, ok := t[k].([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func jsonResult(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return extract.Truncate(string(b), 12000)
}

type nodeBrief struct {
	ID      int64    `json:"id"`
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Summary string   `json:"summary,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Status  string   `json:"status,omitempty"`
	Due     string   `json:"due,omitempty"`
	Created string   `json:"created"`
}

func brief(n *database.Node, loc *time.Location) nodeBrief {
	b := nodeBrief{ID: n.ID, Type: n.Type, Title: n.Title, Summary: extract.Truncate(n.Summary, 300), Tags: n.Tags, Status: n.Status, Created: n.CreatedAt.In(loc).Format("2006-01-02 15:04")}
	if n.DueAt != nil {
		b.Due = n.DueAt.In(loc).Format("2006-01-02 15:04")
	}
	return b
}

// ExecuteTool runs one tool call and returns a JSON string result.
func (a *Agent) ExecuteTool(ctx context.Context, call llm.ToolCall) string {
	var args toolArgs
	_ = json.Unmarshal(call.Arguments, &args)
	if args == nil {
		args = toolArgs{}
	}
	res, err := a.execTool(ctx, call.Name, args)
	if err != nil {
		a.log.Warn("ferramenta falhou", "tool", call.Name, "err", err)
		return jsonResult(map[string]string{"error": err.Error()})
	}
	a.log.Info("ferramenta executada", "tool", call.Name)
	return jsonResult(res)
}

func (a *Agent) execTool(ctx context.Context, name string, args toolArgs) (any, error) {
	loc := a.cfg.Location()
	switch name {
	case "search_brain":
		f := database.NodeFilter{}
		if t := args.str("type"); database.ValidType(t) {
			f.Types = []string{t}
		}
		limit := int(args.int("limit"))
		if limit <= 0 || limit > 25 {
			limit = 8
		}
		hits, err := a.Search(ctx, args.str("query"), f, limit)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(hits))
		for _, h := range hits {
			out = append(out, map[string]any{"node": brief(&h.Node, loc), "snippet": h.Snippet})
		}
		return out, nil
	case "get_node":
		n, err := a.db.GetNode(ctx, args.int("id"))
		if err != nil {
			return nil, err
		}
		links, _ := a.db.Neighbors(ctx, n.ID)
		var neigh []map[string]any
		for _, l := range links {
			neigh = append(neigh, map[string]any{"id": l.Node.ID, "title": l.Node.Title, "type": l.Node.Type, "relation": l.Relation})
		}
		return map[string]any{"node": brief(n, loc), "content": extract.Truncate(n.Content, 8000), "links": neigh}, nil
	case "create_note":
		typ := args.str("type")
		if typ != database.TypeInsight && typ != database.TypeArticle {
			typ = database.TypeNote
		}
		n, _, err := a.Ingest(ctx, IngestInput{Type: typ, Title: args.str("title"), Content: args.str("content"), Tags: args.strs("tags"), Source: "agent", Enrich: true})
		if err != nil {
			return nil, err
		}
		return map[string]any{"created": brief(n, loc)}, nil
	case "create_task":
		var due *time.Time
		if d := args.str("due"); d != "" {
			if t, err := parseLocal(d, loc); err == nil {
				due = &t
			}
		}
		n, _, err := a.Ingest(ctx, IngestInput{Type: database.TypeTask, Title: args.str("title"), Content: args.str("details"), DueAt: due, Source: "agent", Enrich: true})
		if err != nil {
			return nil, err
		}
		return map[string]any{"created": brief(n, loc)}, nil
	case "complete_task":
		n, err := a.db.GetNode(ctx, args.int("id"))
		if err != nil {
			return nil, err
		}
		if n.Type != database.TypeTask {
			return nil, errors.New("o nó não é uma tarefa")
		}
		if err := a.db.SetStatus(ctx, n.ID, database.StatusDone); err != nil {
			return nil, err
		}
		return map[string]any{"completed": n.ID, "title": n.Title}, nil
	case "list_tasks":
		status := args.str("status")
		if status != database.StatusDone {
			status = database.StatusOpen
		}
		nodes, err := a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeTask}, Status: status, Order: "due", Limit: 50})
		if err != nil {
			return nil, err
		}
		out := make([]nodeBrief, len(nodes))
		for i := range nodes {
			out[i] = brief(&nodes[i], loc)
		}
		return out, nil
	case "link_nodes":
		rel := args.str("relation")
		if err := a.db.AddEdge(ctx, args.int("source_id"), args.int("target_id"), rel, 1); err != nil {
			return nil, err
		}
		return map[string]any{"linked": true}, nil
	case "health_summary":
		days := int(args.int("days"))
		if days <= 0 || days > 90 {
			days = 7
		}
		to := time.Now().In(loc)
		ms, err := a.db.MetricsRange(ctx, to.AddDate(0, 0, -days).Format("2006-01-02"), to.Format("2006-01-02"))
		if err != nil {
			return nil, err
		}
		return ms, nil
	case "list_calendar_events":
		now := time.Now().In(loc)
		from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		to := from.AddDate(0, 0, 7)
		if s := args.str("from"); s != "" {
			if t, err := parseLocal(s, loc); err == nil {
				from = t
			}
		}
		if s := args.str("to"); s != "" {
			if t, err := parseLocal(s, loc); err == nil {
				to = t.Add(24 * time.Hour)
			}
		}
		return a.EventsBetween(ctx, from, to)
	case "create_calendar_event":
		start, err := parseLocal(args.str("start"), loc)
		if err != nil {
			return nil, err
		}
		end := start.Add(time.Hour)
		if s := args.str("end"); s != "" {
			if t, err := parseLocal(s, loc); err == nil && t.After(start) {
				end = t
			}
		}
		ev, _, err := a.CreateEvent(ctx, CalendarEvent{Summary: args.str("summary"), Start: start, End: end, Location: args.str("location"), Description: args.str("description")})
		if err != nil {
			return nil, err
		}
		return ev, nil
	case "search_email", "read_email", "create_email_draft", "search_drive", "read_drive_file", "search_contacts":
		return a.execGoogleTool(ctx, name, args)
	case "run_action":
		out, err := a.actions.Run(ctx, args.str("name"), args.str("input"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"output": out}, nil
	}
	return nil, fmt.Errorf("ferramenta desconhecida: %s", name)
}

func clampLimit(v int64, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(int(v), max)
}

func (a *Agent) execGoogleTool(ctx context.Context, name string, args toolArgs) (any, error) {
	service := map[string]string{
		"search_email": GoogleGmail, "read_email": GoogleGmail, "create_email_draft": GoogleDrafts,
		"search_drive": GoogleDrive, "read_drive_file": GoogleDrive, "search_contacts": GoogleContacts,
	}[name]
	g := a.google(service)
	if g == nil {
		return nil, fmt.Errorf("serviço Google %q não está conectado ou autorizado", service)
	}
	switch name {
	case "search_email":
		return g.SearchEmail(ctx, args.str("query"), clampLimit(args.int("limit"), 10, 25))
	case "read_email":
		e, err := g.ReadEmail(ctx, args.str("id"))
		if err != nil {
			return nil, err
		}
		e.Body = extract.Truncate(e.Body, 8000)
		return e, nil
	case "create_email_draft":
		return g.CreateDraft(ctx, EmailDraft{To: args.str("to"), Cc: args.str("cc"), Subject: args.str("subject"), Body: args.str("body"), ReplyTo: args.str("reply_to")})
	case "search_drive":
		return g.SearchDrive(ctx, args.str("query"), clampLimit(args.int("limit"), 10, 25))
	case "read_drive_file":
		f, text, err := g.ReadDriveFile(ctx, args.str("id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"file": f, "text": extract.Truncate(text, 10000)}, nil
	case "search_contacts":
		return g.SearchContacts(ctx, args.str("query"), 10)
	}
	return nil, fmt.Errorf("ferramenta desconhecida: %s", name)
}
