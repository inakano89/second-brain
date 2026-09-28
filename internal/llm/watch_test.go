package llm

import (
	"slices"
	"testing"
)

func TestModelsWatchCandidates(t *testing.T) {
	cat := BuiltinCatalog()
	cases := []struct {
		provider, page string
		want           []string
	}{
		{"anthropic", `<td>claude-opus-5-5</td><td>claude-opus-6</td> claude-sonnet-5-1 claude-sonnet-4-6 claude-opus-5
			claude-haiku-4-5 claude-haiku-4-5-20251001 claude-3-5-haiku-20241022 claude-mythos-5-1 anthropic.claude-opus-5-5-v1:0
			claude-code claude-fable-5 claude-fable-5-1 claude-opus-5-5-system-card claude-fable-and-mythos-5-1 claude-haiku-4-5-20251001-v1`,
			[]string{"claude-opus-6", "claude-sonnet-5-1"}},
		{"openai", `gpt-6-astra gpt-6-sol gpt-6-nova gpt-6.1-luna gpt-5.5 gpt-4o-mini gpt-4.1-2025-04-14 gpt-6-astra-realtime gpt-7`,
			[]string{"gpt-6-nova", "gpt-6.1-luna", "gpt-7"}},
		{"gemini", `gemini-3.8-flash gemini-3.5-flash gemini-3.5-pro gemini-3.1-pro-preview gemini-2.5-flash-preview-09-2025
			gemini-3.8-flash-image gemini-embedding-001 gemini-4-flash gemini-3.8-live`, []string{"gemini-3.5-pro", "gemini-4-flash"}},
	}
	for _, c := range cases {
		got := cat.NewModelCandidates(c.provider, ExtractModelIDs(c.provider, c.page))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v want %v", c.provider, got, c.want)
		}
	}
	if miss := cat.MissingFromDocs("openai", ExtractModelIDs("openai", "gpt-6-astra gpt-6-sol")); !slices.Equal(miss, []string{"gpt-6-luna"}) {
		t.Errorf("missing = %v", miss)
	}
}
