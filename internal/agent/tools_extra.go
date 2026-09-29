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
	toolOnThisDay  = "on_this_day"
	toolYearReview = "year_review"
)

func init() {
	mutatingTools[toolYearReview] = true
}

// extraTools returns the feature tools available in the current configuration.
func (a *Agent) extraTools() []llm.Tool {
	tools := []llm.Tool{
		{Name: toolOnThisDay, Description: "Lembranças: o que o usuário registrou no mesmo dia (mês e dia) em anos anteriores (1, 3 e 5 anos atrás por padrão).",
			Parameters: obj(map[string]any{"date": str("Dia de referência AAAA-MM-DD (padrão hoje)")})},
		{Name: toolYearReview, Description: "Gera e salva a RETROSPECTIVA de um ano (temas, pessoas, decisões, números e pendências) como um insight. Leva alguns segundos.",
			Parameters: obj(map[string]any{"year": integer("Ano, ex.: 2025 (padrão: o ano passado)")})},
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
	case toolYearReview:
		year := int(args.int("year"))
		if year == 0 {
			year = time.Now().In(loc).Year() - 1
		}
		n, text, err := a.YearReview(ctx, year)
		if err != nil {
			return nil, true, err
		}
		return map[string]any{"saved": brief(n, loc), "review": text}, true, nil
	}
	return nil, false, nil
}
