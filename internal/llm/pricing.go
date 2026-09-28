package llm

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Price is USD per 1M tokens.
type Price struct{ In, Out float64 }

// DefaultPricing is an editable estimate table (override with LLM_PRICING).
// Matching is by longest model-name prefix.
var DefaultPricing = map[string]Price{
	// Anthropic
	"claude-fable-5":   {10, 50},
	"claude-mythos-5":  {10, 50},
	"claude-opus-5-5":  {4, 20},
	"claude-opus-5":    {5, 25},
	"claude-opus-4-8":  {5, 25},
	"claude-opus-4-7":  {5, 25},
	"claude-opus-4-6":  {5, 25},
	"claude-opus-4-5":  {5, 25},
	"claude-opus-4":    {15, 75},
	"claude-sonnet-5":  {2, 10},
	"claude-sonnet-4":  {3, 15},
	"claude-haiku-4-5": {1, 5},
	"claude-3-5-haiku": {0.8, 4},
	// OpenAI
	"gpt-4o-mini":            {0.15, 0.60},
	"gpt-4o":                 {2.50, 10},
	"gpt-4.1-nano":           {0.10, 0.40},
	"gpt-4.1-mini":           {0.40, 1.60},
	"gpt-4.1":                {2, 8},
	"gpt-5-nano":             {0.05, 0.40},
	"gpt-5-mini":             {0.25, 2},
	"gpt-5":                  {1.25, 10},
	"o4-mini":                {1.10, 4.40},
	"text-embedding-3-small": {0.02, 0},
	"text-embedding-3-large": {0.13, 0},
	// Google
	"gemini-2.5-pro":        {1.25, 10},
	"gemini-2.5-flash-lite": {0.10, 0.40},
	"gemini-2.5-flash":      {0.30, 2.50},
	"gemini-2.0-flash":      {0.10, 0.40},
	"gemini-embedding":      {0.15, 0},
	// Local
	"local": {0, 0},
}

// PriceChange is a price that takes effect on a date (UTC).
type PriceChange struct {
	From  time.Time
	Price Price
}

// PricePlan is a model's price over time plus an optional long-prompt tier
// (e.g. Gemini charges more when the prompt exceeds 200k tokens).
type PricePlan struct {
	Base      Price
	Changes   []PriceChange // sorted by From
	LongAbove int           // input tokens; 0 = no long-prompt tier
	Long      Price
}

// At returns the price in effect at t for a prompt of inputTokens (0 = short).
func (pl PricePlan) At(t time.Time, inputTokens int) Price {
	if pl.LongAbove > 0 && inputTokens > pl.LongAbove {
		return pl.Long
	}
	p := pl.Base
	for _, c := range pl.Changes {
		if !t.Before(c.From) {
			p = c.Price
		}
	}
	return p
}

// Pricing resolves costs for model names.
type Pricing struct {
	table map[string]PricePlan
	keys  []string
}

// NewPricing merges defaults, curated catalogue prices and a JSON override
// {"prefix":[in,out]} (later sources win).
func NewPricing(override string, cat *CuratedCatalog) Pricing {
	t := map[string]PricePlan{}
	for k, v := range DefaultPricing {
		t[k] = PricePlan{Base: v}
	}
	if cat != nil {
		for k, v := range cat.pricePlans() {
			t[k] = v
		}
	}
	if strings.TrimSpace(override) != "" {
		var o map[string][2]float64
		if json.Unmarshal([]byte(override), &o) == nil {
			for k, v := range o {
				t[strings.ToLower(k)] = PricePlan{Base: Price{v[0], v[1]}}
			}
		}
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return Pricing{table: t, keys: keys}
}

// Plan returns the price plan for model (longest matching prefix).
func (p Pricing) Plan(provider, model string) (PricePlan, bool) {
	if provider == "ollama" || strings.HasPrefix(model, "local") {
		return PricePlan{}, true
	}
	m := strings.TrimPrefix(strings.ToLower(model), "models/")
	if i := strings.LastIndex(m, ":"); i >= 0 {
		m = m[i+1:]
	}
	for _, k := range p.keys {
		if strings.HasPrefix(m, k) {
			return p.table[k], true
		}
	}
	return PricePlan{}, false
}

// Lookup returns today's short-prompt price for model.
func (p Pricing) Lookup(provider, model string) (Price, bool) {
	pl, ok := p.Plan(provider, model)
	return pl.At(time.Now(), 0), ok
}

// Cost returns the USD estimate for usage on model (0 when the price is unknown).
func (p Pricing) Cost(provider, model string, u Usage) float64 {
	pl, _ := p.Plan(provider, model)
	pr := pl.At(time.Now(), u.InputTokens)
	return (float64(u.InputTokens)*pr.In + float64(u.OutputTokens)*pr.Out) / 1e6
}
