package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/finance"
	"github.com/inakano89/second-brain/internal/profile"
)

func seedStatements(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()
	var txs []database.Transaction
	add := func(date string, amount float64, desc, cat string) {
		txs = append(txs, database.Transaction{Date: date, Amount: amount, Description: desc, Merchant: finance.MerchantKey(desc), Category: cat, Account: "a", Ref: fmt.Sprintf("%s|%s|%v", date, desc, amount)})
	}
	for _, m := range []string{"2026-06", "2026-07", "2026-08", "2026-09"} {
		add(m+"-05", 8000, "SALARIO", finance.CatIncome)
		add(m+"-06", -39.90, "NETFLIX.COM", finance.CatSubscription)
		add(m+"-07", -21.90, "SPOTIFY", finance.CatSubscription)
	}
	add("2026-08-10", -400, "SUPERMERCADO", "Mercado")
	add("2026-09-10", -900, "SUPERMERCADO", "Mercado")
	add("2026-09-15", -3000, "PAGAMENTO FATURA", finance.CatCardPayment)
	if _, err := db.InsertTransactions(ctx, txs); err != nil {
		t.Fatal(err)
	}
}

func TestFinanceDigestAndToolPolicy(t *testing.T) {
	ctx := context.Background()
	a, db := setupAgent(t, nil)
	if text, err := a.FinanceDigest(ctx, ""); err != nil || text != "" {
		t.Fatalf("empty = %q, %v", text, err)
	}
	seedStatements(t, db)
	// The Perfil tracks Netflix only.
	sub := profile.Item{Kind: "subscription", Title: "Netflix", Values: map[string]string{"price": "39,90"}}
	if err := a.profile.Save(ctx, &sub); err != nil {
		t.Fatal(err)
	}
	text, err := a.FinanceDigest(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Finanças de 09/2026", "Entradas R$ 8.000,00", "Gastos R$ 961,80", "Saldo R$ 7.038,20", "Mercado: R$ 900,00", "subiu R$ 500,00", "Spotify", "Ainda usa tudo isso?"} {
		if want == "Spotify" {
			want = "SPOTIFY (R$ 21,90/mês)"
		}
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "NETFLIX.COM (R$") {
		t.Errorf("tracked subscription flagged:\n%s", text)
	}
	if _, err := a.FinanceDigest(ctx, "setembro"); err == nil {
		t.Error("bad month should fail")
	}

	// The chat tool gives aggregates only, and only when the policy allows this model.
	res := a.ExecuteTool(ctx, toolCall(toolFinance, `{}`))
	if !strings.Contains(res, "sensíveis") || strings.Contains(res, "SALARIO") {
		t.Fatalf("basic policy with a cloud model = %s", res)
	}
	a.cfg.Update(map[string]string{"PROFILE_AI_ACCESS": "full"})
	res = a.ExecuteTool(ctx, toolCall(toolFinance, `{"month":"2026-09"}`))
	for _, want := range []string{`"month":"2026-09"`, `"spend":961.8`, `"not_in_profile":true`, `"category":"Mercado"`} {
		if !strings.Contains(res, want) {
			t.Errorf("tool result missing %s: %s", want, res)
		}
	}
	if strings.Contains(res, "PAGAMENTO FATURA") {
		t.Errorf("statement lines leaked: %s", res)
	}
	a.cfg.Update(map[string]string{"PROFILE_AI_ACCESS": "none"})
	if res := a.ExecuteTool(ctx, toolCall(toolFinance, `{}`)); !strings.Contains(res, "sensíveis") {
		t.Fatalf("none policy = %s", res)
	}
	if !isProfileTool(toolFinance) { // the turn is stored encrypted
		t.Error("finance turns must be private")
	}
}
