package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/inakano89/second-brain/internal/llm"
)

// Chat tools added by feature modules (memories, retrospectives, CRM, travel, finance, shopping…).
// Tools that change data are listed in mutatingTools and need the user's go-ahead.

const (
	toolOnThisDay     = "on_this_day"
	toolYearReview    = "year_review"
	toolPersonBrief   = "person_brief"
	toolStaleContacts = "stale_contacts"
	toolHealthRoutine = "health_routine"
	toolShoppingList  = "shopping_list"
	toolShoppingEdit  = "shopping_edit"
)

func init() {
	mutatingTools[toolYearReview] = true
	mutatingTools[toolShoppingEdit] = true
}

// extraTools returns the feature tools available in the current configuration.
func (a *Agent) extraTools() []llm.Tool {
	tools := []llm.Tool{
		{Name: toolOnThisDay, Description: "Lembranças: o que o usuário registrou no mesmo dia (mês e dia) em anos anteriores (1, 3 e 5 anos atrás por padrão).",
			Parameters: obj(map[string]any{"date": str("Dia de referência AAAA-MM-DD (padrão hoje)")})},
		{Name: toolYearReview, Description: "Gera e salva a RETROSPECTIVA de um ano (temas, pessoas, decisões, números e pendências) como um insight. Leva alguns segundos.",
			Parameters: obj(map[string]any{"year": integer("Ano, ex.: 2025 (padrão: o ano passado)")})},
		{Name: toolPersonBrief, Description: "CRM pessoal: resumo de uma pessoa antes de uma conversa ou reunião: última interação, interações recentes, tarefas abertas ligadas a ela e próximos eventos.",
			Parameters: obj(map[string]any{"person": str("Nome da pessoa ou #id do nó")}, "person")},
		{Name: toolStaleContacts, Description: "CRM pessoal: pessoas com quem o usuário não fala há mais tempo que o esperado (CRM_STALE_DAYS; contatos frequentes ou com a tag crm).",
			Parameters: obj(map[string]any{"limit": integer("Máximo (padrão 10)")})},
		{Name: toolHealthRoutine, Description: "Saúde × rotina: compara dias cheios de reuniões com dias leves e mostra o efeito no sono e na recuperação da noite seguinte (dados do relógio + agenda).",
			Parameters: obj(map[string]any{"days": integer("Janela em dias (padrão 30, máx. 180)")})},
		{Name: toolShoppingList, Description: "Mostra a lista de compras (casa).", Parameters: obj(map[string]any{})},
		{Name: toolShoppingEdit, Description: "Altera a lista de compras: add (adicionar itens), remove, check (marcar como comprado), uncheck, clear_done (limpar comprados) ou clear_all.",
			Parameters: obj(map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"add", "remove", "check", "uncheck", "clear_done", "clear_all"}},
				"items":  strList("Itens (ex.: [\"leite\", \"pão\"]); vazio em clear_*"),
			}, "action")},
		{Name: toolFinance, Description: "Finanças: resumo de um mês do extrato importado (entradas, gastos por categoria, comparação com o mês anterior e cobranças recorrentes que não estão no Perfil). Só devolve totais, nunca os lançamentos.",
			Parameters: obj(map[string]any{"month": str("Mês AAAA-MM (padrão: o mais recente com dados)")})},
		{Name: toolTravel, Description: "Dossiê de viagem: agenda no período, reservas encontradas no Gmail, arquivos e notas do destino, pendências e, do Perfil, documentos que vencem, vacinas e estoque de medicação. Informe o destino e/ou as datas; sem eles usa a próxima viagem do Perfil.",
			Parameters: obj(map[string]any{"destination": str("Destino, ex.: Lisboa"), "from": str("Ida AAAA-MM-DD"), "to": str("Volta AAAA-MM-DD")})},
	}
	return tools
}

// execExtraTool runs a tool of extraTools; handled is false for unknown names.
func (a *Agent) execExtraTool(ctx context.Context, name string, args toolArgs) (res any, handled bool, err error) {
	loc := a.cfg.Location()
	switch name {
	case toolOnThisDay:
		day := time.Now().In(loc)
		if s := args.str("date"); s != "" {
			t, perr := time.ParseInLocation("2006-01-02", s, loc)
			if perr != nil {
				return nil, true, fmt.Errorf("data inválida: %s", s)
			}
			day = t
		}
		mem, err := a.OnThisDay(ctx, day)
		if err != nil {
			return nil, true, err
		}
		out := make([]map[string]any, 0, len(mem))
		for _, m := range mem {
			briefs := make([]nodeBrief, len(m.Nodes))
			for i := range m.Nodes {
				briefs[i] = brief(&m.Nodes[i], loc)
			}
			out = append(out, map[string]any{"years_ago": m.YearsAgo, "date": m.Date.Format("2006-01-02"), "items": briefs})
		}
		return out, true, nil
	case toolPersonBrief:
		res, err := a.briefTool(ctx, args.str("person"))
		return res, true, err
	case toolStaleContacts:
		list, err := a.StaleContacts(ctx, time.Now(), clampLimit(args.int("limit"), 10, 30))
		if err != nil {
			return nil, true, err
		}
		out := make([]map[string]any, 0, len(list))
		for _, c := range list {
			out = append(out, map[string]any{"id": c.Person.ID, "name": c.Person.Title, "last_interaction": c.Last.In(loc).Format("2006-01-02"),
				"days_since": int(time.Since(c.Last).Hours() / 24), "interactions": c.Count})
		}
		return out, true, nil
	case toolTravel:
		res, err := a.travelForChat(ctx, args)
		return res, true, err
	case toolFinance:
		res, err := a.financeForChat(ctx, args.str("month"))
		return res, true, err
	case toolHealthRoutine:
		_, text, err := a.HealthRoutine(ctx, int(args.int("days")))
		if err != nil {
			return nil, true, err
		}
		if text == "" {
			text = "Sem dados de agenda suficientes no período."
		}
		return map[string]any{"report": text}, true, nil
	case toolShoppingList:
		list, err := a.ShoppingList(ctx)
		return map[string]any{"items": list}, true, err
	case toolShoppingEdit:
		list, err := a.ShoppingEdit(ctx, args.str("action"), args.strs("items"))
		if err != nil {
			return nil, true, err
		}
		return map[string]any{"items": list}, true, nil
	case toolYearReview:
		year := int(args.int("year"))
		if year == 0 {
			year = time.Now().In(loc).Year() - 1
		}
		n, text, err := a.YearReview(ctx, year)
		if err != nil {
			return nil, true, err
		}
		if n == nil {
			return map[string]any{"review": text, "note": "não foi salva: você apagou esta retrospectiva antes com “não trazer de volta”"}, true, nil
		}
		return map[string]any{"saved": brief(n, loc), "review": text}, true, nil
	}
	return nil, false, nil
}
