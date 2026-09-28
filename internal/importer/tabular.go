package importer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/inakano89/second-brain/internal/database"
)

type field int

const (
	fNone field = iota
	fTitle
	fContent
	fSummary
	fTags
	fType
	fURL
	fDue
	fCreated
	fStatus
	fID
	fUID
	fAuthor
	fSource
)

// fieldAliases maps normalized column/key names (pt/en) to item fields.
var fieldAliases = func() map[string]field {
	m := map[string]field{}
	add := func(f field, names ...string) {
		for _, n := range names {
			m[n] = f
		}
	}
	add(fTitle, "title", "titulo", "name", "nome", "subject", "assunto", "heading", "headline", "book title", "livro", "task", "tarefa", "task name", "page", "pagina")
	add(fContent, "content", "conteudo", "body", "text", "texto", "note", "notes", "nota", "notas", "description", "descricao", "highlight", "destaque",
		"details", "detalhes", "review", "my review", "excerpt", "trecho", "markdown", "text content", "comment", "comentario", "observacoes", "obs")
	add(fSummary, "summary", "resumo", "abstract")
	add(fTags, "tags", "tag", "labels", "label", "etiquetas", "etiqueta", "categories", "category", "categoria", "categorias", "folder", "pasta",
		"document tags", "keywords", "palavras chave", "collection", "colecao", "bookshelves")
	add(fType, "type", "tipo", "kind")
	add(fURL, "url", "link", "href", "website", "site", "source url", "uri", "permalink")
	add(fDue, "due", "due date", "due at", "prazo", "vencimento", "deadline", "data limite", "date due", "data de vencimento", "entrega")
	add(fCreated, "created", "created at", "created time", "criado", "criado em", "date", "data", "time added", "added", "date added",
		"highlighted at", "timestamp", "creation date", "data de criacao", "published", "publicado", "added at", "saved at")
	add(fStatus, "status", "done", "completed", "concluido", "concluida", "state", "estado", "situacao", "checked", "feito")
	add(fID, "id", "guid", "key", "chave")
	add(fUID, "uid", "uuid")
	add(fAuthor, "author", "autor", "book author", "by", "creator", "criador", "authors", "autores")
	add(fSource, "source", "fonte", "origem")
	return m
}()

var typeAliases = map[string]string{
	"note": database.TypeNote, "nota": database.TypeNote, "notas": database.TypeNote,
	"task": database.TypeTask, "tarefa": database.TypeTask, "todo": database.TypeTask, "to do": database.TypeTask,
	"event": database.TypeEvent, "evento": database.TypeEvent,
	"person": database.TypePerson, "pessoa": database.TypePerson, "contato": database.TypePerson, "contact": database.TypePerson,
	"insight": database.TypeInsight, "ideia": database.TypeInsight, "idea": database.TypeInsight,
	"article": database.TypeArticle, "artigo": database.TypeArticle, "link": database.TypeArticle, "bookmark": database.TypeArticle,
	"health": database.TypeHealth, "saude": database.TypeHealth,
}

// record is a tabular row / JSON object with values assigned to fields.
type record struct {
	vals  map[field]string
	tags  []string
	extra [][2]string
	meta  map[string]any
}

func newRecord() *record { return &record{vals: map[field]string{}, meta: map[string]any{}} }

func (r *record) set(f field, key, v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	switch {
	case f == fTags:
		r.tags = append(r.tags, splitList(v)...)
	case f == fContent && r.vals[fContent] != "":
		r.vals[fContent] += "\n\n" + v
	case f == fNone || r.vals[f] != "":
		r.extra = append(r.extra, [2]string{key, v})
	default:
		r.vals[f] = v
	}
}

// item converts a record into an Item; strict requires real content or a URL.
func (p *parser) recordItem(format, file string, r *record, strict bool, extraInContent bool) (Item, bool) {
	v := r.vals
	it := Item{Format: format, Title: v[fTitle], Summary: v[fSummary], URL: v[fURL], Tags: r.tags, Meta: r.meta}
	if strict && v[fContent] == "" && it.URL == "" {
		return it, false
	}
	var b strings.Builder
	if a := v[fAuthor]; a != "" {
		it.Meta["author"] = a
		fmt.Fprintf(&b, "**Autor:** %s\n\n", a)
	}
	b.WriteString(v[fContent])
	if extraInContent && len(r.extra) > 0 {
		b.WriteString("\n")
		for _, kv := range r.extra {
			fmt.Fprintf(&b, "\n**%s:** %s", kv[0], kv[1])
		}
	} else {
		for _, kv := range r.extra {
			if _, ok := it.Meta[kv[0]]; !ok && len(kv[1]) <= 500 {
				it.Meta[kv[0]] = kv[1]
			}
		}
	}
	it.Content = strings.TrimSpace(b.String())
	if it.Title == "" && it.Content == "" && it.URL == "" {
		return it, false
	}
	if s := v[fSource]; s != "" {
		it.Meta["original_source"] = s
	}
	if d, ok := parseDate(v[fCreated], p.loc); ok {
		it.CreatedAt = d
	}
	if due := v[fDue]; due != "" {
		if d, ok := parseDate(due, p.loc); ok {
			it.DueAt = &d
		} else {
			it.Meta["due_text"] = due
		}
	}
	switch t := v[fType]; {
	case typeAliases[normKey(t)] != "":
		it.Type = typeAliases[normKey(t)]
	case database.ValidType(t):
		it.Type = t
	case it.DueAt != nil || it.Meta["due_text"] != nil:
		it.Type = database.TypeTask
	case it.URL != "" && v[fContent] == "":
		it.Type = database.TypeArticle
	default:
		it.Type = database.TypeNote
	}
	if st := v[fStatus]; st != "" {
		if it.Type == database.TypeTask {
			it.Status = database.StatusOpen
			if truthy(st) || normKey(st) == "closed" {
				it.Status = database.StatusDone
			}
		} else {
			it.Meta["status"] = st
		}
	}
	if it.URL != "" && it.Type == database.TypeArticle && !strings.Contains(it.Content, it.URL) {
		it.Content = strings.TrimSpace(it.Content + "\n\nFonte: " + it.URL)
	}
	switch {
	case v[fUID] != "":
		it.Ref = "uid:" + v[fUID]
	case v[fID] != "":
		it.Ref = "id:" + cleanName(file) + ":" + v[fID]
	case it.URL != "":
		it.Ref = it.URL
	default:
		it.Ref = hashRef(it.Title, v[fCreated], it.Content)
	}
	return it, true
}

// ---- CSV ----

func sniffDelimiter(text, name string) rune {
	if strings.HasSuffix(strings.ToLower(name), ".tsv") {
		return '\t'
	}
	line, _, _ := strings.Cut(text, "\n")
	best, bestN := ',', -1
	for _, d := range []rune{',', ';', '\t', '|'} {
		n, inQ := 0, false
		for _, r := range line {
			switch {
			case r == '"':
				inQ = !inQ
			case r == d && !inQ:
				n++
			}
		}
		if n > bestN {
			best, bestN = d, n
		}
	}
	return best
}

func (p *parser) parseCSV(data []byte, name string, strict bool) ([]Item, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = sniffDelimiter(text, name)
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: CSV inválido: %w", name, err)
	}
	cols := make([]field, len(header))
	keys := make([]string, len(header))
	hasTitle := false
	norm := map[string]bool{}
	for i, h := range header {
		keys[i] = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
		k := normKey(h)
		norm[k] = true
		cols[i] = fieldAliases[k]
		hasTitle = hasTitle || cols[i] == fTitle
	}
	if norm["type"] && norm["content"] && norm["priority"] {
		return p.parseTodoist(r, header, name)
	}
	if !hasTitle && len(cols) > 0 && cols[0] == fNone {
		cols[0] = fTitle // Notion databases and most sheets: first column is the name
	}
	var items []Item
	for line := 2; ; line++ {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return items, fmt.Errorf("%s: linha %d: %w", name, line, err)
		}
		rec := newRecord()
		for i, v := range row {
			if i < len(cols) {
				rec.set(cols[i], keys[i], v)
			} else {
				rec.set(fNone, fmt.Sprintf("col%d", i+1), v)
			}
		}
		if it, ok := p.recordItem(FormatCSV, name, rec, strict, true); ok {
			items = append(items, it)
		}
	}
	return items, nil
}

var todoistLabel = regexp.MustCompile(`(?:^|\s)@([\p{L}\p{N}_\-]+)`)

// parseTodoist handles Todoist's project CSV template (TYPE/CONTENT/PRIORITY/DATE...).
func (p *parser) parseTodoist(r *csv.Reader, header []string, name string) ([]Item, error) {
	idx := map[string]int{}
	for i, h := range header {
		idx[normKey(h)] = i
	}
	get := func(row []string, k string) string {
		if i, ok := idx[k]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	project := cleanName(name)
	var items []Item
	section := ""
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return items, fmt.Errorf("%s: %w", name, err)
		}
		content := get(row, "content")
		switch strings.ToLower(get(row, "type")) {
		case "section":
			section = content
		case "note":
			if n := len(items); n > 0 && content != "" {
				items[n-1].Content = strings.TrimSpace(items[n-1].Content + "\n\n💬 " + content)
			}
		case "task":
			if content == "" {
				continue
			}
			it := Item{Format: FormatTodoist, Type: database.TypeTask, Status: database.StatusOpen, Content: get(row, "description"),
				Tags: []string{"todoist", project}, Meta: map[string]any{"project": project}}
			for _, m := range todoistLabel.FindAllStringSubmatch(content, -1) {
				it.Tags = append(it.Tags, m[1])
			}
			it.Title = strings.TrimSpace(todoistLabel.ReplaceAllString(content, ""))
			if section != "" {
				it.Meta["section"] = section
				it.Tags = append(it.Tags, section)
			}
			if pr := get(row, "priority"); pr != "" {
				it.Meta["priority"] = pr
			}
			if d := get(row, "date"); d != "" {
				if t, ok := parseDate(d, p.loc); ok {
					it.DueAt = &t
				} else {
					it.Meta["due_text"] = d
				}
			}
			it.Ref = hashRef(project, section, it.Title)
			items = append(items, it)
		}
	}
	return items, nil
}

// ---- JSON / JSON Lines ----

var jsonContainers = []string{"nodes", "items", "notes", "data", "results", "entries", "records", "bookmarks", "highlights", "documents", "list"}

func jsonObjects(v any) []map[string]any {
	switch x := v.(type) {
	case []any:
		var out []map[string]any
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		for _, k := range jsonContainers {
			if arr, ok := x[k].([]any); ok {
				return jsonObjects(arr)
			}
		}
		return []map[string]any{x}
	}
	return nil
}

func jsonScalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

func (p *parser) parseJSON(data []byte, name string, strict bool) ([]Item, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var objs []map[string]any
	for n := 0; ; n++ {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if n == 0 {
				return nil, fmt.Errorf("%s: JSON inválido: %w", name, err)
			}
			return nil, fmt.Errorf("%s: JSON Lines inválido na linha %d: %w", name, n+1, err)
		}
		objs = append(objs, jsonObjects(v)...)
	}
	var items []Item
	for _, o := range objs {
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rec := newRecord()
		for _, k := range keys {
			f := fieldAliases[normKey(k)]
			switch x := o[k].(type) {
			case map[string]any:
				if normKey(k) == "meta" {
					for mk, mv := range x {
						rec.meta[mk] = mv
					}
				}
			case []any:
				var parts []string
				for _, e := range x {
					if s, ok := jsonScalar(e); ok {
						parts = append(parts, s)
					} else if m, ok := e.(map[string]any); ok {
						if s, ok := jsonScalar(m["name"]); ok {
							parts = append(parts, s)
						}
					}
				}
				if f == fTags {
					rec.tags = append(rec.tags, parts...)
				} else if f != fNone {
					rec.set(f, k, strings.Join(parts, "\n"))
				}
			default:
				if s, ok := jsonScalar(x); ok {
					if f == fNone && (normKey(k) == "source ref" || normKey(k) == "score" || normKey(k) == "updated at") {
						continue
					}
					rec.set(f, k, s)
				}
			}
		}
		if it, ok := p.recordItem(FormatJSON, name, rec, strict, false); ok {
			items = append(items, it)
		}
	}
	return items, nil
}
