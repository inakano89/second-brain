package llm

import (
	"encoding/json"
	"sort"
	"strings"
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

// Pricing resolves costs for model names.
type Pricing struct {
	table map[string]Price
	keys  []string
}

// NewPricing merges defaults with a JSON override {"prefix":[in,out]}.
func NewPricing(override string) Pricing {
	t := map[string]Price{}
	for k, v := range DefaultPricing {
		t[k] = v
	}
	if strings.TrimSpace(override) != "" {
		var o map[string][2]float64
		if json.Unmarshal([]byte(override), &o) == nil {
			for k, v := range o {
				t[k] = Price{v[0], v[1]}
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

// Cost returns the USD estimate for usage on model.
func (p Pricing) Cost(provider, model string, u Usage) float64 {
	if provider == "ollama" || strings.HasPrefix(model, "local") {
		return 0
	}
	m := strings.TrimPrefix(strings.ToLower(model), "models/")
	if i := strings.LastIndex(m, ":"); i >= 0 {
		m = m[i+1:]
	}
	for _, k := range p.keys {
		if strings.HasPrefix(m, k) {
			pr := p.table[k]
			return (float64(u.InputTokens)*pr.In + float64(u.OutputTokens)*pr.Out) / 1e6
		}
	}
	return 0
}
