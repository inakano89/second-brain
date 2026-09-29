package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/finance"
	"github.com/inakano89/second-brain/internal/profile"
)

const toolFinance = "finance_summary"

func brl(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	whole, cents, _ := strings.Cut(fmt.Sprintf("%.2f", v), ".")
	var parts []string
	for len(whole) > 3 {
		parts = append([]string{whole[len(whole)-3:]}, parts...)
		whole = whole[:len(whole)-3]
	}
	parts = append([]string{whole}, parts...)
	out := "R$ " + strings.Join(parts, ".") + "," + cents
	if neg {
		out = "-" + out
	}
	return out
}

// financeData loads what the summaries need for a month (YYYY-MM; empty = the latest with data).
func (a *Agent) financeData(ctx context.Context, month string) (m string, cur, prev finance.Summary, rec []finance.Recurring, err error) {
	months, err := a.db.TransactionMonths(ctx)
	if err != nil || len(months) == 0 {
		return "", cur, prev, nil, err
	}
	m = month
	if m == "" {
		m = months[0]
	}
	from, to, ok := finance.MonthBounds(m)
	if !ok {
		return "", cur, prev, nil, fmt.Errorf("mês inválido: %q (use AAAA-MM)", month)
	}
	pfrom, pto, _ := finance.MonthBounds(finance.PrevMonth(m))
	hfrom, _, _ := finance.MonthBounds(finance.ShiftMonth(m, -11))
	txs, err := a.db.Transactions(ctx, from, to, 0)
	if err != nil {
		return "", cur, prev, nil, err
	}
	ptxs, err := a.db.Transactions(ctx, pfrom, pto, 0)
	if err != nil {
		return "", cur, prev, nil, err
	}
	hist, err := a.db.Transactions(ctx, hfrom, to, 0)
	if err != nil {
		return "", cur, prev, nil, err
	}
	var titles []string
	if items, err := a.profile.List(ctx, false); err == nil {
		for _, it := range items {
			if !it.Locked && (it.Kind == "subscription" || it.Kind == "bill") {
				titles = append(titles, it.Title)
			}
		}
	}
	end, _ := time.Parse("2006-01-02", to)
	now := time.Now()
	if e := end.AddDate(0, 0, 1); e.Before(now) {
		now = e
	}
	return m, finance.Summarize(txs), finance.Summarize(ptxs), finance.FindRecurring(hist, now, titles), nil
}

// FinanceDigest writes the monthly summary for the user's chat (Telegram): totals, biggest
// categories, what changed and recurring charges missing from the Perfil. It never reaches a model.
func (a *Agent) FinanceDigest(ctx context.Context, month string) (string, error) {
	m, cur, prev, rec, err := a.financeData(ctx, month)
	if err != nil || m == "" {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "💰 *Finanças de %s*\n", m[5:]+"/"+m[:4])
	fmt.Fprintf(&b, "Entradas %s · Gastos %s · Saldo %s\n", brl(cur.Income), brl(cur.Spend), brl(cur.Net))
	if prev.Spend > 0 {
		d := (cur.Spend - prev.Spend) / prev.Spend * 100
		fmt.Fprintf(&b, "Gastos %+.0f%% em relação ao mês anterior (%s).\n", d, brl(prev.Spend))
	}
	for i, c := range cur.Categories {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "• %s: %s (%.0f%%)\n", c.Category, brl(c.Total), c.Share)
	}
	for _, ch := range finance.Compare(cur, prev, 2) {
		if prev.Spend > 0 && ch.Delta > 50 && ch.Before > 0 {
			fmt.Fprintf(&b, "📈 %s subiu %s (de %s para %s)\n", ch.Category, brl(ch.Delta), brl(ch.Before), brl(ch.Now))
		}
	}
	var forgotten, raised []string
	for _, r := range rec {
		if !r.Active {
			continue
		}
		if r.Untracked {
			forgotten = append(forgotten, fmt.Sprintf("%s (%s/mês)", oneLine(r.Sample), brl(r.Latest)))
		}
		if r.Increased {
			raised = append(raised, fmt.Sprintf("%s: %s → %s", oneLine(r.Sample), brl(r.Amount), brl(r.Latest)))
		}
	}
	if len(forgotten) > 0 {
		fmt.Fprintf(&b, "\n🔁 Cobranças recorrentes que não estão no Perfil: %s. Ainda usa tudo isso?\n", strings.Join(forgotten[:min(6, len(forgotten))], "; "))
	}
	if len(raised) > 0 {
		fmt.Fprintf(&b, "⬆️ Reajustes: %s\n", strings.Join(raised[:min(4, len(raised))], "; "))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// financeForChat answers the chat tool with aggregates only (no statement lines), and only when
// the personal-data policy lets this model read them: like sensitive Perfil items, that needs a
// local model or PROFILE_AI_ACCESS=full.
func (a *Agent) financeForChat(ctx context.Context, month string) (any, error) {
	access := a.profile.Access()
	local := a.llm.IsLocal("chat") && a.llm.IsLocal("telegram")
	if access == profile.AccessNone || (access == profile.AccessBasic && !local) {
		return map[string]any{"error": "dados financeiros são sensíveis: o usuário só libera para modelo local ou com PROFILE_AI_ACCESS=full. Veja a página Finanças."}, nil
	}
	m, cur, prev, rec, err := a.financeData(ctx, month)
	if err != nil {
		return nil, err
	}
	if m == "" {
		return map[string]any{"error": "nenhum extrato importado (página Importar → Extrato bancário)"}, nil
	}
	out := map[string]any{"month": m, "income": cur.Income, "spend": cur.Spend, "net": cur.Net, "categories": cur.Categories,
		"previous_month": map[string]any{"month": finance.PrevMonth(m), "spend": prev.Spend}, "changes": finance.Compare(cur, prev, 6)}
	var recurring []map[string]any
	for _, r := range rec {
		recurring = append(recurring, map[string]any{"name": r.Sample, "category": r.Category, "usual": r.Amount, "latest": r.Latest, "per_year": r.Yearly,
			"active": r.Active, "price_increased": r.Increased, "not_in_profile": r.Untracked})
	}
	out["recurring"] = recurring
	return out, nil
}
