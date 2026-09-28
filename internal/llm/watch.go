package llm

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Helpers for cmd/modelswatch: spot model ids on the providers' documentation
// pages that are newer than the curated catalogue.

var watchIDRe = map[string]*regexp.Regexp{
	"anthropic": regexp.MustCompile(`\bclaude-[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?\b`),
	"openai":    regexp.MustCompile(`\bgpt-[0-9](?:[a-z0-9.-]*[a-z0-9])?\b`),
	"gemini":    regexp.MustCompile(`\bgemini-[0-9](?:[a-z0-9.-]*[a-z0-9])?\b`),
}

var (
	watchSnapshotRe = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2}|\d{2}-\d{4}|\d{3,4}|v\d+)$`)
	watchVersionRe  = regexp.MustCompile(`^\d+(\.\d+)?$`)
	watchSkip       = []string{"embed", "tts", "audio", "realtime", "transcribe", "image", "search", "moderation", "computer-use", "robotics", "native", "live", "system-card", "-and-"}
)

// ExtractModelIDs returns the distinct model ids of provider mentioned in page.
func ExtractModelIDs(provider, page string) []string {
	re := watchIDRe[provider]
	if re == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range re.FindAllString(strings.ToLower(page), -1) {
		id = strings.TrimRight(id, ".-")
		if seen[id] || !strings.ContainsAny(id, "0123456789") {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// modelFamily splits an id into its variant family and numeric version:
// claude-opus-5-5 → ("opus", [5 5]), claude-3-5-haiku → ("haiku", [3 5]),
// gpt-6-astra → ("astra", [6]), gemini-3.1-pro-preview → ("pro", [3 1]).
func modelFamily(id string) (string, []int) {
	id = watchSnapshotRe.ReplaceAllString(id, "")
	family := ""
	var ver []int
	for _, p := range strings.Split(id, "-")[1:] {
		switch {
		case p == "":
		case watchVersionRe.MatchString(p):
			if len(ver) < 2 {
				for _, n := range strings.Split(p, ".") {
					v, _ := strconv.Atoi(n)
					ver = append(ver, v)
				}
			}
		case len(ver) == 0 && p[0] >= '0' && p[0] <= '9': // gpt-4o
			n, _ := strconv.Atoi(strings.TrimRightFunc(p, func(r rune) bool { return r < '0' || r > '9' }))
			ver = append(ver, n)
		case family == "":
			family = p
		}
	}
	return family, ver
}

func cmpVersion(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// NewModelCandidates filters ids found in the docs down to chat models that are
// not curated yet and at least as new as the oldest curated model of the same
// family (a family the catalogue lacks is compared with the provider's newest).
func (c *CuratedCatalog) NewModelCandidates(provider string, ids []string) []string {
	ignore := map[string]bool{}
	for _, id := range c.WatchIgnore {
		ignore[strings.ToLower(id)] = true
	}
	minFam := map[string][]int{}
	var newest []int
	for _, m := range c.Providers[provider].Models {
		ignore[strings.ToLower(m.ID)] = true
		f, v := modelFamily(strings.ToLower(m.ID))
		if cur, ok := minFam[f]; !ok || cmpVersion(v, cur) < 0 {
			minFam[f] = v
		}
		if cmpVersion(v, newest) > 0 {
			newest = v
		}
	}
	var out []string
	for _, id := range ids {
		if ignore[id] || watchSnapshotRe.MatchString(id) || slices.ContainsFunc(watchSkip, func(s string) bool { return strings.Contains(id, s) }) {
			continue
		}
		f, v := modelFamily(id)
		if len(v) == 0 {
			continue
		}
		floor, ok := minFam[f]
		if !ok {
			floor = newest
		}
		if cmpVersion(v, floor) >= 0 {
			out = append(out, id)
		}
	}
	return out
}

// MissingFromDocs lists curated models of provider that the page no longer mentions.
func (c *CuratedCatalog) MissingFromDocs(provider string, ids []string) []string {
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	var out []string
	for _, m := range c.Providers[provider].Models {
		if !found[strings.ToLower(m.ID)] {
			out = append(out, m.ID)
		}
	}
	return out
}
