package agent

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/queue"
)

// MediaInput is a file captured from any channel.
type MediaInput struct {
	Data      []byte
	Filename  string
	MIME      string
	Caption   string
	Source    string
	SourceRef string
	Meta      map[string]any
	Voice     bool // quick voice capture: may be re-typed into a task/insight
}

// DetectMIME guesses the MIME type from name/content.
func DetectMIME(name string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown"
	case ".txt", ".log":
		return "text/plain"
	case ".oga", ".ogg", ".opus":
		return "audio/ogg"
	case ".m4a":
		return "audio/mp4"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if i := strings.Index(t, ";"); i > 0 {
			t = t[:i]
		}
		return t
	}
	if len(data) > 4 && string(data[:4]) == "%PDF" {
		return "application/pdf"
	}
	return "application/octet-stream"
}

// MediaDir is where original images/documents are kept for reference.
func (a *Agent) MediaDir() string { return filepath.Join(a.cfg.GetPath("DATA_DIR"), "media") }

// PurgeTrash permanently deletes trashed nodes (ids, or with ids nil everything deleted
// before the cutoff) together with their media files.
func (a *Agent) PurgeTrash(ctx context.Context, ids []int64, before time.Time) (int, error) {
	files, n, err := a.db.PurgeTrash(ctx, ids, before)
	for _, f := range files {
		os.Remove(filepath.Join(a.MediaDir(), filepath.Base(f)))
	}
	return n, err
}

func (a *Agent) keepFile(data []byte, name string) string {
	dir := a.MediaDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	fn := crypto.RandomToken(6) + strings.ToLower(filepath.Ext(name))
	if err := os.WriteFile(filepath.Join(dir, fn), data, 0o644); err != nil {
		return ""
	}
	return fn
}

// IngestMedia converts a file (image, PDF, audio, text, HTML) into an enriched node.
func (a *Agent) IngestMedia(ctx context.Context, in MediaInput) (*database.Node, error) {
	n, err := a.ingestMedia(ctx, in)
	if err == nil && (strings.HasPrefix(in.MIME, "image/") || in.MIME == "application/pdf") {
		a.afterReceipt(ctx, in, n) // purchase documents become warranties in the profile
	}
	return n, err
}

func (a *Agent) ingestMedia(ctx context.Context, in MediaInput) (*database.Node, error) {
	if in.MIME == "" {
		in.MIME = DetectMIME(in.Filename, in.Data)
	}
	if in.Meta == nil {
		in.Meta = map[string]any{}
	}
	in.Meta["filename"] = in.Filename
	base := strings.TrimSuffix(filepath.Base(in.Filename), filepath.Ext(in.Filename))
	switch {
	case strings.HasPrefix(in.MIME, "image/"):
		return a.ingestImage(ctx, in, base)
	case in.MIME == "application/pdf":
		return a.ingestPDF(ctx, in, base)
	case strings.HasPrefix(in.MIME, "audio/"):
		return a.ingestAudio(ctx, in)
	case in.MIME == "text/html":
		art, err := extract.Readability(string(in.Data))
		if err != nil {
			return nil, queue.Permanent(err)
		}
		n, _, err := a.Ingest(ctx, IngestInput{Type: database.TypeArticle, Title: art.Title, Content: art.Text, Source: in.Source, SourceRef: in.SourceRef, Meta: in.Meta, Enrich: true})
		return n, err
	case strings.HasPrefix(in.MIME, "text/") || in.MIME == "application/json":
		return a.ingestText(ctx, in, base)
	}
	return nil, queue.Permanentf("tipo de arquivo não suportado: %s", in.MIME)
}

func (a *Agent) ingestText(ctx context.Context, in MediaInput, base string) (*database.Node, error) {
	fm, body := ParseFrontmatter(string(in.Data))
	title := fm["title"]
	if title == "" {
		if h := firstHeading(body); h != "" {
			title = h
		} else {
			title = base
		}
	}
	typ := fm["type"]
	if !database.ValidType(typ) {
		typ = database.TypeNote
	}
	var tags []string
	if t := strings.Trim(fm["tags"], "[]"); t != "" {
		for _, x := range strings.Split(t, ",") {
			tags = append(tags, strings.Trim(strings.TrimSpace(x), `"'`))
		}
	}
	for k, v := range fm {
		if k != "title" && k != "tags" && k != "type" {
			in.Meta[k] = v
		}
	}
	if in.Caption != "" {
		body = in.Caption + "\n\n" + body
	}
	n, _, err := a.Ingest(ctx, IngestInput{Type: typ, Title: title, Content: body, Tags: tags, Source: in.Source, SourceRef: in.SourceRef, Meta: in.Meta, Enrich: true})
	return n, err
}

func firstHeading(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "# ") {
			return strings.TrimSpace(l[2:])
		}
	}
	return ""
}

// ParseFrontmatter splits a Markdown document's simple YAML frontmatter.
func ParseFrontmatter(s string) (map[string]string, string) {
	fm := map[string]string{}
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return fm, s
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, s
	}
	for _, l := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		fm[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	body := rest[end+4:]
	body = strings.TrimLeft(body, "-\r\n")
	return fm, body
}

type visionResult struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Text        string   `json:"text"`
	Tables      string   `json:"tables_markdown"`
	Tags        []string `json:"tags"`
}

const visionPrompt = `Analise a imagem e responda SOMENTE JSON:
{"title":"título curto","description":"descrição objetiva do que aparece","text":"todo texto legível (OCR fiel, preserve quebras de linha)","tables_markdown":"tabelas/dados tabulares em Markdown, ou vazio","tags":["até 6 tags"]}
Se for um documento (recibo, nota fiscal, boleto, formulário), extraia campos-chave (datas, valores, nomes) na descrição.`

func (a *Agent) ingestImage(ctx context.Context, in MediaInput, base string) (*database.Node, error) {
	kept := a.keepFile(in.Data, in.Filename)
	if kept != "" {
		in.Meta["file"] = kept
	}
	if !a.llm.Enabled() {
		n, _, err := a.Ingest(ctx, IngestInput{Title: firstNonEmpty(in.Caption, base), Content: in.Caption, Source: in.Source, SourceRef: in.SourceRef, Meta: in.Meta, Tags: []string{"imagem"}, Enrich: true})
		return n, err
	}
	var vr visionResult
	resp, err := a.llm.Multimodal(ctx, llm.Request{
		Purpose: "vision", JSON: true, MaxTokens: 6000,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: visionPrompt + captionHint(in.Caption), Parts: []llm.Part{{Type: llm.PartImage, MIME: in.MIME, Data: in.Data, Name: in.Filename}}}},
	})
	if err != nil {
		return nil, err
	}
	if jerr := jsonUnmarshal(llm.ExtractJSON(resp.Text), &vr); jerr != nil {
		vr.Description = resp.Text
	}
	var b strings.Builder
	if in.Caption != "" {
		b.WriteString("## Legenda\n" + in.Caption + "\n\n")
	}
	if vr.Description != "" {
		b.WriteString("## Descrição\n" + vr.Description + "\n\n")
	}
	if strings.TrimSpace(vr.Text) != "" {
		b.WriteString("## Texto (OCR)\n" + vr.Text + "\n\n")
	}
	if strings.TrimSpace(vr.Tables) != "" {
		b.WriteString("## Tabelas\n" + vr.Tables + "\n")
	}
	n, _, err := a.Ingest(ctx, IngestInput{
		Title: firstNonEmpty(vr.Title, in.Caption, base), Content: strings.TrimSpace(b.String()), Summary: extract.Truncate(vr.Description, 300),
		Tags: append(vr.Tags, "imagem"), Source: in.Source, SourceRef: in.SourceRef, Meta: in.Meta, Enrich: true,
	})
	return n, err
}

func captionHint(c string) string {
	if c == "" {
		return ""
	}
	return "\nLegenda do usuário: " + c
}

func (a *Agent) ingestPDF(ctx context.Context, in MediaInput, base string) (*database.Node, error) {
	if kept := a.keepFile(in.Data, in.Filename); kept != "" {
		in.Meta["file"] = kept
	}
	text, _ := extract.PDFBytes(in.Data)
	if len(strings.TrimSpace(text)) < 200 && a.llm.Enabled() {
		resp, err := a.llm.Multimodal(ctx, llm.Request{
			Purpose: "pdf-ocr", MaxTokens: 16000,
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "Transcreva o conteúdo deste documento em Markdown fiel, incluindo tabelas. Não resuma.",
				Parts: []llm.Part{{Type: llm.PartFile, MIME: "application/pdf", Data: in.Data, Name: in.Filename}}}},
		})
		if err != nil {
			return nil, err
		}
		text = resp.Text
	}
	if in.Caption != "" {
		text = in.Caption + "\n\n" + text
	}
	n, _, err := a.Ingest(ctx, IngestInput{Title: base, Content: text, Tags: []string{"documento", "pdf"}, Source: in.Source, SourceRef: in.SourceRef, Meta: in.Meta, Enrich: true})
	return n, err
}

func (a *Agent) ingestAudio(ctx context.Context, in MediaInput) (*database.Node, error) {
	if !a.llm.Enabled() {
		return nil, queue.Permanent(llm.ErrNoProvider)
	}
	text, err := a.llm.Transcribe(ctx, in.Data, in.Filename, in.MIME)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, queue.Permanent(errors.New("transcrição vazia"))
	}
	in.Meta["transcribed"] = true
	if in.Voice {
		in.Meta["quick"] = true
	}
	content := text
	if in.Caption != "" {
		content = in.Caption + "\n\n" + text
	}
	n, _, err := a.Ingest(ctx, IngestInput{Title: extract.Truncate(text, 60), Content: content, Tags: []string{"voz"}, Source: in.Source, SourceRef: in.SourceRef, Meta: in.Meta, Enrich: true})
	return n, err
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return "Sem título"
}

// ---- web clipper ----

// ClipInput is a web clipper payload.
type ClipInput struct {
	URL   string   `json:"url"`
	Title string   `json:"title"`
	HTML  string   `json:"html"`
	Text  string   `json:"text"`
	Note  string   `json:"note"`
	Tags  []string `json:"tags"`
}

// Clip stores a clipped page; missing content is fetched asynchronously (offline-safe).
func (a *Agent) Clip(ctx context.Context, in ClipInput) (*database.Node, error) {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("URL inválida")
	}
	content := strings.TrimSpace(in.Text)
	if in.HTML != "" {
		content = extract.HTMLToText(in.HTML)
	}
	if in.Note != "" {
		content = "> " + strings.ReplaceAll(in.Note, "\n", "\n> ") + "\n\n" + content
	}
	title := firstNonEmpty(in.Title, u.Host+u.Path)
	meta := map[string]any{"url": u.String(), "site": u.Host, "selection": in.HTML != "" || in.Text != ""}
	n, _, err := a.Ingest(ctx, IngestInput{
		Type: database.TypeArticle, Title: title, Content: content + "\n\nFonte: " + u.String(), Tags: append(in.Tags, "clip"),
		Source: "clip", SourceRef: u.String(), Meta: meta, Enrich: in.HTML != "" || in.Text != "",
	})
	if err != nil {
		return nil, err
	}
	if in.HTML == "" && in.Text == "" {
		_, err = a.db.Enqueue(ctx, TaskClipFetch, map[string]int64{"id": n.ID}, database.EnqueueOpts{DedupeKey: fmt.Sprintf("clip:%d", n.ID), MaxAttempts: 5})
	}
	return n, err
}

func (a *Agent) fetchClip(ctx context.Context, id int64) error {
	n, err := a.db.GetNode(ctx, id)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	u, _ := n.Meta["url"].(string)
	if u == "" {
		u = n.SourceRef
	}
	art, err := extract.Fetch(ctx, u)
	if err != nil {
		return err
	}
	if art.Title != "" && (n.Title == "" || strings.Contains(n.Title, "/")) {
		n.Title = extract.Truncate(art.Title, 200)
	}
	note := ""
	if i := strings.Index(n.Content, "\n\nFonte: "); i > 0 {
		note = n.Content[:i] + "\n\n"
	}
	n.Content = note + art.Text + "\n\nFonte: " + u
	if art.Byline != "" {
		n.Meta["byline"] = art.Byline
	}
	if art.Description != "" && n.Summary == "" {
		n.Summary = art.Description
	}
	if err := a.db.UpdateNode(ctx, n); err != nil {
		return err
	}
	return a.QueueEnrich(ctx, n.ID)
}
