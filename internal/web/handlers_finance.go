package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/finance"
)

type financeCategory struct {
	finance.CategoryTotal
	Delta   float64
	HasPrev bool
}

type financeView struct {
	Months     []string
	Month      string
	MonthLabel string
	Newer      string // month to the right (empty on the latest)
	Older      string
	Total      int // lines stored
	Cur, Prev  finance.Summary
	Categories []financeCategory
	Recurring  []finance.Recurring
	RecurTotal float64 // monthly cost of the active recurring charges
	Untracked  int
	Lines      []database.Transaction
	LinesTotal int
	Rules      string
}

const financeLines = 150

// monthLabel renders "2026-09" as "setembro de 2026".
func monthLabel(m string) string {
	t, err := time.Parse("2006-01", m)
	if err != nil {
		return m
	}
	return fmt.Sprintf("%s de %d", monthNames[t.Month()-1], t.Year())
}

// profileSubscriptions lists the titles of the Perfil's subscriptions and fixed bills.
func (s *Server) profileSubscriptions(ctx context.Context) []string {
	items, err := s.Agent.Profile().List(ctx, false)
	if err != nil {
		return nil
	}
	var titles []string
	for _, it := range items {
		if !it.Locked && (it.Kind == "subscription" || it.Kind == "bill") {
			titles = append(titles, it.Title)
		}
	}
	return titles
}

func (s *Server) financePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	months, err := s.DB.TransactionMonths(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	v := financeView{Months: months, Rules: s.Cfg.Get("FINANCE_RULES")}
	v.Total, _ = s.DB.CountTransactions(ctx)
	if len(months) > 0 {
		v.Month = r.URL.Query().Get("month")
		if !slices.Contains(months, v.Month) {
			v.Month = months[0]
		}
		i := slices.Index(months, v.Month)
		if i > 0 {
			v.Newer = months[i-1]
		}
		if i+1 < len(months) {
			v.Older = months[i+1]
		}
		v.MonthLabel = monthLabel(v.Month)
		from, to, _ := finance.MonthBounds(v.Month)
		pfrom, pto, _ := finance.MonthBounds(finance.PrevMonth(v.Month))
		histFrom, _, _ := finance.MonthBounds(finance.ShiftMonth(v.Month, -11))
		var cur, prev, hist []database.Transaction
		var titles []string
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() (err error) { cur, err = s.DB.Transactions(gctx, from, to, 0); return })
		g.Go(func() (err error) { prev, err = s.DB.Transactions(gctx, pfrom, pto, 0); return })
		g.Go(func() (err error) { hist, err = s.DB.Transactions(gctx, histFrom, to, 0); return })
		g.Go(func() error { titles = s.profileSubscriptions(gctx); return nil })
		if err := g.Wait(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		v.Cur, v.Prev = finance.Summarize(cur), finance.Summarize(prev)
		before := map[string]float64{}
		for _, c := range v.Prev.Categories {
			before[c.Category] = c.Total
		}
		for _, c := range v.Cur.Categories {
			b, ok := before[c.Category]
			v.Categories = append(v.Categories, financeCategory{CategoryTotal: c, Delta: c.Total - b, HasPrev: ok})
		}
		end, _ := time.Parse("2006-01-02", to)
		now := time.Now()
		if e := end.AddDate(0, 0, 1); e.Before(now) {
			now = e // an old month: judge "still active" as of its end
		}
		v.Recurring = finance.FindRecurring(hist, now, titles)
		for _, rc := range v.Recurring {
			if rc.Active {
				v.RecurTotal += rc.Latest
				if rc.Untracked {
					v.Untracked++
				}
			}
		}
		v.LinesTotal = len(cur)
		v.Lines = cur[:min(financeLines, len(cur))]
	}
	s.render(w, "financas", s.page(r, "Finanças", "financas", v))
}

func (s *Server) financeRecategorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	txs, err := s.DB.Transactions(ctx, "", "", 0)
	if err != nil {
		redirectFlash(w, r, "/financas", err.Error(), true)
		return
	}
	cat := finance.NewCategorizer(strings.TrimSpace(s.Cfg.Get("FINANCE_RULES")))
	changed := 0
	for _, t := range txs {
		if c := cat.Category(t); c != t.Category {
			if err := s.DB.SetTransactionCategory(ctx, t.ID, c); err != nil {
				redirectFlash(w, r, "/financas", err.Error(), true)
				return
			}
			changed++
		}
	}
	redirectFlash(w, r, "/financas", fmt.Sprintf("%d lançamento(s) recategorizado(s).", changed), false)
}

func (s *Server) financeRules(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/financas", err.Error(), true)
		return
	}
	if err := s.Cfg.Update(map[string]string{"FINANCE_RULES": strings.TrimSpace(r.FormValue("rules"))}); err != nil {
		redirectFlash(w, r, "/financas", err.Error(), true)
		return
	}
	s.financeRecategorize(w, r)
}

func (s *Server) financeClear(w http.ResponseWriter, r *http.Request) {
	n, err := s.DB.DeleteTransactions(r.Context(), "")
	if err != nil {
		redirectFlash(w, r, "/financas", err.Error(), true)
		return
	}
	s.log.Info("lançamentos de extrato apagados", "count", n)
	redirectFlash(w, r, "/financas", fmt.Sprintf("%d lançamento(s) apagado(s).", n), false)
}
