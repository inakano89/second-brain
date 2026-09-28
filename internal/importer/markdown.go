package importer

import (
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
)

var (
	notionID   = regexp.MustCompile(`\s+[0-9a-f]{32}$`)
	fmEnd      = regexp.MustCompile(`(?m)^(?:---|\.\.\.)[ \t]*$`)
	logseqProp = regexp.MustCompile(`^([A-Za-z][\w-]*)::\s*(.*)$`)
	mdFileLink = regexp.MustCompile(`\[([^\]\n]*)\]\(([^)\s]+?\.md)\)`)
	inlineTag  = regexp.MustCompile(`(?:^|\s)#([\p{L}\p{N}_\-/]*\p{L}[\p{L}\p{N}_\-/]*)`)
	exportRel  = regexp.MustCompile(`^- \*\*([^*]+):\*\*\s*(.+)$`)
	wikiTarget = regexp.MustCompile(`\[\[([^\]|#]+)(?:[#|][^\]]*)?\]\]`)
	dailyNote  = regexp.MustCompile(`^\d{4}[-_]\d{2}[-_]\d{2}$`)
)

// cleanName turns a file name into a title: extension, Notion ids and URL escapes removed.
func cleanName(p string) string {
	base := path.Base(p)
	base = strings.TrimSuffix(base, path.Ext(base))
	if strings.Contains(base, "%") {
		if u, err := url.PathUnescape(base); err == nil {
			base = u
		}
	}
	return strings.TrimSpace(notionID.ReplaceAllString(base, ""))
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
	}
	return strings.Trim(v, `"'`)
}

// parseFrontmatter reads a YAML frontmatter subset: scalars, [inline, lists] and block lists.
func parseFrontmatter(s string) (map[string][]string, string) {
	fm := map[string][]string{}
	if !strings.HasPrefix(s, "---\n") {
		return fm, s
	}
	rest := s[4:]
	loc := fmEnd.FindStringIndex(rest)
	if loc == nil {
		return fm, s
	}
	key := ""
	for _, l := range strings.Split(rest[:loc[0]], "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if key != "" && (strings.HasPrefix(t, "- ") || t == "-") && (l[0] == ' ' || l[0] == '\t' || l[0] == '-') {
			if v := unquote(strings.TrimPrefix(t, "-")); v != "" {
				fm[key] = append(fm[key], v)
			}
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch {
		case v == "":
			fm[key] = nil
		case strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"):
			var list []string
			for _, x := range strings.Split(v[1:len(v)-1], ",") {
				if x = unquote(x); x != "" {
					list = append(list, x)
				}
			}
			fm[key] = list
		default:
			fm[key] = []string{unquote(v)}
		}
	}
	return fm, strings.TrimLeft(rest[loc[1]:], "\n")
}

func first(m map[string][]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; len(v) > 0 && strings.TrimSpace(v[0]) != "" {
			return strings.TrimSpace(v[0])
		}
	}
	return ""
}

// fmList returns list values; a scalar is split by commas (or spaces for tags).
func fmList(m map[string][]string, key string) []string {
	v := m[key]
	if len(v) == 1 {
		s := strings.ReplaceAll(strings.ReplaceAll(v[0], "[[", ""), "]]", "")
		if strings.Contains(s, ",") {
			return splitList(s)
		}
		if key == "tags" || key == "tag" {
			return strings.Fields(s)
		}
		return []string{s}
	}
	return v
}

// logseqProps strips leading "key:: value" page properties (Logseq).
func logseqProps(body string) (map[string][]string, string) {
	props := map[string][]string{}
	lines := strings.Split(body, "\n")
	i := 0
	for ; i < len(lines); i++ {
		m := logseqProp.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			break
		}
		props[strings.ToLower(m[1])] = []string{m[2]}
	}
	if i == 0 {
		return props, body
	}
	return props, strings.TrimLeft(strings.Join(lines[i:], "\n"), "\n")
}

// convertMDLinks rewrites relative links to .md files (Notion, Obsidian) as [[wiki-links]].
func convertMDLinks(s string) string {
	return mdFileLink.ReplaceAllStringFunc(s, func(m string) string {
		p := mdFileLink.FindStringSubmatch(m)
		if strings.Contains(p[2], "://") {
			return m
		}
		target := cleanName(p[2])
		text := strings.TrimSpace(p[1])
		if target == "" {
			return m
		}
		if text == "" || strings.EqualFold(text, target) {
			return "[[" + target + "]]"
		}
		return "[[" + target + "|" + text + "]]"
	})
}

func inlineTags(body string) []string {
	var out []string
	inCode := false
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		for _, m := range inlineTag.FindAllStringSubmatch(l, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// markdownItem converts a Markdown document (Obsidian, Logseq, Notion, Joplin, Bear or
// a Second Brain export) into an item. relPath is the path inside the archive.
func (p *parser) markdownItem(kind, relPath, text string, mod time.Time) Item {
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n")
	fm, body := parseFrontmatter(text)
	props, body := logseqProps(body)
	for k, v := range props {
		if _, ok := fm[k]; !ok {
			fm[k] = v
		}
	}
	name := cleanName(relPath)
	it := Item{Format: kind, Type: database.TypeNote, Meta: map[string]any{"path": relPath}, Aliases: []string{name}}
	it.Title = firstNonEmpty(first(fm, "title"), name)
	if it.Title == "" || strings.EqualFold(it.Title, "untitled") || strings.EqualFold(it.Title, "sem título") {
		it.Title = firstNonEmpty(extract.FirstLine(body), it.Title)
	}
	it.Aliases = append(it.Aliases, fmList(fm, "aliases")...)
	it.Aliases = append(it.Aliases, fmList(fm, "alias")...)
	it.Tags = append(fmList(fm, "tags"), fmList(fm, "tag")...)
	it.Tags = append(it.Tags, inlineTags(body)...)
	if t := first(fm, "type"); database.ValidType(t) {
		it.Type = t
	}
	it.Status = first(fm, "status")
	if it.Type != database.TypeTask {
		it.Status = ""
	}
	if d, ok := parseDate(first(fm, "due", "due_at", "prazo"), p.loc); ok {
		it.DueAt = &d
	}
	it.URL = first(fm, "url", "source_url", "link")
	it.Summary = first(fm, "summary", "resumo")
	if d, ok := parseDate(first(fm, "created", "created_at", "date", "criado", "data"), p.loc); ok {
		it.CreatedAt = d
	} else if d, ok := parseDate(strings.ReplaceAll(name, "_", "-"), p.loc); ok && dailyNote.MatchString(name) {
		it.CreatedAt = d // daily notes: 2024-01-15.md, journals/2024_01_15.md
	} else {
		it.CreatedAt = mod
	}
	for _, k := range []string{"author", "autor", "source", "updated", "modified"} {
		if v := first(fm, k); v != "" {
			it.Meta[k] = v
		}
	}
	if kind == FormatNotion {
		body = strings.TrimPrefix(body, "# "+it.Title+"\n")
	}
	if uid := first(fm, "uid"); uid != "" && first(fm, "type") != "" {
		// Second Brain export: restore the original identity and typed connections.
		it.Ref = "uid:" + uid
		if src := first(fm, "source"); src != "" {
			it.Meta["original_source"] = src
		}
		delete(it.Meta, "source")
		body, it.Links = stripExportSections(body, it.Title)
	}
	if it.Ref == "" {
		it.Ref = relPath
	}
	body = convertMDLinks(body)
	if kind != FormatNotion {
		body = stripLogseqNoise(body)
	}
	it.Content = strings.TrimSpace(body)
	return it
}

// stripExportSections removes the heading, task checkbox and "## Conexões" block that
// export.Obsidian adds, returning the outgoing typed links from that block.
func stripExportSections(body, title string) (string, []Link) {
	var links []Link
	if i := strings.LastIndex(body, "\n## Conexões\n"); i >= 0 {
		for _, l := range strings.Split(body[i+len("\n## Conexões\n"):], "\n") {
			m := exportRel.FindStringSubmatch(strings.TrimSpace(l))
			if m == nil || strings.HasPrefix(m[1], "←") {
				continue
			}
			for _, t := range wikiTarget.FindAllStringSubmatch(m[2], -1) {
				links = append(links, Link{Target: strings.TrimSpace(t[1]), Relation: strings.TrimSpace(m[1])})
			}
		}
		body = body[:i]
	}
	body = strings.TrimPrefix(body, "# "+title+"\n")
	body = strings.TrimLeft(body, "\n")
	for _, box := range []string{"- [ ] ", "- [x] "} {
		body = strings.TrimPrefix(body, box+title+"\n")
	}
	return body, links
}

var logseqNoise = regexp.MustCompile(`(?m)^\s*(?:id|collapsed)::\s.*\n?`)

func stripLogseqNoise(s string) string { return logseqNoise.ReplaceAllString(s, "") }

// htmlItem extracts the readable text of an HTML page (Notion/Evernote HTML exports).
func (p *parser) htmlItem(relPath, doc string, mod time.Time) (Item, error) {
	art, err := extract.Readability(doc)
	if err != nil {
		return Item{}, err
	}
	name := cleanName(relPath)
	return Item{
		Format: FormatHTML, Ref: relPath, Type: database.TypeNote, Title: firstNonEmpty(art.Title, name),
		Content: art.Text, CreatedAt: mod, Aliases: []string{name}, Meta: map[string]any{"path": relPath},
	}, nil
}
