package llm

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

//go:embed models.json
var builtinCatalogJSON []byte

// CatalogModel is one curated model.
type CatalogModel struct {
	ID    string    `json:"id"`
	Name  string    `json:"name,omitempty"`
	Price []float64 `json:"price,omitempty"` // USD per 1M tokens: [input, output]
	Note  string    `json:"note,omitempty"`
}

// CatalogProvider lists a provider's curated models and its recommended default.
type CatalogProvider struct {
	Default string         `json:"default"`
	Models  []CatalogModel `json:"models"`
}

// CuratedCatalog is the maintained model list (internal/llm/models.json). It ships
// inside the binary and is refreshed from the repository, so installs follow new
// provider models without waiting for a release. Bump Revision on every change.
type CuratedCatalog struct {
	Revision    int                        `json:"revision"`
	Updated     string                     `json:"updated"`
	Sources     map[string]string          `json:"sources,omitempty"`
	Providers   map[string]CatalogProvider `json:"providers"`
	WatchIgnore []string                   `json:"watch_ignore,omitempty"`
}

var (
	catalogIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,119}$`)
	datedSuffixRe = regexp.MustCompile(`^(\d{8}|\d{4}-\d{2}-\d{2})$`)
)

// ParseCuratedCatalog decodes and validates a catalogue document.
func ParseCuratedCatalog(b []byte) (*CuratedCatalog, error) {
	var c CuratedCatalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("catálogo de modelos inválido: %w", err)
	}
	if c.Revision <= 0 {
		return nil, errors.New("catálogo de modelos sem revisão")
	}
	if len(c.Providers) == 0 {
		return nil, errors.New("catálogo de modelos vazio")
	}
	for p, cp := range c.Providers {
		if DefaultModelKey(p) == "" {
			return nil, fmt.Errorf("catálogo: provedor desconhecido %q", p)
		}
		if len(cp.Models) == 0 {
			return nil, fmt.Errorf("catálogo: %s sem modelos", p)
		}
		seen := map[string]bool{}
		for _, m := range cp.Models {
			if !catalogIDRe.MatchString(m.ID) {
				return nil, fmt.Errorf("catálogo: id de modelo inválido %q", m.ID)
			}
			if seen[m.ID] {
				return nil, fmt.Errorf("catálogo: modelo repetido %q", m.ID)
			}
			seen[m.ID] = true
			if len(m.Price) != 0 && (len(m.Price) != 2 || m.Price[0] < 0 || m.Price[1] < 0) {
				return nil, fmt.Errorf("catálogo: preço inválido em %q", m.ID)
			}
		}
		if !seen[cp.Default] {
			return nil, fmt.Errorf("catálogo: padrão de %s (%q) não está na lista", p, cp.Default)
		}
	}
	return &c, nil
}

var builtinCatalog = sync.OnceValue(func() *CuratedCatalog {
	c, err := ParseCuratedCatalog(builtinCatalogJSON)
	if err != nil {
		panic(err)
	}
	return c
})

// BuiltinCatalog returns the catalogue embedded in this binary.
func BuiltinCatalog() *CuratedCatalog { return builtinCatalog() }

// legacyCatalog is what installs used before the curated catalogue existed
// (the old schema defaults); it is the baseline for the first sync.
var legacyCatalog = &CuratedCatalog{Revision: 1, Updated: "2026-09-01", Providers: map[string]CatalogProvider{
	"anthropic": {Default: "claude-opus-5", Models: []CatalogModel{{ID: "claude-opus-5"}, {ID: "claude-sonnet-5"}, {ID: "claude-haiku-4-5"}}},
	"openai":    {Default: "gpt-4o-mini", Models: []CatalogModel{{ID: "gpt-4o-mini"}, {ID: "gpt-4.1"}}},
	"gemini":    {Default: "gemini-2.5-flash", Models: []CatalogModel{{ID: "gemini-2.5-flash"}, {ID: "gemini-2.5-pro"}}},
}}

// Specs lists "provider:model" in provider display order.
func (c *CuratedCatalog) Specs() []string {
	var out []string
	for _, p := range ProviderNames {
		for _, m := range c.Providers[p].Models {
			out = append(out, p+":"+m.ID)
		}
	}
	return out
}

// Has reports whether spec ("provider:model") is curated.
func (c *CuratedCatalog) Has(spec string) bool {
	_, ok := c.Lookup(spec)
	return ok
}

// Lookup returns the curated entry for spec.
func (c *CuratedCatalog) Lookup(spec string) (CatalogModel, bool) {
	p, id, _ := strings.Cut(spec, ":")
	for _, m := range c.Providers[p].Models {
		if m.ID == id {
			return m, true
		}
	}
	return CatalogModel{}, false
}

// DefaultFor returns the recommended default model of provider ("" if not curated).
func (c *CuratedCatalog) DefaultFor(provider string) string {
	return c.Providers[provider].Default
}

func (c *CuratedCatalog) prices() map[string]Price {
	out := map[string]Price{}
	for _, cp := range c.Providers {
		for _, m := range cp.Models {
			if len(m.Price) == 2 {
				out[strings.ToLower(m.ID)] = Price{m.Price[0], m.Price[1]}
			}
		}
	}
	return out
}

// CatalogPlan is the set of .env edits that moves an install from one catalogue
// revision to the next.
type CatalogPlan struct {
	Changes  map[string]string
	Added    []string
	Removed  []string
	Defaults []string
}

// Summary describes the plan for logs and notifications.
func (p CatalogPlan) Summary() []string {
	var out []string
	for _, s := range p.Added {
		out = append(out, "+ "+s)
	}
	for _, s := range p.Removed {
		out = append(out, "− "+s)
	}
	for _, s := range p.Defaults {
		out = append(out, "★ "+s)
	}
	return out
}

// PlanCatalog computes the edits from prev to next while keeping user choices:
//   - models dropped from the curated list are removed; models the user added
//     themselves stay, and curated models the user removed are not re-added;
//   - a provider default follows the new recommendation unless the user picked
//     a different model that is still available;
//   - routes and council entries pointing at removed models fall back to the
//     provider's default.
func PlanCatalog(get func(string) string, prev, next *CuratedCatalog) CatalogPlan {
	plan := CatalogPlan{Changes: map[string]string{}}
	prevHas := map[string]bool{}
	for _, s := range prev.Specs() {
		prevHas[s] = true
	}
	// Removed models are replaced by a dated successor when the catalogue has one
	// (claude-haiku-4-5 → claude-haiku-4-5-20251001), else by the provider default.
	removed := map[string]bool{}
	repl := map[string]string{}
	for s := range prevHas {
		if !next.Has(s) {
			removed[s] = true
			p, _, _ := strings.Cut(s, ":")
			repl[s] = p
			for _, n := range next.Specs() {
				if suffix, ok := strings.CutPrefix(n, s+"-"); ok && datedSuffixRe.MatchString(suffix) {
					repl[s] = n
					break
				}
			}
		}
	}
	current := ParseCatalog(get("LLM_MODELS"))
	have := map[string]bool{}
	for _, s := range current {
		have[s] = true
		if removed[s] {
			plan.Removed = append(plan.Removed, s)
		}
	}
	var final []string
	for _, s := range next.Specs() {
		switch {
		case have[s]:
			final = append(final, s)
		case !prevHas[s]:
			final = append(final, s)
			plan.Added = append(plan.Added, s)
		}
	}
	for _, s := range current {
		if !removed[s] && !next.Has(s) {
			final = append(final, s)
		}
	}
	if strings.Join(final, ",") != strings.Join(current, ",") {
		plan.Changes["LLM_MODELS"] = strings.Join(final, ",")
	}
	for _, p := range ProviderNames {
		nd := next.DefaultFor(p)
		if nd == "" {
			continue
		}
		key := DefaultModelKey(p)
		cur := strings.TrimSpace(get(key))
		if cur == nd {
			continue
		}
		spec := p + ":" + cur
		if cur != "" && cur != prev.DefaultFor(p) && removed[spec] && repl[spec] != p {
			_, nd, _ = strings.Cut(repl[spec], ":")
		}
		if cur == "" || cur == prev.DefaultFor(p) || removed[spec] {
			plan.Changes[key] = nd
			plan.Defaults = append(plan.Defaults, fmt.Sprintf("%s: %s → %s", ProviderLabel(p), cur, nd))
		}
	}
	DetachSpecs(get, repl, plan.Changes)
	return plan
}

// DetachSpecs rewrites task routes and council settings that point at removed
// "provider:model" specs, using repl (removed spec → "provider" or a new spec).
func DetachSpecs(get func(string) string, repl map[string]string, changes map[string]string) {
	if len(repl) == 0 {
		return
	}
	detach := func(v string) string {
		if r, ok := repl[v]; ok {
			return r
		}
		return v
	}
	for _, t := range Tasks {
		k := RouteKey(t.Key)
		if v := strings.TrimSpace(get(k)); detach(v) != v {
			changes[k] = detach(v)
		}
	}
	if v := strings.TrimSpace(get("LLM_COUNCIL_JUDGE")); detach(v) != v {
		changes["LLM_COUNCIL_JUDGE"] = detach(v)
	}
	var members []string
	seen, changed := map[string]bool{}, false
	for _, m := range strings.Split(get("LLM_COUNCIL_MEMBERS"), ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		d := detach(m)
		changed = changed || d != m
		if !seen[d] {
			seen[d] = true
			members = append(members, d)
		}
	}
	if changed {
		changes["LLM_COUNCIL_MEMBERS"] = strings.Join(members, ",")
	}
}
