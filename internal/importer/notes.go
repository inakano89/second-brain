package importer

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
)

// ---- Evernote (.enex) ----

type enexNote struct {
	Title   string   `xml:"title"`
	Content string   `xml:"content"`
	Created string   `xml:"created"`
	Updated string   `xml:"updated"`
	Tags    []string `xml:"tag"`
	Attr    struct {
		SourceURL string `xml:"source-url"`
		Author    string `xml:"author"`
	} `xml:"note-attributes"`
	Resources []struct {
		Mime string `xml:"mime"`
		Attr struct {
			FileName string `xml:"file-name"`
		} `xml:"resource-attributes"`
	} `xml:"resource"`
	Tasks []struct {
		Title  string `xml:"title"`
		Status string `xml:"taskStatus"`
	} `xml:"task"`
}

var (
	enTodoDone = regexp.MustCompile(`<en-todo[^>]*checked="true"[^>]*/?>(?:</en-todo>)?`)
	enTodo     = regexp.MustCompile(`<en-todo[^>]*/?>(?:</en-todo>)?`)
	enMedia    = regexp.MustCompile(`<en-media[^>]*/?>(?:</en-media>)?`)
)

// enmlToText converts Evernote's ENML (XHTML) into Markdown-ish text.
func enmlToText(enml string) string {
	if i := strings.Index(enml, "<en-note"); i >= 0 {
		enml = enml[i:]
	}
	enml = enTodoDone.ReplaceAllString(enml, "- [x] ")
	enml = enTodo.ReplaceAllString(enml, "- [ ] ")
	enml = enMedia.ReplaceAllString(enml, " [anexo] ")
	return extract.HTMLToText(enml)
}

// parseENEX streams notes one by one, so attachments never pile up in memory.
func (p *parser) parseENEX(r io.Reader, name string) ([]Item, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	dec.CharsetReader = charset.NewReaderLabel
	notebook := cleanName(name)
	var items []Item
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if len(items) > 0 {
				return items, fmt.Errorf("%s: ENEX truncado após %d notas: %w", name, len(items), err)
			}
			return nil, fmt.Errorf("%s: ENEX inválido: %w", name, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "note" {
			continue
		}
		var n enexNote
		if err := dec.DecodeElement(&n, &se); err != nil {
			return items, fmt.Errorf("%s: nota inválida: %w", name, err)
		}
		it := Item{Format: FormatEvernote, Type: database.TypeNote, Title: strings.TrimSpace(n.Title), Tags: n.Tags, URL: strings.TrimSpace(n.Attr.SourceURL),
			Meta: map[string]any{"notebook": notebook}}
		it.Content = enmlToText(n.Content)
		if len(n.Tasks) > 0 {
			var b strings.Builder
			for _, t := range n.Tasks {
				box := " "
				if strings.EqualFold(t.Status, "completed") {
					box = "x"
				}
				fmt.Fprintf(&b, "- [%s] %s\n", box, strings.TrimSpace(t.Title))
			}
			it.Content = strings.TrimSpace(it.Content + "\n\n## Tarefas\n" + b.String())
		}
		if len(n.Resources) > 0 {
			var files []string
			for _, res := range n.Resources {
				files = append(files, firstNonEmpty(res.Attr.FileName, res.Mime))
			}
			it.Meta["attachments"] = files
		}
		if n.Attr.Author != "" {
			it.Meta["author"] = n.Attr.Author
		}
		if t, ok := parseDate(n.Created, time.UTC); ok {
			it.CreatedAt = t
		}
		if t, ok := parseDate(n.Updated, time.UTC); ok {
			it.Meta["updated"] = t.Format(time.RFC3339)
		}
		if n.Created != "" {
			it.Ref = hashRef(it.Title, n.Created)
		} else {
			it.Ref = hashRef(notebook, it.Title, extract.Truncate(it.Content, 200))
		}
		if it.Title == "" && it.Content == "" {
			continue
		}
		items = append(items, it)
	}
	return items, nil
}

// ---- Google Keep (Takeout JSON) ----

type keepNote struct {
	Title       string                  `json:"title"`
	TextContent string                  `json:"textContent"`
	IsTrashed   bool                    `json:"isTrashed"`
	IsArchived  bool                    `json:"isArchived"`
	IsPinned    bool                    `json:"isPinned"`
	Color       string                  `json:"color"`
	Created     int64                   `json:"createdTimestampUsec"`
	Edited      int64                   `json:"userEditedTimestampUsec"`
	Labels      []struct{ Name string } `json:"labels"`
	ListContent []struct {
		Text      string `json:"text"`
		IsChecked bool   `json:"isChecked"`
	} `json:"listContent"`
	Annotations []struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
	} `json:"annotations"`
	Attachments []struct {
		FilePath string `json:"filePath"`
	} `json:"attachments"`
}

func (p *parser) parseKeep(data []byte, name string) ([]Item, error) {
	var notes []keepNote
	trim := strings.TrimSpace(string(data))
	if strings.HasPrefix(trim, "[") {
		if err := json.Unmarshal(data, &notes); err != nil {
			return nil, fmt.Errorf("%s: JSON do Keep inválido: %w", name, err)
		}
	} else {
		var n keepNote
		if err := json.Unmarshal(data, &n); err != nil {
			return nil, fmt.Errorf("%s: JSON do Keep inválido: %w", name, err)
		}
		notes = []keepNote{n}
	}
	var items []Item
	for i, n := range notes {
		if n.IsTrashed {
			continue
		}
		var b strings.Builder
		b.WriteString(strings.TrimSpace(n.TextContent))
		for _, li := range n.ListContent {
			box := " "
			if li.IsChecked {
				box = "x"
			}
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "- [%s] %s", box, strings.TrimSpace(li.Text))
		}
		it := Item{Format: FormatKeep, Type: database.TypeNote, Title: strings.TrimSpace(n.Title), Tags: []string{"keep"}, Meta: map[string]any{}}
		for _, a := range n.Annotations {
			if a.URL != "" {
				fmt.Fprintf(&b, "\n\n🔗 [%s](%s)", firstNonEmpty(a.Title, a.URL), a.URL)
				if it.URL == "" {
					it.URL = a.URL
				}
			}
		}
		it.Content = strings.TrimSpace(b.String())
		if it.Title == "" && it.Content == "" {
			continue
		}
		for _, l := range n.Labels {
			it.Tags = append(it.Tags, l.Name)
		}
		if n.IsArchived {
			it.Meta["archived"] = true
		}
		if n.IsPinned {
			it.Meta["pinned"] = true
		}
		if len(n.Attachments) > 0 {
			var files []string
			for _, a := range n.Attachments {
				files = append(files, a.FilePath)
			}
			it.Meta["attachments"] = files
		}
		if n.Created > 0 {
			it.CreatedAt = time.UnixMicro(n.Created)
		} else if n.Edited > 0 {
			it.CreatedAt = time.UnixMicro(n.Edited)
		}
		it.Ref = cleanName(name)
		if len(notes) > 1 {
			it.Ref += "#" + strconv.Itoa(i)
		}
		items = append(items, it)
	}
	return items, nil
}

// ---- browser bookmarks (Netscape format), Pocket and Raindrop HTML exports ----

var rootFolders = map[string]bool{
	"bookmarks bar": true, "barra de favoritos": true, "other bookmarks": true, "outros favoritos": true, "mobile bookmarks": true,
	"favoritos do celular": true, "bookmarks toolbar": true, "barra de ferramentas de favoritos": true, "bookmarks menu": true,
	"menu de favoritos": true, "favorites bar": true, "barra de favoritos do edge": true, "favorites": true, "bookmarks": true,
	"favoritos": true, "unread": true, "read archive": true,
}

func attrOf(t html.Token, key string) string {
	for _, a := range t.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// parseBookmarks tokenizes the export, tracking the folder hierarchy (<H3> + <DL>).
func (p *parser) parseBookmarks(doc string) []Item {
	z := html.NewTokenizer(strings.NewReader(doc))
	var (
		folders    []string
		pending    string // last <H3> text, pushed on the next <DL>
		inH3, inA  bool
		cur        *Item
		inDD       bool
		items      []Item
		seen       = map[string]bool{}
		textBuf    strings.Builder
		pocketSect string
		inH1       bool
	)
	flush := func() {
		if cur == nil {
			return
		}
		cur.Title = firstNonEmpty(cur.Title, cur.URL)
		cur.Content = strings.TrimSpace(cur.Content)
		if cur.Content != "" {
			cur.Content += "\n\n"
		}
		cur.Content += "Fonte: " + cur.URL
		items = append(items, *cur)
		cur = nil
	}
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		t := z.Token()
		switch tt {
		case html.StartTagToken:
			switch t.DataAtom {
			case atom.H3:
				inH3, pending = true, ""
				textBuf.Reset()
			case atom.H1:
				inH1 = true
				textBuf.Reset()
			case atom.Dl:
				folders = append(folders, pending)
				pending = ""
			case atom.A:
				flush()
				inDD = false
				href := strings.TrimSpace(attrOf(t, "href"))
				if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") || seen[href] {
					continue
				}
				seen[href] = true
				inA = true
				textBuf.Reset()
				it := Item{Format: FormatBookmarks, Type: database.TypeArticle, URL: href, Ref: href, Tags: []string{"favorito"}, Meta: map[string]any{}}
				if d, ok := parseDate(firstNonEmpty(attrOf(t, "add_date"), attrOf(t, "time_added")), p.loc); ok {
					it.CreatedAt = d
				}
				it.Tags = append(it.Tags, splitList(attrOf(t, "tags"))...)
				var path []string
				for _, f := range folders {
					if f != "" && !rootFolders[strings.ToLower(f)] {
						path = append(path, f)
					}
				}
				if len(path) > 0 {
					it.Meta["folder"] = strings.Join(path, "/")
					it.Tags = append(it.Tags, path[len(path)-1])
				}
				if pocketSect != "" {
					it.Meta["pocket"] = pocketSect
				}
				cur = &it
			case atom.Dd:
				inDD = cur != nil
			}
		case html.EndTagToken:
			switch t.DataAtom {
			case atom.H3:
				inH3 = false
				pending = strings.TrimSpace(textBuf.String())
			case atom.H1:
				inH1 = false
				pocketSect = strings.ToLower(strings.TrimSpace(textBuf.String()))
			case atom.A:
				if inA && cur != nil {
					cur.Title = strings.Join(strings.Fields(textBuf.String()), " ")
				}
				inA = false
			case atom.Dl:
				flush()
				if len(folders) > 0 {
					folders = folders[:len(folders)-1]
				}
			}
		case html.TextToken:
			switch {
			case inH3 || inA || inH1:
				textBuf.WriteString(t.Data)
			case inDD && cur != nil:
				cur.Content += t.Data
			}
		}
	}
	flush()
	return items
}

// ---- OPML (feed subscriptions and outlines) ----

type outline struct {
	Text     string    `xml:"text,attr"`
	Title    string    `xml:"title,attr"`
	XMLURL   string    `xml:"xmlUrl,attr"`
	HTMLURL  string    `xml:"htmlUrl,attr"`
	Note     string    `xml:"_note,attr"`
	Children []outline `xml:"outline"`
}

func (o outline) label() string { return firstNonEmpty(o.Text, o.Title) }

func (o outline) hasFeeds() bool {
	if o.XMLURL != "" {
		return true
	}
	for _, c := range o.Children {
		if c.hasFeeds() {
			return true
		}
	}
	return false
}

func (o outline) collectFeeds(out *[]string) {
	if u := strings.TrimSpace(o.XMLURL); u != "" {
		*out = append(*out, u)
	}
	for _, c := range o.Children {
		c.collectFeeds(out)
	}
}

func writeOutline(b *strings.Builder, os []outline, depth int) {
	for _, o := range os {
		fmt.Fprintf(b, "%s- %s\n", strings.Repeat("  ", depth), strings.TrimSpace(extract.HTMLToText(o.label())))
		if n := strings.TrimSpace(o.Note); n != "" {
			fmt.Fprintf(b, "%s  %s\n", strings.Repeat("  ", depth), strings.ReplaceAll(n, "\n", " "))
		}
		writeOutline(b, o.Children, depth+1)
	}
}

// parseOPML returns feed URLs (for RSS_FEEDS) and outline notes (Workflowy, Dynalist).
func (p *parser) parseOPML(data []byte, name string) ([]Item, []string, error) {
	var doc struct {
		Head struct {
			Title string `xml:"title"`
		} `xml:"head"`
		Body struct {
			Outlines []outline `xml:"outline"`
		} `xml:"body"`
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	dec.CharsetReader = charset.NewReaderLabel
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("%s: OPML inválido: %w", name, err)
	}
	var feeds []string
	var items []Item
	var loose []outline
	for _, o := range doc.Body.Outlines {
		if o.hasFeeds() {
			o.collectFeeds(&feeds)
			continue
		}
		if len(o.Children) == 0 {
			loose = append(loose, o)
			continue
		}
		var b strings.Builder
		if n := strings.TrimSpace(o.Note); n != "" {
			b.WriteString(n + "\n\n")
		}
		writeOutline(&b, o.Children, 0)
		title := strings.TrimSpace(extract.HTMLToText(o.label()))
		items = append(items, Item{Format: FormatOPML, Ref: hashRef(name, title), Type: database.TypeNote, Title: title, Content: strings.TrimSpace(b.String()), Tags: []string{"outline"}})
	}
	if len(loose) > 0 {
		var b strings.Builder
		writeOutline(&b, loose, 0)
		title := firstNonEmpty(doc.Head.Title, cleanName(name))
		items = append(items, Item{Format: FormatOPML, Ref: hashRef(name, "#loose"), Type: database.TypeNote, Title: title, Content: strings.TrimSpace(b.String()), Tags: []string{"outline"}})
	}
	return items, feeds, nil
}

// ---- Kindle (My Clippings.txt) ----

var (
	kindleLoc    = regexp.MustCompile(`(?i)(?:location|loc\.|posição|posições|pos\.|position)\s*([\d]+(?:-\d+)?)`)
	kindlePage   = regexp.MustCompile(`(?i)(?:page|página|pagina|seite|page)\s*([\dxivlc]+(?:-[\dxivlc]+)?)`)
	kindleAuthor = regexp.MustCompile(`^(.*)\(([^()]*)\)\s*$`)
	ptMonths     = strings.NewReplacer("janeiro", "January", "fevereiro", "February", "março", "March", "marco", "March", "abril", "April", "maio", "May",
		"junho", "June", "julho", "July", "agosto", "August", "setembro", "September", "outubro", "October", "novembro", "November", "dezembro", "December")
	kindleDatePT = regexp.MustCompile(`(\d{1,2}) de (\p{L}+) de (\d{4}),? (\d{1,2}:\d{2}(?::\d{2})?)`)
	kindleDateEN = regexp.MustCompile(`(\p{L}+ \d{1,2}, \d{4}) (\d{1,2}:\d{2}:\d{2} [AP]M)`)
)

type clipping struct {
	kind, text, loc, page string
	at                    time.Time
}

func (p *parser) kindleDate(s string) time.Time {
	if m := kindleDateEN.FindStringSubmatch(s); m != nil {
		if t, err := time.ParseInLocation("January 2, 2006 3:04:05 PM", m[1]+" "+m[2], p.loc); err == nil {
			return t
		}
	}
	if m := kindleDatePT.FindStringSubmatch(strings.ToLower(s)); m != nil {
		clock := m[4]
		if strings.Count(clock, ":") == 1 {
			clock += ":00"
		}
		if t, err := time.ParseInLocation("2 January 2006 15:04:05", m[1]+" "+ptMonths.Replace(m[2])+" "+m[3]+" "+clock, p.loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseKindle groups highlights and notes by book: one article per book.
func (p *parser) parseKindle(doc string) []Item {
	doc = strings.ReplaceAll(strings.ReplaceAll(doc, "\ufeff", ""), "\r\n", "\n")
	type book struct {
		title, author string
		clips         []clipping
		seen          map[string]bool
		first         time.Time
	}
	books := map[string]*book{}
	var order []string
	for _, chunk := range strings.Split(doc, "==========") {
		lines := strings.Split(strings.Trim(chunk, "\n "), "\n")
		if len(lines) < 2 {
			continue
		}
		head := strings.TrimSpace(lines[0])
		info := strings.TrimSpace(lines[1])
		text := strings.TrimSpace(strings.Join(lines[2:], "\n"))
		lower := strings.ToLower(info)
		kind := "highlight"
		switch {
		case strings.Contains(lower, "bookmark") || strings.Contains(lower, "marcador"):
			continue
		case strings.Contains(lower, "note") || strings.Contains(lower, "nota"):
			kind = "note"
		}
		if text == "" {
			continue
		}
		title, author := head, ""
		if m := kindleAuthor.FindStringSubmatch(head); m != nil {
			title, author = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		}
		key := strings.ToLower(title + "|" + author)
		bk := books[key]
		if bk == nil {
			bk = &book{title: title, author: author, seen: map[string]bool{}}
			books[key] = bk
			order = append(order, key)
		}
		if bk.seen[kind+text] {
			continue
		}
		bk.seen[kind+text] = true
		c := clipping{kind: kind, text: text, at: p.kindleDate(info)}
		if m := kindleLoc.FindStringSubmatch(info); m != nil {
			c.loc = m[1]
		}
		if m := kindlePage.FindStringSubmatch(info); m != nil {
			c.page = m[1]
		}
		if !c.at.IsZero() && (bk.first.IsZero() || c.at.Before(bk.first)) {
			bk.first = c.at
		}
		bk.clips = append(bk.clips, c)
	}
	var items []Item
	for _, key := range order {
		bk := books[key]
		var b strings.Builder
		if bk.author != "" {
			fmt.Fprintf(&b, "**Autor:** %s\n\n", bk.author)
		}
		b.WriteString("## Destaques\n\n")
		for _, c := range bk.clips {
			var ref []string
			if c.page != "" {
				ref = append(ref, "p. "+c.page)
			}
			if c.loc != "" {
				ref = append(ref, "pos. "+c.loc)
			}
			if !c.at.IsZero() {
				ref = append(ref, c.at.Format("02/01/2006"))
			}
			if c.kind == "note" {
				fmt.Fprintf(&b, "📝 %s", c.text)
			} else {
				fmt.Fprintf(&b, "> %s", strings.ReplaceAll(c.text, "\n", "\n> "))
			}
			if len(ref) > 0 {
				fmt.Fprintf(&b, "\n— %s", strings.Join(ref, " · "))
			}
			b.WriteString("\n\n")
		}
		meta := map[string]any{"highlights": len(bk.clips)}
		if bk.author != "" {
			meta["author"] = bk.author
		}
		items = append(items, Item{
			Format: FormatKindle, Ref: hashRef(key), Type: database.TypeArticle, Title: bk.title, Content: strings.TrimSpace(b.String()),
			Tags: []string{"kindle", "livro"}, CreatedAt: bk.first, Meta: meta,
		})
	}
	return items
}
