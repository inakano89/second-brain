package finance

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

// CategoryTotal is what was spent in one category.
type CategoryTotal struct {
	Category string  `json:"category"`
	Total    float64 `json:"total"` // positive reais
	Count    int     `json:"count"`
	Share    float64 `json:"share"` // of total spending, 0-100
}

// Summary totals a period.
type Summary struct {
	Income     float64         `json:"income"`
	Spend      float64         `json:"spend"`
	Net        float64         `json:"net"`
	Neutral    float64         `json:"neutral"` // transfers, card bill payments and investments moved
	Categories []CategoryTotal `json:"categories"`
	Lines      int             `json:"lines"`
}

// Summarize totals the lines: income (positive, non-neutral), spending (negative, non-neutral)
// and the spending per category, largest first. A refund reduces its category.
func Summarize(txs []database.Transaction) Summary {
	var s Summary
	byCat := map[string]*CategoryTotal{}
	for _, t := range txs {
		s.Lines++
		if IsNeutral(t.Category) {
			s.Neutral += math.Abs(t.Amount)
			continue
		}
		if t.Amount > 0 && (t.Category == CatIncome || t.Category == CatOtherIncome) {
			s.Income += t.Amount
			continue
		}
		c := byCat[t.Category]
		if c == nil {
			c = &CategoryTotal{Category: t.Category}
			byCat[t.Category] = c
		}
		c.Total -= t.Amount // spending is negative: refunds (positive) reduce it
		c.Count++
	}
	for _, c := range byCat {
		s.Spend += c.Total
		if c.Total > 0 {
			s.Categories = append(s.Categories, *c)
		}
	}
	s.Net = s.Income - s.Spend
	sort.Slice(s.Categories, func(i, j int) bool {
		if s.Categories[i].Total != s.Categories[j].Total {
			return s.Categories[i].Total > s.Categories[j].Total
		}
		return s.Categories[i].Category < s.Categories[j].Category
	})
	for i := range s.Categories {
		if s.Spend > 0 {
			s.Categories[i].Share = s.Categories[i].Total / s.Spend * 100
		}
	}
	return s
}

// CategoryChange compares one category between two periods.
type CategoryChange struct {
	Category string  `json:"category"`
	Now      float64 `json:"now"`
	Before   float64 `json:"before"`
	Delta    float64 `json:"delta"`
}

// Compare lists the categories whose spending changed the most between two periods.
func Compare(cur, prev Summary, limit int) []CategoryChange {
	before := map[string]float64{}
	for _, c := range prev.Categories {
		before[c.Category] = c.Total
	}
	seen := map[string]bool{}
	var out []CategoryChange
	for _, c := range cur.Categories {
		seen[c.Category] = true
		out = append(out, CategoryChange{Category: c.Category, Now: c.Total, Before: before[c.Category], Delta: c.Total - before[c.Category]})
	}
	for _, c := range prev.Categories {
		if !seen[c.Category] {
			out = append(out, CategoryChange{Category: c.Category, Before: c.Total, Delta: -c.Total})
		}
	}
	sort.Slice(out, func(i, j int) bool { return math.Abs(out[i].Delta) > math.Abs(out[j].Delta) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Recurring is a charge that repeats every month: a subscription, a bill, the rent.
type Recurring struct {
	Merchant  string  `json:"merchant"`
	Sample    string  `json:"sample"` // a description as it appears on the statement
	Category  string  `json:"category"`
	Amount    float64 `json:"amount"` // usual value, positive reais
	Latest    float64 `json:"latest"` // value of the last charge
	Months    int     `json:"months"` // distinct months charged
	First     string  `json:"first"`
	Last      string  `json:"last"`
	Active    bool    `json:"active"`    // charged in the last ~7 weeks
	Increased bool    `json:"increased"` // the latest charge is higher than usual
	Untracked bool    `json:"untracked"` // not among the Perfil's subscriptions and bills
	Yearly    float64 `json:"yearly"`    // cost per year
}

// FindRecurring detects monthly repeating charges: at least three months with about the same
// value (±15%), roughly one line per month. profileTitles are the titles of the Perfil's
// subscriptions and fixed bills, used to flag charges the user does not track there.
func FindRecurring(txs []database.Transaction, now time.Time, profileTitles []string) []Recurring {
	groups := map[string][]database.Transaction{}
	for _, t := range txs {
		if t.Amount >= 0 || t.Merchant == "" || IsNeutral(t.Category) {
			continue
		}
		groups[t.Merchant] = append(groups[t.Merchant], t)
	}
	var tracked [][]string
	for _, title := range profileTitles {
		tracked = append(tracked, tokens(fold(title)))
	}
	var out []Recurring
	for merchant, list := range groups {
		core := cluster(list)
		if core == nil {
			continue
		}
		months := map[string]bool{}
		var amounts []float64
		for _, t := range core {
			months[t.Date[:7]] = true
			amounts = append(amounts, -t.Amount)
		}
		if len(months) < 3 || float64(len(core)) > float64(len(months))*1.5 {
			continue
		}
		best := extendSeries(core, list)
		for _, t := range best {
			months[t.Date[:7]] = true
		}
		sort.Slice(best, func(i, j int) bool { return best[i].Date < best[j].Date })
		sort.Float64s(amounts)
		usual := amounts[len(amounts)/2]
		last := best[len(best)-1]
		lastDate, _ := time.Parse("2006-01-02", last.Date)
		r := Recurring{
			Merchant: merchant, Sample: last.Description, Category: last.Category, Amount: usual, Latest: -last.Amount, Months: len(months),
			First: best[0].Date, Last: last.Date, Active: now.Sub(lastDate) <= 50*24*time.Hour, Yearly: usual * 12,
			Increased: -last.Amount > usual*1.08,
		}
		r.Untracked = !isTracked(merchant, tracked)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		return out[i].Merchant < out[j].Merchant
	})
	return out
}

// cluster returns the largest set of lines with similar amounts (within 15% of one of them),
// preferring the one that spans the most months.
func cluster(list []database.Transaction) []database.Transaction {
	var best []database.Transaction
	bestMonths := 0
	for _, c := range list {
		center := -c.Amount
		var set []database.Transaction
		months := map[string]bool{}
		for _, t := range list {
			if v := -t.Amount; v >= center*0.85 && v <= center*1.15 {
				set = append(set, t)
				months[t.Date[:7]] = true
			}
		}
		if len(months) > bestMonths || (len(months) == bestMonths && len(set) < len(best)) {
			best, bestMonths = set, len(months)
		}
	}
	return best
}

func monthIndex(date string) int {
	t, err := time.Parse("2006-01", date[:7])
	if err != nil {
		return 0
	}
	return t.Year()*12 + int(t.Month())
}

// extendSeries adds the lines around a recurring core that follow the same monthly rhythm but at
// another price: consecutive months with a single charge each (a price increase or a cheaper plan).
func extendSeries(core, all []database.Transaction) []database.Transaction {
	have := map[int]bool{}
	for i := range core {
		have[monthIndex(core[i].Date)] = true
	}
	perMonth := map[int][]database.Transaction{}
	for _, t := range all {
		perMonth[monthIndex(t.Date)] = append(perMonth[monthIndex(t.Date)], t)
	}
	out := append([]database.Transaction(nil), core...)
	for changed := true; changed; {
		changed = false
		for m, list := range perMonth {
			if have[m] || len(list) != 1 || !(have[m-1] || have[m+1]) {
				continue
			}
			have[m] = true
			out = append(out, list[0])
			changed = true
		}
	}
	return out
}

func isTracked(merchant string, profile [][]string) bool {
	mt := tokens(merchant)
	for _, toks := range profile {
		for _, a := range mt {
			for _, b := range toks {
				if len(a) >= 4 && (a == b || strings.HasPrefix(b, a) || strings.HasPrefix(a, b) && len(b) >= 4) {
					return true
				}
			}
		}
	}
	return false
}

// MonthOf returns YYYY-MM of a date string.
func MonthOf(date string) string {
	if len(date) >= 7 {
		return date[:7]
	}
	return date
}

// MonthBounds returns the first and last day (YYYY-MM-DD) of a YYYY-MM month.
func MonthBounds(month string) (from, to string, ok bool) {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return "", "", false
	}
	return t.Format("2006-01-02"), t.AddDate(0, 1, -1).Format("2006-01-02"), true
}

// ShiftMonth moves a YYYY-MM month by n months (negative goes back).
func ShiftMonth(month string, n int) string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return ""
	}
	return t.AddDate(0, n, 0).Format("2006-01")
}

// PrevMonth returns the month before a YYYY-MM month.
func PrevMonth(month string) string { return ShiftMonth(month, -1) }
