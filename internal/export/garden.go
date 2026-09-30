package export

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/mdhtml"
)

// Digital garden: the notes tagged with the publishing tag (#publico by default) become a static
// website: an index, one page per note with its links and backlinks, one page per tag and an Atom
// feed. Only notes, insights and articles that carry the tag are published. Links to anything else
// are shown as plain text, so nothing private leaks through a [[link]].

// GardenOpts configures the site.
type GardenOpts struct {
	Title   string
	Tag     string // publishing tag, without '#'
	BaseURL string // public address of the site, for the feed (optional)
	Loc     *time.Location
}

// GardenOptionsFrom builds the options from the configuration (get reads a setting).
func GardenOptionsFrom(get func(string) string, loc *time.Location) GardenOpts {
	tag := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(get("GARDEN_TAG")), "#"))
	if tag == "" {
		tag = "publico"
	}
	return GardenOpts{Title: get("BRAIN_NAME"), Tag: tag, BaseURL: strings.TrimRight(strings.TrimSpace(get("GARDEN_URL")), "/"), Loc: loc}
}

// GardenPage is one published note.
type GardenPage struct {
	ID    int64
	Title string
	Path  string
}

// GardenReport says what was published.
type GardenReport struct {
	Pages []GardenPage
	Tags  int
}

var gardenTypes = []string{database.TypeNote, database.TypeInsight, database.TypeArticle}

// gardenSlug turns a title into a file name: lower case, no accents, hyphens.
func gardenSlug(s string) string {
	s = strings.ToLower(norm.NFD.String(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if r := []rune(out); len(r) > 60 {
		out = strings.Trim(string(r[:60]), "-")
	}
	if out == "" {
		out = "nota"
	}
	return out
}

const gardenCSS = `:root{--bg:#fbfbfa;--fg:#1f2328;--muted:#656d76;--accent:#6e56cf;--border:#e2e5ea}
@media (prefers-color-scheme:dark){:root{--bg:#15171a;--fg:#e6e8eb;--muted:#9aa3ad;--accent:#a08bff;--border:#2a2f36}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:17px/1.65 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main,header,footer{max-width:46rem;margin:0 auto;padding:0 1rem}header{padding-top:1.5rem;padding-bottom:.5rem;border-bottom:1px solid var(--border)}
header a{color:var(--fg);text-decoration:none;font-weight:700;font-size:1.2rem}a{color:var(--accent)}h1{line-height:1.25;margin:1.4rem 0 .3rem}
.meta{color:var(--muted);font-size:.9rem}.tags a{margin-right:.5rem;font-size:.9rem}ul.list{list-style:none;padding:0}ul.list li{padding:.6rem 0;border-bottom:1px solid var(--border)}
ul.list small{display:block;color:var(--muted)}blockquote{margin:1rem 0;padding:.1rem 1rem;border-left:3px solid var(--border);color:var(--muted)}
pre{background:color-mix(in srgb,var(--fg) 7%,transparent);padding:.8rem;overflow:auto;border-radius:6px}code{font-size:.92em}
table.md{border-collapse:collapse}table.md td{border:1px solid var(--border);padding:.3rem .6rem}ul.checklist{list-style:none;padding-left:.5rem}
section.links{margin-top:2.5rem;padding-top:1rem;border-top:1px solid var(--border)}footer{color:var(--muted);font-size:.85rem;padding-top:2rem;padding-bottom:3rem}`

func gardenPageHTML(o GardenOpts, root, title, desc, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>`, html.EscapeString(title))
	if desc != "" {
		fmt.Fprintf(&b, `<meta name="description" content="%s">`, html.EscapeString(desc))
	}
	fmt.Fprintf(&b, `<link rel="stylesheet" href="%sstyle.css"><link rel="alternate" type="application/atom+xml" href="%sfeed.xml"></head><body>`, root, root)
	fmt.Fprintf(&b, `<header><a href="%sindex.html">%s</a></header><main>%s</main><footer>Jardim digital feito com Second Brain.</footer></body></html>`, root, html.EscapeString(o.Title), body)
	return b.String()
}

// GardenFiles renders the whole site in memory: path → content.
func GardenFiles(ctx context.Context, db *database.DB, o GardenOpts) (map[string][]byte, GardenReport, error) {
	if o.Loc == nil {
		o.Loc = time.Local
	}
	if o.Tag == "" {
		o.Tag = "publico"
	}
	nodes, err := db.ListNodes(ctx, database.NodeFilter{Types: gardenTypes, Tag: o.Tag, Limit: 5000, Order: "date"})
	if err != nil {
		return nil, GardenReport{}, err
	}
	// Slugs and title lookup.
	slugOf := map[int64]string{}
	byTitle := map[string]int64{}
	used := map[string]bool{}
	for _, n := range nodes {
		base := gardenSlug(n.Title)
		slug := base
		for i := 2; used[slug]; i++ {
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		used[slug] = true
		slugOf[n.ID] = slug
		if _, ok := byTitle[strings.ToLower(strings.TrimSpace(n.Title))]; !ok {
			byTitle[strings.ToLower(strings.TrimSpace(n.Title))] = n.ID
		}
	}
	nodeByID := map[int64]*database.Node{}
	for i := range nodes {
		nodeByID[nodes[i].ID] = &nodes[i]
	}

	// Links between published notes: [[wiki-links]] in the text and edges in the graph.
	related := map[int64]map[int64]bool{}
	back := map[int64]map[int64]bool{}
	link := func(from, to int64) {
		if from == to {
			return
		}
		if related[from] == nil {
			related[from] = map[int64]bool{}
		}
		related[from][to] = true
		if back[to] == nil {
			back[to] = map[int64]bool{}
		}
		back[to][from] = true
	}
	for _, n := range nodes {
		for _, m := range wikiTargets(n.Content) {
			if id, ok := byTitle[strings.ToLower(m)]; ok {
				link(n.ID, id)
			}
		}
	}
	if len(nodes) > 0 {
		rows, err := db.QueryContext(ctx, `SELECT source_id, target_id FROM edges WHERE relation <> 'shares_tags'`)
		if err != nil {
			return nil, GardenReport{}, err
		}
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				rows.Close()
				return nil, GardenReport{}, err
			}
			if nodeByID[a] != nil && nodeByID[b] != nil {
				link(a, b)
				link(b, a)
			}
		}
		rows.Close()
	}

	wiki := func(target, label string) string {
		if id, ok := byTitle[strings.ToLower(strings.TrimSpace(target))]; ok {
			return `<a href="` + slugOf[id] + `.html">` + label + `</a>`
		}
		return label // not published: plain text
	}
	files := map[string][]byte{"style.css": []byte(gardenCSS), ".nojekyll": nil}
	tagPages := map[string][]int64{}
	dateStr := func(n *database.Node) string {
		if n.DateUnknown() {
			return ""
		}
		return n.CreatedAt.In(o.Loc).Format("02/01/2006")
	}
	listItem := func(prefix string, n *database.Node) string {
		s := fmt.Sprintf(`<li><a href="%s%s.html">%s</a>`, prefix, slugOf[n.ID], html.EscapeString(n.Title))
		var sub []string
		if d := dateStr(n); d != "" {
			sub = append(sub, d)
		}
		if t := strings.TrimSpace(n.Summary); t != "" {
			sub = append(sub, html.EscapeString(truncateRunes(oneLineText(t), 160)))
		}
		if len(sub) > 0 {
			s += "<small>" + strings.Join(sub, " · ") + "</small>"
		}
		return s + "</li>"
	}
	var rep GardenReport
	for i := range nodes {
		n := &nodes[i]
		var tags []string
		for _, t := range n.Tags {
			if t != o.Tag {
				tags = append(tags, t)
				tagPages[t] = append(tagPages[t], n.ID)
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<article><h1>%s</h1>", html.EscapeString(n.Title))
		var meta []string
		if d := dateStr(n); d != "" {
			meta = append(meta, d)
		}
		if u, _ := n.Meta["url"].(string); strings.HasPrefix(u, "http") {
			meta = append(meta, `<a href="`+html.EscapeString(u)+`" rel="noopener">fonte</a>`)
		}
		if len(meta) > 0 {
			b.WriteString(`<p class="meta">` + strings.Join(meta, " · ") + "</p>")
		}
		if len(tags) > 0 {
			b.WriteString(`<p class="tags">`)
			for _, t := range tags {
				fmt.Fprintf(&b, `<a href="../tags/%s.html">#%s</a> `, gardenSlug(t), html.EscapeString(t))
			}
			b.WriteString("</p>")
		}
		b.WriteString(mdhtml.Render(n.Content, wiki))
		b.WriteString("</article>")
		var refs []int64
		for id := range related[n.ID] {
			refs = append(refs, id)
		}
		for id := range back[n.ID] {
			if !related[n.ID][id] {
				refs = append(refs, id)
			}
		}
		if len(refs) > 0 {
			sort.Slice(refs, func(a, c int) bool {
				return strings.ToLower(nodeByID[refs[a]].Title) < strings.ToLower(nodeByID[refs[c]].Title)
			})
			b.WriteString(`<section class="links"><h2>Notas ligadas</h2><ul class="list">`)
			for _, id := range refs {
				b.WriteString(listItem("", nodeByID[id]))
			}
			b.WriteString("</ul></section>")
		}
		path := "notas/" + slugOf[n.ID] + ".html"
		files[path] = []byte(gardenPageHTML(o, "../", n.Title+" · "+o.Title, truncateRunes(oneLineText(firstNonEmptyText(n.Summary, n.Content)), 160), b.String()))
		rep.Pages = append(rep.Pages, GardenPage{ID: n.ID, Title: n.Title, Path: path})
	}

	// Index, tags and feed.
	var idx strings.Builder
	fmt.Fprintf(&idx, "<h1>%s</h1>", html.EscapeString(o.Title))
	if len(nodes) == 0 {
		fmt.Fprintf(&idx, `<p class="meta">Nenhuma nota publicada ainda. Marque uma nota com <b>#%s</b>.</p>`, html.EscapeString(o.Tag))
	} else {
		fmt.Fprintf(&idx, `<p class="meta">%d notas</p><ul class="list">`, len(nodes))
		for i := range nodes {
			idx.WriteString(listItem("notas/", &nodes[i]))
		}
		idx.WriteString("</ul>")
	}
	var tagNames []string
	for t := range tagPages {
		tagNames = append(tagNames, t)
	}
	sort.Strings(tagNames)
	if len(tagNames) > 0 {
		idx.WriteString(`<section class="links"><h2>Assuntos</h2><p class="tags">`)
		for _, t := range tagNames {
			fmt.Fprintf(&idx, `<a href="tags/%s.html">#%s</a> <span class="meta">(%d)</span> `, gardenSlug(t), html.EscapeString(t), len(tagPages[t]))
		}
		idx.WriteString("</p></section>")
	}
	files["index.html"] = []byte(gardenPageHTML(o, "", o.Title, "", idx.String()))
	tagSlugs := map[string][]string{}
	for _, t := range tagNames {
		tagSlugs[gardenSlug(t)] = append(tagSlugs[gardenSlug(t)], t)
	}
	for slug, names := range tagSlugs { // "Ideias" and "ideias" share a file
		var ids []int64
		seen := map[int64]bool{}
		for _, t := range names {
			for _, id := range tagPages[t] {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<h1>#%s</h1><ul class=\"list\">", html.EscapeString(names[0]))
		for i := range nodes {
			if seen[nodes[i].ID] {
				b.WriteString(listItem("../notas/", &nodes[i]))
			}
		}
		b.WriteString("</ul>")
		files["tags/"+slug+".html"] = []byte(gardenPageHTML(o, "../", "#"+names[0]+" · "+o.Title, "", b.String()))
	}
	rep.Tags = len(tagSlugs)
	if o.BaseURL != "" {
		files["feed.xml"] = []byte(gardenFeed(o, nodes, slugOf))
	}
	return files, rep, nil
}

func gardenFeed(o GardenOpts, nodes []database.Node, slugOf map[int64]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><feed xmlns="http://www.w3.org/2005/Atom">`)
	fmt.Fprintf(&b, "<title>%s</title><id>%s/</id><link href=\"%s/\"/>", xmlEscape(o.Title), xmlEscape(o.BaseURL), xmlEscape(o.BaseURL))
	updated := time.Time{}
	for i := range nodes {
		if !nodes[i].DateUnknown() && nodes[i].CreatedAt.After(updated) {
			updated = nodes[i].CreatedAt
		}
	}
	if updated.IsZero() {
		updated = time.Now()
	}
	fmt.Fprintf(&b, "<updated>%s</updated>", updated.UTC().Format(time.RFC3339))
	for i := range nodes {
		if i >= 30 {
			break
		}
		n := &nodes[i]
		u := o.BaseURL + "/notas/" + url.PathEscape(slugOf[n.ID]) + ".html"
		at := n.CreatedAt
		fmt.Fprintf(&b, "<entry><title>%s</title><id>%s</id><link href=\"%s\"/><updated>%s</updated><summary>%s</summary></entry>",
			xmlEscape(n.Title), xmlEscape(u), xmlEscape(u), at.UTC().Format(time.RFC3339), xmlEscape(truncateRunes(oneLineText(firstNonEmptyText(n.Summary, n.Content)), 280)))
	}
	b.WriteString("</feed>")
	return b.String()
}

func xmlEscape(s string) string { return html.EscapeString(s) }

func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func firstNonEmptyText(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// wikiTargets lists the [[targets]] of a text.
func wikiTargets(text string) []string {
	var out []string
	for {
		i := strings.Index(text, "[[")
		if i < 0 {
			return out
		}
		j := strings.Index(text[i:], "]]")
		if j < 0 {
			return out
		}
		inner := text[i+2 : i+j]
		if k := strings.IndexAny(inner, "|#"); k >= 0 {
			inner = inner[:k]
		}
		if inner = strings.TrimSpace(inner); inner != "" && !strings.Contains(inner, "\n") {
			out = append(out, inner)
		}
		text = text[i+j+2:]
	}
}

// Garden writes the site as a zip.
func Garden(ctx context.Context, db *database.DB, w io.Writer, o GardenOpts) (GardenReport, error) {
	files, rep, err := GardenFiles(ctx, db, o)
	if err != nil {
		return rep, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	zw := zip.NewWriter(w)
	for _, name := range names {
		f, err := zw.Create(name)
		if err != nil {
			return rep, err
		}
		if _, err := f.Write(files[name]); err != nil {
			return rep, err
		}
	}
	return rep, zw.Close()
}

const gardenManifest = ".garden-manifest"

// WriteGarden renders the site into dir. Files are replaced atomically and only when they changed;
// pages that are no longer published are removed (tracked in a manifest, so other files in dir,
// like a CNAME, are left alone).
func WriteGarden(ctx context.Context, db *database.DB, dir string, o GardenOpts) (GardenReport, error) {
	files, rep, err := GardenFiles(ctx, db, o)
	if err != nil {
		return rep, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return rep, err
	}
	old := map[string]bool{}
	if b, err := os.ReadFile(filepath.Join(dir, gardenManifest)); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" && filepath.IsLocal(ln) {
				old[ln] = true
			}
		}
	}
	names := make([]string, 0, len(files))
	for name, data := range files {
		names = append(names, name)
		target := filepath.Join(dir, filepath.FromSlash(name))
		if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return rep, err
		}
		tmp := target + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return rep, err
		}
		if err := os.Rename(tmp, target); err != nil {
			return rep, err
		}
	}
	sort.Strings(names)
	for name := range old {
		if _, still := files[name]; !still {
			os.Remove(filepath.Join(dir, filepath.FromSlash(name)))
			os.Remove(filepath.Dir(filepath.Join(dir, filepath.FromSlash(name)))) // only succeeds when empty
		}
	}
	return rep, os.WriteFile(filepath.Join(dir, gardenManifest), []byte(strings.Join(names, "\n")+"\n"), 0o644)
}

// GardenCandidates lists what would be published (for the preview in the web panel).
func GardenCandidates(ctx context.Context, db *database.DB, o GardenOpts) ([]database.Node, error) {
	return db.ListNodes(ctx, database.NodeFilter{Types: gardenTypes, Tag: o.Tag, Limit: 5000, Order: "date"})
}
