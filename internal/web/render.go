package web

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/profile"
)

var weekdayNames = []string{"Domingo", "Segunda", "Terça", "Quarta", "Quinta", "Sexta", "Sábado"}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(s.Cfg.Location()).Format("02/01/2006 15:04")
		},
		"fmtDate": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return ""
			}
			return t.In(s.Cfg.Location()).Format("02/01/2006 15:04")
		},
		"inputDate": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return ""
			}
			return t.In(s.Cfg.Location()).Format("2006-01-02T15:04")
		},
		"since": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			d := time.Since(t)
			switch {
			case d < 0:
				return "em " + humanDur(-d)
			case d < time.Minute:
				return "agora"
			}
			return "há " + humanDur(d)
		},
		"until":     func(t time.Time) string { return humanDur(time.Until(t)) },
		"truncate":  extract.Truncate,
		"join":      strings.Join,
		"typeLabel": func(t string) string { return typeLabels[t] },
		"typeColor": func(t string) template.CSS { return template.CSS(typeColors[t]) }, // constant var(--t-*) values
		"markdown":  Markdown,
		"json": func(v any) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
		"usd":      func(v float64) string { return fmt.Sprintf("US$ %.4f", v) },
		"weekdays": func() []string { return profile.Weekdays },
		"kindLabel": func(k string) string {
			if d := profile.KindOf(k); d != nil {
				return d.Label
			}
			return k
		},
		"fmtDay": func(t time.Time) string {
			return weekdayNames[t.Weekday()] + ", " + t.Format("02/01/2006")
		},
		"rfc3339": func(t time.Time) string { return t.Format(time.RFC3339) },
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"num":       func(v int64) string { return humanInt(v) },
		"pct":       func(v float64) string { return fmt.Sprintf("%.0f%%", v) },
		"safeURL":   func(s string) template.URL { return template.URL(s) },
		"urlquery":  url.QueryEscape,
		"hasPrefix": strings.HasPrefix,
		"split":     func(s string) []string { return strings.Split(s, ",") },
		"routeKey":  llm.RouteKey,
		"price": func(p *llm.Price) string {
			if p == nil {
				return "preço desconhecido"
			}
			return fmt.Sprintf("US$ %g / %g", p.In, p.Out)
		},
		"helpAnchor": func(group string) string { return helpAnchors[group] },
		"metricVal": func(kind string, v float64) string {
			if strings.HasSuffix(kind, "_minutes") {
				return fmt.Sprintf("%dh%02d", int(v)/60, int(v)%60)
			}
			return fmt.Sprintf("%.0f", v)
		},
		"levelClass":    func(l string) string { return "lvl-" + strings.ToLower(l) },
		"importState":   func(st string) string { return importStates[st] },
		"sourceLabel":   sourceLabel,
		"nodeDate":      func(n database.Node) string { return nodeDate(n, s.Cfg.Location()) },
		"importedNote":  func(n database.Node) string { return importedNote(n, s.Cfg.Location()) },
		"originLabel":   func(o string) string { return originLabels[o] },
		"isUnknownDate": func(n database.Node) bool { return n.DateUnknown() },
	}
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d dias", int(d.Hours()/24))
}

func humanInt(v int64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(v)/1e6)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", float64(v)/1e3)
	}
	return fmt.Sprint(v)
}

var (
	mdBold   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdItal   = regexp.MustCompile(`(^|[\s(])[*_]([^*_\n]+)[*_]`)
	mdCode   = regexp.MustCompile("`([^`\n]+)`")
	mdLink   = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	mdWiki   = regexp.MustCompile(`\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]`)
	mdURL    = regexp.MustCompile(`(^|\s)(https?://[^\s<]+)`)
	mdOrder  = regexp.MustCompile(`^\d+[.)]\s+`)
	mdHeader = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
)

func inline(s string) string {
	s = html.EscapeString(s)
	var codes []string
	s = mdCode.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, "<code>"+m[1:len(m)-1]+"</code>")
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	})
	s = mdWiki.ReplaceAllStringFunc(s, func(m string) string {
		p := mdWiki.FindStringSubmatch(m)
		label := p[1]
		if p[2] != "" {
			label = p[2]
		}
		return `<a class="wikilink" href="/?q=` + url.QueryEscape(html.UnescapeString(p[1])) + `">` + label + `</a>`
	})
	s = mdLink.ReplaceAllString(s, `<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>`)
	s = mdURL.ReplaceAllString(s, `$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>`)
	s = mdBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = mdItal.ReplaceAllString(s, "$1<em>$2</em>")
	for i, c := range codes {
		s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), c, 1)
	}
	return s
}

// Markdown renders a safe subset of Markdown to HTML (all input is escaped first).
func Markdown(src string) template.HTML {
	var b strings.Builder
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	inCode, inList, inTable := false, "", false
	var para []string
	flushPara := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
			para = nil
		}
	}
	closeList := func() {
		if inList != "" {
			b.WriteString("</" + inList + ">")
			inList = ""
		}
	}
	closeTable := func() {
		if inTable {
			b.WriteString("</table>")
			inTable = false
		}
	}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			flushPara()
			closeList()
			closeTable()
			if inCode {
				b.WriteString("</code></pre>")
			} else {
				b.WriteString("<pre><code>")
			}
			inCode = !inCode
			continue
		}
		if inCode {
			b.WriteString(html.EscapeString(ln) + "\n")
			continue
		}
		switch {
		case t == "":
			flushPara()
			closeList()
			closeTable()
		case mdHeader.MatchString(t):
			flushPara()
			closeList()
			closeTable()
			m := mdHeader.FindStringSubmatch(t)
			lvl := len(m[1]) + 2
			if lvl > 6 {
				lvl = 6
			}
			fmt.Fprintf(&b, "<h%d>%s</h%d>", lvl, inline(m[2]), lvl)
		case strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|"):
			flushPara()
			closeList()
			if strings.Trim(t, "|-: ") == "" {
				continue
			}
			if !inTable {
				b.WriteString(`<table class="md">`)
				inTable = true
			}
			b.WriteString("<tr>")
			for _, c := range strings.Split(strings.Trim(t, "|"), "|") {
				b.WriteString("<td>" + inline(strings.TrimSpace(c)) + "</td>")
			}
			b.WriteString("</tr>")
		case strings.HasPrefix(t, "- [ ] ") || strings.HasPrefix(t, "- [x] "):
			flushPara()
			if inList != "ul" {
				closeList()
				b.WriteString(`<ul class="checklist">`)
				inList = "ul"
			}
			mark := "☐"
			if strings.HasPrefix(t, "- [x]") {
				mark = "☑"
			}
			b.WriteString("<li>" + mark + " " + inline(t[6:]) + "</li>")
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "• "):
			flushPara()
			if inList != "ul" {
				closeList()
				b.WriteString("<ul>")
				inList = "ul"
			}
			_, rest, _ := strings.Cut(t, " ")
			b.WriteString("<li>" + inline(rest) + "</li>")
		case mdOrder.MatchString(t):
			flushPara()
			if inList != "ol" {
				closeList()
				b.WriteString("<ol>")
				inList = "ol"
			}
			b.WriteString("<li>" + inline(mdOrder.ReplaceAllString(t, "")) + "</li>")
		case strings.HasPrefix(t, ">"):
			flushPara()
			closeList()
			b.WriteString("<blockquote>" + inline(strings.TrimSpace(strings.TrimPrefix(t, ">"))) + "</blockquote>")
		case t == "---" || t == "***":
			flushPara()
			closeList()
			b.WriteString("<hr>")
		default:
			closeList()
			closeTable()
			para = append(para, inline(t))
		}
	}
	if inCode {
		b.WriteString("</code></pre>")
	}
	flushPara()
	closeList()
	closeTable()
	return template.HTML(b.String())
}
