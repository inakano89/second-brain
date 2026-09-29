package importer

import (
	"reflect"
	"regexp"
	"strings"
	"time"
)

// Contact text is made of "**Label:** value" lines (see the vCard reader) plus free text.
var contactLineRe = regexp.MustCompile(`^\*\*([^*:]+):\*\*\s*(.*)$`)

// listLabels hold several independent values; the other labels hold one value each.
var listLabels = map[string]bool{"E-mail": true, "Telefone": true}

// listMeta are the person meta keys that hold lists.
var listMeta = map[string]bool{"emails": true, "phones": true}

type contactText struct {
	labels []string
	values map[string]string
	free   string
}

func parseContact(s string) contactText {
	c := contactText{values: map[string]string{}}
	var free []string
	for ln := range strings.SplitSeq(s, "\n") {
		if m := contactLineRe.FindStringSubmatch(strings.TrimSpace(ln)); m != nil {
			label := strings.TrimSpace(m[1])
			if _, ok := c.values[label]; !ok {
				c.labels = append(c.labels, label)
				c.values[label] = strings.TrimSpace(m[2])
			}
			continue
		}
		free = append(free, ln)
	}
	c.free = strings.TrimSpace(strings.Join(free, "\n"))
	return c
}

// splitTop splits on commas that are not inside parentheses ("+55 11 9999 (home, voice)" stays whole).
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth = max(depth-1, 0)
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// mergeStrings unites two lists, the newer one first, without repeats (case-insensitive).
func mergeStrings(newer, older []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{newer, older} {
		for _, p := range list {
			p = strings.TrimSpace(p)
			if k := strings.ToLower(p); p != "" && !seen[k] {
				seen[k] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// mergeList joins comma-separated lists, the newer one first, without repeats.
func mergeList(newer, older string) string {
	return strings.Join(mergeStrings(splitTop(newer), splitTop(older)), ", ")
}

var beforeRe = regexp.MustCompile(`\s*_\(antes: .*\)_$`)

// splitBefore separates "Nova SA _(antes: Velha SA)_" into the current value and its note.
func splitBefore(v string) (cur, note string) {
	if loc := beforeRe.FindStringIndex(v); loc != nil {
		return v[:loc[0]], v[loc[0]:]
	}
	return v, ""
}

// mergeContact combines the text of two records of the same person. Where they disagree the
// newer record wins and the older value is kept as "(antes: …)"; phones and e-mails are united
// with the newer ones first; free text of the incoming record is appended when new.
func mergeContact(existing, incoming string, incomingNewer bool) string {
	ex, in := parseContact(existing), parseContact(incoming)
	newer, older := ex, in
	if incomingNewer {
		newer, older = in, ex
	}
	labels := append([]string(nil), newer.labels...)
	for _, l := range older.labels {
		if _, ok := newer.values[l]; !ok {
			labels = append(labels, l)
		}
	}
	var b strings.Builder
	for _, l := range labels {
		nv, ov := newer.values[l], older.values[l]
		ncur, _ := splitBefore(nv)
		ocur, _ := splitBefore(ov)
		var v string
		switch {
		case nv == "":
			v = ov
		case ov == "" || strings.EqualFold(ncur, ocur):
			v = nv // same value: keep the newer line, with its own "antes" note
		case listLabels[l]:
			v = mergeList(nv, ov)
		default:
			v = ncur + " _(antes: " + ocur + ")_"
		}
		b.WriteString("**" + l + ":** " + v + "\n")
	}
	free := ex.free
	if in.free != "" && !strings.Contains(ex.free, in.free) {
		free = strings.TrimSpace(free + "\n\n" + in.free)
	}
	if free != "" {
		b.WriteString("\n" + free + "\n")
	}
	return strings.TrimSpace(b.String())
}

func metaStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// mergePersonMeta folds src into dst: missing keys are copied; on a disagreement the newer side wins
// (lists are united, newer first). It reports whether dst changed.
func mergePersonMeta(dst, src map[string]any, srcNewer bool) bool {
	changed := false
	for k, sv := range src {
		dv, ok := dst[k]
		switch {
		case !ok || dv == nil || dv == "":
			dst[k] = sv
			changed = true
		case listMeta[k]:
			a, b := metaStrings(dv), metaStrings(sv)
			if srcNewer {
				a, b = b, a
			}
			merged := mergeStrings(a, b)
			if !reflect.DeepEqual(merged, metaStrings(dv)) {
				dst[k] = merged
				changed = true
			}
		case srcNewer && !reflect.DeepEqual(dv, sv) && sv != nil && sv != "" && !strings.HasPrefix(k, "import_"):
			dst[k] = sv
			changed = true
		}
	}
	return changed
}

// newerThan reports whether a is a more recent record than b; an unknown date never wins.
func newerThan(a, b time.Time) bool { return !a.IsZero() && a.After(b) }
