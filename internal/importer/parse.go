package importer

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// Format identifiers (also used as the node source suffix: "import:<format>").
const (
	FormatAuto      = "auto"
	FormatMarkdown  = "markdown"
	FormatNotion    = "notion"
	FormatEvernote  = "evernote"
	FormatKeep      = "keep"
	FormatBookmarks = "bookmarks"
	FormatCSV       = "csv"
	FormatTodoist   = "todoist"
	FormatJSON      = "json"
	FormatKindle    = "kindle"
	FormatVCard     = "vcard"
	FormatICal      = "ical"
	FormatOPML      = "opml"
	FormatHTML      = "html"

	formatZip = "zip"
)

// FormatInfo documents a supported source for the UI.
type FormatInfo struct {
	ID     string
	Name   string
	Files  string
	HowTo  string
	Result string
	Select bool // selectable as a forced format for single files
}

// Formats lists the supported sources, most common first.
var Formats = []FormatInfo{
	{FormatMarkdown, "Obsidian, Logseq, Joplin, Bear, Markdown", ".md, .zip", "Compacte a pasta do vault em .zip (Obsidian/Logseq) ou exporte em Markdown (Joplin: Arquivo → Exportar → MD; Bear: Exportar → Markdown).", "notas, com frontmatter, #tags e [[links]] preservados", true},
	{FormatNotion, "Notion", ".zip", "Settings → Export all workspace content (ou ••• → Export na página) → formato Markdown & CSV.", "páginas viram notas; links entre páginas viram conexões", false},
	{FormatEvernote, "Evernote", ".enex", "Selecione notas ou um caderno → Exportar → formato ENEX.", "notas com tags, datas e checklists", true},
	{FormatKeep, "Google Keep", ".zip, .json", "takeout.google.com → desmarque tudo, marque Keep → exportar .zip.", "notas e listas (lixeira ignorada)", true},
	{FormatBookmarks, "Favoritos (Chrome, Firefox, Edge, Safari), Pocket, Raindrop", ".html", "Gerenciador de favoritos → ⋮ → Exportar favoritos (arquivo HTML).", "artigos, com a pasta como tag; opcionalmente baixa o texto das páginas", true},
	{FormatCSV, "Planilhas: Excel, Google Sheets, Todoist, Readwise, Goodreads, Pocket", ".csv, .tsv", "Salve como CSV. Colunas reconhecidas: título, conteúdo, tags, tipo, url, prazo, data, status (pt/en). Colunas extras vão para o conteúdo.", "uma nota/tarefa por linha", true},
	{FormatJSON, "JSON (API do Second Brain e genérico)", ".json, .jsonl", "Lista de objetos com campos como title, content, tags, type, url, due, created_at.", "um nó por objeto", true},
	{FormatKindle, "Kindle", "My Clippings.txt", "Conecte o Kindle via USB e copie documents/My Clippings.txt.", "um artigo por livro com todos os destaques e notas", true},
	{FormatVCard, "Contatos (Google, iPhone, Outlook)", ".vcf", "contacts.google.com → Exportar → vCard; iPhone: iCloud.com → Contatos → Exportar vCard.", "pessoas (mescladas com as já existentes)", true},
	{FormatICal, "Agenda (Google Agenda, Outlook, Apple)", ".ics", "Google Agenda → Configurações → Importar e exportar → Exportar (.zip com .ics).", "eventos e tarefas (VTODO)", true},
	{FormatOPML, "OPML: Feedly, Inoreader, outliners", ".opml", "Leitor RSS → Exportar OPML. Outliners (Workflowy, Dynalist) também exportam OPML.", "feeds vão para RSS_FEEDS; tópicos viram notas", true},
	{FormatHTML, "Páginas HTML (Notion/Evernote em HTML)", ".html", "Qualquer página salva ou exportada em HTML.", "notas com o texto principal da página", true},
}

// Item is a normalized record ready to become a node.
type Item struct {
	Format    string
	Ref       string // stable key within the format (idempotent re-imports)
	Type      string
	Title     string
	Content   string
	Summary   string
	Tags      []string
	URL       string
	Status    string
	DueAt     *time.Time
	CreatedAt time.Time
	Meta      map[string]any
	Aliases   []string // names other items may use to link here (file names, aliases)
	Links     []Link   // explicit typed links (e.g. "## Conexões" of our own export)
}

// Link is a typed reference to another item by name.
type Link struct {
	Target   string
	Relation string
}

// Batch is the result of parsing one or more files.
type Batch struct {
	Items    []Item
	Feeds    []string
	Formats  map[string]int
	Ignored  int
	Warnings []string
}

func newBatch() *Batch { return &Batch{Formats: map[string]int{}} }

func (b *Batch) addItems(items ...Item) {
	for _, it := range items {
		b.Items = append(b.Items, it)
		b.Formats[it.Format]++
	}
}

func (b *Batch) merge(o *Batch) {
	if o == nil {
		return
	}
	b.Items = append(b.Items, o.Items...)
	b.Feeds = append(b.Feeds, o.Feeds...)
	for k, v := range o.Formats {
		b.Formats[k] += v
	}
	b.Ignored += o.Ignored
	b.Warnings = append(b.Warnings, o.Warnings...)
}

func (b *Batch) warn(format string, a ...any) {
	if len(b.Warnings) < 50 {
		b.Warnings = append(b.Warnings, fmt.Sprintf(format, a...))
	}
}

// ErrUnknownFormat is returned when a file cannot be recognized.
var ErrUnknownFormat = errors.New("formato não reconhecido")

var errTooLarge = errors.New("conteúdo descompactado excede o limite de importação")

// parser holds per-import settings shared by the format readers.
type parser struct {
	loc      *time.Location
	maxBytes int64        // per-file limit
	budget   atomic.Int64 // remaining uncompressed bytes for archives
}

func newParser(loc *time.Location, maxBytes int64) *parser {
	if loc == nil {
		loc = time.Local
	}
	if maxBytes <= 0 {
		maxBytes = 200 << 20
	}
	p := &parser{loc: loc, maxBytes: maxBytes}
	p.budget.Store(max(4*maxBytes, 1<<30))
	return p
}

// parseFile reads a file from disk. format may be "" or "auto" for detection;
// archives (.zip) are always walked entry by entry with auto-detection.
func (p *parser) parseFile(ctx context.Context, filePath, name, format string) (*Batch, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > p.maxBytes {
		return nil, fmt.Errorf("%s: arquivo maior que o limite (%d MB)", name, p.maxBytes>>20)
	}
	br := bufio.NewReaderSize(f, 64<<10)
	head, _ := br.Peek(8192)
	if isZip(head) {
		return p.parseZip(ctx, f, st.Size(), 0)
	}
	kind := format
	if kind == "" || kind == FormatAuto {
		kind = detect(name, head)
	}
	if kind == "" {
		return nil, fmt.Errorf("%s: %w", name, ErrUnknownFormat)
	}
	return p.parseStream(kind, name, name, br, time.Time{}, false)
}

func isZip(head []byte) bool { return bytes.HasPrefix(head, []byte("PK\x03\x04")) }

// detect guesses the format from the file name and its first bytes.
func detect(name string, head []byte) string {
	lower := strings.ToLower(string(head))
	base := strings.ToLower(path.Base(filepath.ToSlash(name)))
	switch strings.ToLower(path.Ext(base)) {
	case ".zip":
		return formatZip
	case ".enex":
		return FormatEvernote
	case ".vcf", ".vcard":
		return FormatVCard
	case ".ics", ".ical", ".ifb":
		return FormatICal
	case ".opml":
		return FormatOPML
	case ".csv", ".tsv":
		return FormatCSV
	case ".md", ".markdown", ".mdown":
		return FormatMarkdown
	case ".jsonl", ".ndjson":
		return FormatJSON
	case ".json":
		if isKeepJSON(lower) {
			return FormatKeep
		}
		return FormatJSON
	case ".html", ".htm":
		if isBookmarks(lower) {
			return FormatBookmarks
		}
		return FormatHTML
	case ".txt":
		if isKindle(base, lower) {
			return FormatKindle
		}
		return FormatMarkdown
	}
	t := strings.TrimSpace(strings.TrimPrefix(lower, "\ufeff"))
	switch {
	case isZip(head):
		return formatZip
	case strings.Contains(t, "<en-export"):
		return FormatEvernote
	case strings.HasPrefix(t, "begin:vcard"):
		return FormatVCard
	case strings.HasPrefix(t, "begin:vcalendar"):
		return FormatICal
	case strings.Contains(t, "<opml"):
		return FormatOPML
	case isBookmarks(t):
		return FormatBookmarks
	case isKeepJSON(t):
		return FormatKeep
	case strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{"):
		return FormatJSON
	}
	return ""
}

func isKeepJSON(lower string) bool {
	return strings.Contains(lower, `"usereditedtimestampusec"`) || (strings.Contains(lower, `"textcontent"`) && strings.Contains(lower, `"istrashed"`))
}

func isBookmarks(lower string) bool {
	return strings.Contains(lower, "netscape-bookmark-file") || strings.Contains(lower, "<title>pocket export") ||
		(strings.Contains(lower, "<dt><a ") && strings.Contains(lower, "href="))
}

func isKindle(base, lower string) bool {
	return strings.Contains(base, "clippings") || (strings.Contains(lower, "==========") && strings.Contains(lower, "\n- "))
}

// parseStream parses a single (non-archive) document. strict drops records without
// real content (used inside archives, where unrelated JSON/CSV files are common).
func (p *parser) parseStream(kind, name, relPath string, r io.Reader, mod time.Time, strict bool) (*Batch, error) {
	b := newBatch()
	switch kind {
	case FormatEvernote:
		items, err := p.parseENEX(r, name)
		b.addItems(items...)
		return b, err
	}
	data, err := io.ReadAll(io.LimitReader(r, p.maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > p.maxBytes {
		return nil, fmt.Errorf("%s: arquivo maior que o limite (%d MB)", name, p.maxBytes>>20)
	}
	switch kind {
	case FormatMarkdown, FormatNotion:
		b.addItems(p.markdownItem(kind, relPath, string(data), mod))
	case FormatHTML:
		it, err := p.htmlItem(relPath, string(data), mod)
		if err != nil {
			return nil, err
		}
		b.addItems(it)
	case FormatKeep:
		items, err := p.parseKeep(data, name)
		if err != nil {
			return nil, err
		}
		b.addItems(items...)
	case FormatBookmarks:
		b.addItems(p.parseBookmarks(string(data))...)
	case FormatCSV:
		items, err := p.parseCSV(data, name, strict)
		if err != nil {
			return nil, err
		}
		b.addItems(items...)
	case FormatJSON:
		items, err := p.parseJSON(data, name, strict)
		if err != nil {
			return nil, err
		}
		b.addItems(items...)
	case FormatKindle:
		b.addItems(p.parseKindle(string(data))...)
	case FormatVCard:
		b.addItems(p.parseVCard(string(data))...)
	case FormatICal:
		b.addItems(p.parseICal(string(data))...)
	case FormatOPML:
		items, feeds, err := p.parseOPML(data, name)
		if err != nil {
			return nil, err
		}
		b.addItems(items...)
		b.Feeds = append(b.Feeds, feeds...)
	default:
		return nil, fmt.Errorf("%s: %w (%s)", name, ErrUnknownFormat, kind)
	}
	return b, nil
}

// ---- archives ----

// skipPath filters metadata folders of common apps and OS junk.
func skipPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, ".") || part == "__MACOSX" || part == "node_modules" {
			return true
		}
	}
	lp := strings.ToLower(p)
	return strings.HasPrefix(lp, "logseq/") || strings.Contains(lp, "/logseq/bak/") || strings.Contains(lp, "/logseq/version-files/")
}

// junkFiles are index/metadata files of common exports (Takeout, Notion, Keep).
var junkFiles = map[string]bool{"archive_browser.html": true, "index.html": true, "labels.txt": true}

type zipEntry struct {
	f    *zip.File
	name string // full path inside the archive
	rel  string // path without the archive's single top-level folder (stable refs)
}

type countingReader struct {
	r      io.Reader
	budget *atomic.Int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if c.budget.Add(-int64(n)) < 0 {
		return n, errTooLarge
	}
	return n, err
}

// parseZip walks an archive, parsing entries concurrently (one goroutine per CPU).
func (p *parser) parseZip(ctx context.Context, ra io.ReaderAt, size int64, depth int) (*Batch, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("zip inválido: %w", err)
	}
	out := newBatch()
	var entries []zipEntry
	hasMD := false
	jsonBase := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(f.Name), "/")
		if f.FileInfo().IsDir() || skipPath(name) {
			continue
		}
		ext := strings.ToLower(path.Ext(name))
		switch ext {
		case ".md", ".markdown":
			hasMD = true
		case ".json":
			jsonBase[strings.TrimSuffix(name, path.Ext(name))] = true
		}
		entries = append(entries, zipEntry{f: f, name: name, rel: name})
	}
	// "Vault/…" and "MyVault/…" zips of the same notes must produce the same refs.
	if len(entries) > 0 {
		root, _, ok := strings.Cut(entries[0].name, "/")
		for _, e := range entries {
			ok = ok && strings.HasPrefix(e.name, root+"/")
		}
		if ok {
			for i := range entries {
				entries[i].rel = strings.TrimPrefix(entries[i].name, root+"/")
			}
		}
	}

	results := make([]*Batch, len(entries))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(runtime.NumCPU())
	for i, e := range entries {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			b, err := p.parseEntry(gctx, e, depth, hasMD, jsonBase)
			if errors.Is(err, errTooLarge) {
				return err
			}
			if err != nil {
				b = newBatch()
				b.warn("%s: %v", e.name, err)
			}
			results[i] = b
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for _, b := range results {
		out.merge(b)
	}
	return out, nil
}

func (p *parser) parseEntry(ctx context.Context, e zipEntry, depth int, hasMD bool, jsonBase map[string]bool) (*Batch, error) {
	base := strings.ToLower(path.Base(e.name))
	ext := strings.ToLower(path.Ext(base))
	ignored := func() (*Batch, error) {
		b := newBatch()
		b.Ignored = 1
		return b, nil
	}
	if junkFiles[base] {
		return ignored()
	}
	switch ext {
	case ".md", ".markdown", ".mdown", ".txt", ".enex", ".vcf", ".vcard", ".ics", ".opml", ".csv", ".tsv", ".json", ".jsonl", ".ndjson", ".html", ".htm", ".zip", ".xml":
	default:
		return ignored()
	}
	if ext == ".csv" && hasMD {
		return ignored() // Notion databases: rows already exported as pages
	}
	if (ext == ".html" || ext == ".htm") && jsonBase[strings.TrimSuffix(e.name, path.Ext(e.name))] {
		return ignored() // Keep: .html duplicates the .json note
	}
	rc, err := e.f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	r := bufio.NewReaderSize(&countingReader{r: rc, budget: &p.budget}, 64<<10)
	if ext == ".zip" {
		if depth >= 2 {
			return ignored()
		}
		data, err := io.ReadAll(io.LimitReader(r, p.maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > p.maxBytes {
			return nil, errTooLarge
		}
		return p.parseZip(ctx, bytes.NewReader(data), int64(len(data)), depth+1)
	}
	head, _ := r.Peek(8192)
	kind := detect(e.name, head)
	switch {
	case kind == "":
		return ignored()
	case kind == FormatMarkdown && notionID.MatchString(strings.TrimSuffix(path.Base(e.name), path.Ext(e.name))):
		kind = FormatNotion
	}
	return p.parseStream(kind, path.Base(e.name), e.rel, r, e.f.Modified, true)
}
