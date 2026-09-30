// Package mdhtml renders the small Markdown subset used by notes (headings, lists, checklists,
// tables, quotes, code, links and [[wiki-links]]) to safe HTML: everything is escaped first.
package mdhtml

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// WikiFunc renders a [[target|label]] link; target is raw text and label is HTML-escaped.
type WikiFunc func(target, label string) string

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

func inline(s string, wiki WikiFunc) string {
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
		return wiki(html.UnescapeString(p[1]), label)
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

// Render renders a safe subset of Markdown to HTML (all input is escaped first). wiki turns a
// [[target|label]] link into HTML; target is raw text, label is already escaped.
func Render(src string, wiki WikiFunc) string {
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
			fmt.Fprintf(&b, "<h%d>%s</h%d>", lvl, inline(m[2], wiki), lvl)
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
				b.WriteString("<td>" + inline(strings.TrimSpace(c), wiki) + "</td>")
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
			b.WriteString("<li>" + mark + " " + inline(t[6:], wiki) + "</li>")
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "• "):
			flushPara()
			if inList != "ul" {
				closeList()
				b.WriteString("<ul>")
				inList = "ul"
			}
			_, rest, _ := strings.Cut(t, " ")
			b.WriteString("<li>" + inline(rest, wiki) + "</li>")
		case mdOrder.MatchString(t):
			flushPara()
			if inList != "ol" {
				closeList()
				b.WriteString("<ol>")
				inList = "ol"
			}
			b.WriteString("<li>" + inline(mdOrder.ReplaceAllString(t, ""), wiki) + "</li>")
		case strings.HasPrefix(t, ">"):
			flushPara()
			closeList()
			b.WriteString("<blockquote>" + inline(strings.TrimSpace(strings.TrimPrefix(t, ">")), wiki) + "</blockquote>")
		case t == "---" || t == "***":
			flushPara()
			closeList()
			b.WriteString("<hr>")
		default:
			closeList()
			closeTable()
			para = append(para, inline(t, wiki))
		}
	}
	if inCode {
		b.WriteString("</code></pre>")
	}
	flushPara()
	closeList()
	closeTable()
	return b.String()
}
