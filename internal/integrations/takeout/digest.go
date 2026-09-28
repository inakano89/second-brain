// Package takeout reads the Google Takeout files that have no API and no generic
// importer: My Activity (YouTube, Search, Maps, Chrome, Play, Gemini…), Chrome
// history, location history / Timeline, Maps saved places and Play Store lists.
// Files are recognised by content, so exports in any language work. Thousands of
// entries become a few notes: one per product and month, one per list.
package takeout

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Note is a digest ready to become a graph node.
type Note struct {
	Ref       string // stable key: "youtube-watch:2026-09", "play:install"…
	Title     string
	Content   string
	Summary   string
	Tags      []string
	CreatedAt time.Time
	Meta      map[string]any
}

// Collector accumulates Takeout files; Parse is safe for concurrent use.
type Collector struct{ c *collection }

// NewCollector groups entries by month in loc.
func NewCollector(loc *time.Location) *Collector {
	if loc == nil {
		loc = time.Local
	}
	return &Collector{c: newCollection(loc)}
}

var playHead = regexp.MustCompile(`^\s*\[\s*\{\s*"(install|libraryDoc|purchaseHistory|subscription|review|orderHistory)"\s*:`)

// Recognizes reports whether a JSON document (its first bytes) is Takeout data read here.
func Recognizes(head []byte) bool {
	h := string(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")))
	t := strings.TrimSpace(h)
	switch {
	case strings.HasPrefix(t, "[") && strings.Contains(h, `"header"`) && strings.Contains(h, `"time"`) && strings.Contains(h, `"products"`):
		return true // My Activity
	case strings.HasPrefix(t, "[") && strings.Contains(h, `"startTime"`) && strings.Contains(h, `"topCandidate"`):
		return true // Timeline (iOS)
	case strings.HasPrefix(t, "{") && (strings.Contains(h, `"Browser History"`) || strings.Contains(h, `"timelineObjects"`) ||
		strings.Contains(h, `"semanticSegments"`) || strings.Contains(h, `"FeatureCollection"`)):
		return true
	}
	return playHead.MatchString(t)
}

// Parse reads one JSON file; unknown content is ignored.
func (c *Collector) Parse(name string, r io.Reader) error {
	if err := c.c.parse(name, r); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// WarnHTML records that history was exported as HTML (once per collector).
func (c *Collector) WarnHTML(name string) { c.c.warnHTML(name) }

// Formats counts recognised files per format.
func (c *Collector) Formats() map[string]int {
	c.c.mu.Lock()
	defer c.c.mu.Unlock()
	out := make(map[string]int, len(c.c.formats))
	for k, v := range c.c.formats {
		out[k] = v
	}
	return out
}

// Warnings returns parse problems worth showing to the user.
func (c *Collector) Warnings() []string {
	c.c.mu.Lock()
	defer c.c.mu.Unlock()
	return append([]string(nil), c.c.warnings...)
}

// Items is the number of entries read.
func (c *Collector) Items() int {
	c.c.mu.Lock()
	defer c.c.mu.Unlock()
	return c.c.items
}

const maxDigestLines = 3000

var months = [...]string{"", "janeiro", "fevereiro", "março", "abril", "maio", "junho", "julho", "agosto", "setembro", "outubro", "novembro", "dezembro"}

var (
	mdText = strings.NewReplacer("[", "(", "]", ")", "\n", " ")
	mdURL  = strings.NewReplacer("(", "%28", ")", "%29", " ", "%20")
)

// Notes renders the digests: one note per stream and month, one per list.
func (c *Collector) Notes() []Note {
	col := c.c
	col.mu.Lock()
	defer col.mu.Unlock()
	var out []Note
	for _, st := range col.streams {
		byMonth := map[string][]item{}
		for _, it := range st.items {
			m := it.t.In(col.loc).Format("2006-01")
			byMonth[m] = append(byMonth[m], it)
		}
		for m, items := range byMonth {
			if n, ok := digestNote(st, m, items, col.loc); ok {
				out = append(out, n)
			}
		}
	}
	for _, l := range col.lists {
		out = append(out, listNoteOf(l))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// digestLines sorts a month and drops consecutive duplicates (reloads, repeated plays).
func digestLines(items []item, loc *time.Location) (lines []string, kept []item) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].t.Before(items[j].t) })
	for i, it := range items {
		if i > 0 {
			p := items[i-1]
			if p.text == it.text && p.url == it.url && it.t.Sub(p.t) < 10*time.Minute {
				continue
			}
		}
		kept = append(kept, it)
		line := "- " + it.t.In(loc).Format("02/01 15:04") + " · "
		text := mdText.Replace(strings.TrimSpace(it.text))
		if strings.HasPrefix(it.url, "http") {
			line += "[" + text + "](" + mdURL.Replace(it.url) + ")"
		} else {
			line += text
		}
		if it.extra != "" && !strings.Contains(it.text, it.extra) {
			line += " — " + mdText.Replace(it.extra)
		}
		lines = append(lines, line)
	}
	return lines, kept
}

func digestNote(st *stream, month string, items []item, loc *time.Location) (Note, bool) {
	start, err := time.ParseInLocation("2006-01", month, loc)
	if err != nil {
		return Note{}, false
	}
	lines, kept := digestLines(items, loc)
	if len(lines) == 0 {
		return Note{}, false
	}
	period := fmt.Sprintf("%s de %d", months[start.Month()], start.Year())
	summary := fmt.Sprintf("%d registros em %s.", len(kept), period)
	if top := topKeys(kept, 8); top != "" {
		summary += " Mais frequentes: " + top + "."
	}
	if len(lines) > maxDigestLines {
		lines = append(lines[:maxDigestLines], fmt.Sprintf("- … e mais %d registros", len(lines)-maxDigestLines))
	}
	return Note{
		Ref: st.kind + ":" + month, Title: st.label + " — " + period, Summary: summary,
		Content: summary + "\n\n" + strings.Join(lines, "\n"), Tags: st.tags, CreatedAt: start,
		Meta: map[string]any{"kind": st.kind, "month": month, "items": len(kept), "enriched": true},
	}, true
}

func listNoteOf(l *listNote) Note {
	seen := map[string]bool{}
	var lines []string
	for _, ln := range l.lines {
		if !seen[ln] {
			seen[ln] = true
			lines = append(lines, ln)
		}
	}
	sort.Strings(lines)
	summary := fmt.Sprintf("%d itens (Google Takeout).", len(lines))
	return Note{Ref: l.ref, Title: l.title, Summary: summary, Content: summary + "\n\n" + strings.Join(lines, "\n"), Tags: l.tags,
		Meta: map[string]any{"items": len(lines), "enriched": true}}
}
