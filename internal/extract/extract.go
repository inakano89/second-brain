// Package extract turns HTML pages, PDFs and text files into clean text
// (simplified Readability algorithm for articles).
package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Article is the extracted readable content of a page.
type Article struct {
	URL         string
	Title       string
	Byline      string
	Description string
	SiteName    string
	Text        string // markdown-ish plain text
}

// HTTPClient is used for fetching pages.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// Fetch downloads a URL and extracts its main content.
func Fetch(ctx context.Context, url string) (*Article, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SecondBrain/1.0; +https://github.com/inakano89/second-brain)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/pdf") {
		text, err := PDFBytes(body)
		return &Article{URL: url, Title: url, Text: text}, err
	}
	if !strings.Contains(ct, "html") && ct != "" {
		return &Article{URL: url, Title: url, Text: string(body)}, nil
	}
	a, err := Readability(string(body))
	if err != nil {
		return nil, err
	}
	a.URL = url
	return a, nil
}

var skipTags = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Svg: true, atom.Iframe: true,
	atom.Form: true, atom.Button: true, atom.Input: true, atom.Select: true, atom.Textarea: true,
	atom.Nav: true, atom.Footer: true, atom.Aside: true, atom.Template: true, atom.Canvas: true,
}

var negativeRe = regexp.MustCompile(`(?i)comment|meta|footer|footnote|sidebar|sponsor|ad-|advert|promo|share|social|related|newsletter|cookie|banner|popup|menu|nav|breadcrumb|subscribe`)
var positiveRe = regexp.MustCompile(`(?i)article|body|content|entry|main|page|post|text|blog|story`)

// Readability extracts the main article from an HTML document.
func Readability(doc string) (*Article, error) {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return nil, err
	}
	a := &Article{}
	collectMeta(root, a)
	best := bestCandidate(root)
	if best == nil {
		best = findFirst(root, atom.Body)
	}
	if best == nil {
		best = root
	}
	a.Text = cleanup(render(best))
	if a.Title == "" {
		a.Title = firstLine(a.Text)
	}
	if a.Text == "" {
		return a, errors.New("extract: no readable content")
	}
	return a, nil
}

// HTMLToText converts an HTML fragment (e.g. user selection) into text.
func HTMLToText(fragment string) string {
	root, err := html.Parse(strings.NewReader("<html><body>" + fragment + "</body></html>"))
	if err != nil {
		return fragment
	}
	return cleanup(render(root))
}

func collectMeta(n *html.Node, a *Article) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Title:
				if a.Title == "" && n.FirstChild != nil {
					a.Title = strings.TrimSpace(n.FirstChild.Data)
				}
			case atom.Meta:
				key := strings.ToLower(attr(n, "property") + attr(n, "name"))
				val := strings.TrimSpace(attr(n, "content"))
				switch key {
				case "og:title", "twitter:title":
					if val != "" {
						a.Title = val
					}
				case "description", "og:description":
					if a.Description == "" {
						a.Description = val
					}
				case "author", "article:author":
					a.Byline = val
				case "og:site_name":
					a.SiteName = val
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func findFirst(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findFirst(c, a); f != nil {
			return f
		}
	}
	return nil
}

func textLen(n *html.Node) (chars, linkChars int) {
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inLink bool) {
		if n.Type == html.ElementNode && skipTags[n.DataAtom] {
			return
		}
		if n.Type == html.TextNode {
			l := utf8.RuneCountInString(strings.TrimSpace(n.Data))
			chars += l
			if inLink {
				linkChars += l
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inLink || (n.Type == html.ElementNode && n.DataAtom == atom.A))
		}
	}
	walk(n, false)
	return
}

func classWeight(n *html.Node) float64 {
	s := attr(n, "class") + " " + attr(n, "id")
	w := 0.0
	if negativeRe.MatchString(s) {
		w -= 25
	}
	if positiveRe.MatchString(s) {
		w += 25
	}
	return w
}

// bestCandidate scores parents of paragraphs (Arc90 heuristic).
func bestCandidate(root *html.Node) *html.Node {
	if art := findFirst(root, atom.Article); art != nil {
		if c, _ := textLen(art); c > 500 {
			return art
		}
	}
	scores := map[*html.Node]float64{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.DataAtom] {
			return
		}
		if n.Type == html.ElementNode && (n.DataAtom == atom.P || n.DataAtom == atom.Pre || n.DataAtom == atom.Td || n.DataAtom == atom.Blockquote) {
			chars, _ := textLen(n)
			if chars >= 25 {
				var sb strings.Builder
				collectText(n, &sb)
				score := 1 + float64(strings.Count(sb.String(), ",")) + float64(min(chars/100, 3))
				if p := n.Parent; p != nil {
					scores[p] += score
					if gp := p.Parent; gp != nil {
						scores[gp] += score / 2
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	var best *html.Node
	bestScore := 0.0
	for n, s := range scores {
		chars, links := textLen(n)
		density := 0.0
		if chars > 0 {
			density = float64(links) / float64(chars)
		}
		final := (s + classWeight(n)) * (1 - density)
		if final > bestScore {
			best, bestScore = n, final
		}
	}
	return best
}

func collectText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, sb)
	}
}

// render converts a subtree to markdown-ish text.
func render(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node, int)
	walk = func(n *html.Node, depth int) {
		switch n.Type {
		case html.TextNode:
			t := strings.Join(strings.Fields(n.Data), " ")
			if t != "" {
				if b.Len() > 0 {
					last := b.String()[b.Len()-1]
					if last != '\n' && last != ' ' && last != '(' && last != '[' {
						b.WriteByte(' ')
					}
				}
				b.WriteString(t)
			}
			return
		case html.ElementNode:
			if skipTags[n.DataAtom] {
				return
			}
			switch n.DataAtom {
			case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
				level := int(n.Data[1] - '0')
				b.WriteString("\n\n" + strings.Repeat("#", level) + " ")
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, depth)
				}
				b.WriteString("\n\n")
				return
			case atom.P, atom.Div, atom.Section, atom.Article, atom.Blockquote, atom.Table, atom.Figure:
				b.WriteString("\n\n")
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, depth)
				}
				b.WriteString("\n\n")
				return
			case atom.Br:
				b.WriteString("\n")
				return
			case atom.Li:
				b.WriteString("\n" + strings.Repeat("  ", max(depth-1, 0)) + "- ")
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, depth)
				}
				return
			case atom.Ul, atom.Ol:
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, depth+1)
				}
				b.WriteString("\n")
				return
			case atom.Tr:
				b.WriteString("\n|")
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, depth)
					if c.Type == html.ElementNode {
						b.WriteString(" |")
					}
				}
				return
			case atom.Pre:
				var sb strings.Builder
				collectText(n, &sb)
				b.WriteString("\n\n```\n" + strings.TrimSpace(sb.String()) + "\n```\n\n")
				return
			case atom.A:
				href := attr(n, "href")
				var sb strings.Builder
				collectText(n, &sb)
				t := strings.Join(strings.Fields(sb.String()), " ")
				if t == "" {
					return
				}
				if strings.HasPrefix(href, "http") {
					b.WriteString(" [" + t + "](" + href + ")")
				} else {
					b.WriteString(" " + t)
				}
				return
			case atom.Img:
				if alt := attr(n, "alt"); alt != "" {
					b.WriteString(" [imagem: " + alt + "]")
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, depth)
		}
	}
	walk(n, 0)
	return b.String()
}

var multiNL = regexp.MustCompile(`\n{3,}`)
var spaceNL = regexp.MustCompile(`[ \t]+\n`)

func cleanup(s string) string {
	s = spaceNL.ReplaceAllString(s, "\n")
	s = multiNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.TrimLeft(line, "# ")
	if r := []rune(line); len(r) > 100 {
		line = string(r[:100]) + "…"
	}
	return line
}

// FirstLine returns a short title candidate from text.
func FirstLine(s string) string { return firstLine(s) }

// PDFBytes extracts plain text from a PDF.
func PDFBytes(data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		t, err := p.GetPlainText(nil)
		if err != nil {
			continue
		}
		b.WriteString(t)
		b.WriteString("\n\n")
	}
	return cleanup(b.String()), nil
}

// Truncate cuts s to at most n runes.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
