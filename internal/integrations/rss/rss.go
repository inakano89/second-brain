// Package rss polls RSS/Atom feeds, scores relevance with the LLM and stores
// summaries of the most relevant items as article nodes.
package rss

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/queue"
)

// TaskPoll is the queue kind for a feed poll.
const TaskPoll = "rss.poll"

// Item is a normalized feed entry.
type Item struct {
	GUID        string
	Title       string
	Link        string
	Description string
	Published   time.Time
	Feed        string
}

// Feed is a parsed feed.
type Feed struct {
	Title string
	Items []Item
}

type rssDoc struct {
	Channel struct {
		Title string `xml:"title"`
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			GUID        string `xml:"guid"`
			Description string `xml:"description"`
			Content     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
			PubDate     string `xml:"pubDate"`
			Date        string `xml:"http://purl.org/dc/elements/1.1/ date"`
		} `xml:"item"`
	} `xml:"channel"`
	Items []struct { // RSS 1.0 (RDF)
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		Description string `xml:"description"`
		Date        string `xml:"http://purl.org/dc/elements/1.1/ date"`
	} `xml:"item"`
}

type atomDoc struct {
	Title   string `xml:"title"`
	Entries []struct {
		Title string `xml:"title"`
		ID    string `xml:"id"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Summary   string `xml:"summary"`
		Content   string `xml:"content"`
		Updated   string `xml:"updated"`
		Published string `xml:"published"`
	} `xml:"entry"`
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, l := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Parse decodes RSS 2.0, RSS 1.0 (RDF) or Atom.
func Parse(data []byte) (*Feed, error) {
	newDec := func() *xml.Decoder {
		d := xml.NewDecoder(strings.NewReader(string(data)))
		d.CharsetReader = charset.NewReaderLabel
		d.Strict = false
		return d
	}
	var root struct{ XMLName xml.Name }
	if err := newDec().Decode(&root); err != nil {
		return nil, err
	}
	f := &Feed{}
	switch strings.ToLower(root.XMLName.Local) {
	case "feed":
		var a atomDoc
		if err := newDec().Decode(&a); err != nil {
			return nil, err
		}
		f.Title = a.Title
		for _, e := range a.Entries {
			link := ""
			for _, l := range e.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = l.Href
					break
				}
			}
			desc := e.Content
			if desc == "" {
				desc = e.Summary
			}
			pub := parseDate(e.Published)
			if pub.IsZero() {
				pub = parseDate(e.Updated)
			}
			f.Items = append(f.Items, Item{GUID: firstNonEmpty(e.ID, link), Title: e.Title, Link: link, Description: desc, Published: pub})
		}
	default:
		var r rssDoc
		if err := newDec().Decode(&r); err != nil {
			return nil, err
		}
		f.Title = r.Channel.Title
		for _, it := range r.Channel.Items {
			desc := it.Content
			if desc == "" {
				desc = it.Description
			}
			pub := parseDate(it.PubDate)
			if pub.IsZero() {
				pub = parseDate(it.Date)
			}
			f.Items = append(f.Items, Item{GUID: firstNonEmpty(it.GUID, it.Link), Title: it.Title, Link: strings.TrimSpace(it.Link), Description: desc, Published: pub})
		}
		for _, it := range r.Items {
			f.Items = append(f.Items, Item{GUID: it.Link, Title: it.Title, Link: strings.TrimSpace(it.Link), Description: it.Description, Published: parseDate(it.Date)})
		}
	}
	for i := range f.Items {
		f.Items[i].Feed = f.Title
		f.Items[i].Title = strings.TrimSpace(f.Items[i].Title)
	}
	return f, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// Poller fetches configured feeds concurrently.
type Poller struct {
	cfg  *config.Config
	ag   *agent.Agent
	log  *slog.Logger
	http *http.Client
}

// New creates a poller.
func New(cfg *config.Config, ag *agent.Agent, log *slog.Logger) *Poller {
	return &Poller{cfg: cfg, ag: ag, log: log.With("component", "rss"), http: &http.Client{Timeout: 30 * time.Second}}
}

// RegisterTasks wires the queue handler.
func (p *Poller) RegisterTasks(w *queue.Worker) {
	w.Handle(TaskPoll, func(ctx context.Context, _ json.RawMessage) error {
		n, err := p.Poll(ctx)
		if err == nil && n > 0 {
			p.log.Info("itens RSS relevantes importados", "count", n)
		}
		return err
	})
}

func (p *Poller) fetch(ctx context.Context, url string) (*Feed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "SecondBrain/1.0 RSS")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

type scored struct {
	Index   int      `json:"i"`
	Score   float64  `json:"score"`
	Summary string   `json:"summary"`
	Tags    []string `json:"tags"`
}

// Poll fetches all feeds in parallel, dedupes, scores and stores relevant items.
func (p *Poller) Poll(ctx context.Context) (int, error) {
	feeds := p.cfg.GetList("RSS_FEEDS")
	if len(feeds) == 0 {
		return 0, nil
	}
	db := p.ag.DB()
	var mu sync.Mutex
	var fresh []Item
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	failures := 0
	for _, u := range feeds {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f, err := p.fetch(ctx, u)
			if err != nil {
				p.log.Warn("feed falhou", "url", u, "err", err)
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			for _, it := range f.Items {
				key := firstNonEmpty(it.GUID, it.Link, it.Title)
				if key == "" {
					continue
				}
				if seen, _ := db.Seen(ctx, "rss", key); seen {
					continue
				}
				if !it.Published.IsZero() && time.Since(it.Published) > 7*24*time.Hour {
					db.MarkSeen(ctx, "rss", key)
					continue
				}
				it.GUID = key
				mu.Lock()
				fresh = append(fresh, it)
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	if failures == len(feeds) {
		return 0, fmt.Errorf("rss: todos os %d feeds falharam", failures)
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Published.After(fresh[j].Published) })
	if max := p.cfg.GetInt("RSS_MAX_ITEMS", 20); len(fresh) > max {
		fresh = fresh[:max]
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	scores, err := p.score(ctx, fresh)
	if err != nil {
		return 0, err
	}
	minScore := p.cfg.GetFloat("RSS_MIN_SCORE", 6)
	stored := 0
	for i, it := range fresh {
		s := scores[i]
		if s.Score >= minScore {
			desc := extract.HTMLToText(it.Description)
			content := fmt.Sprintf("%s\n\n---\n%s\n\nFonte: %s (%s)", s.Summary, extract.Truncate(desc, 4000), it.Link, it.Feed)
			_, _, err := p.ag.Ingest(ctx, agent.IngestInput{
				Type: database.TypeArticle, Title: it.Title, Content: content, Summary: s.Summary, Tags: append(s.Tags, "rss"),
				Source: "rss", SourceRef: it.GUID, CreatedAt: it.Published,
				Meta: map[string]any{"url": it.Link, "feed": it.Feed, "relevance": s.Score}, Enrich: true,
			})
			if err != nil {
				return stored, err
			}
			stored++
		}
		db.MarkSeen(ctx, "rss", it.GUID)
	}
	return stored, nil
}

// score asks the LLM to rate items in one batch; without LLM every item scores 10.
func (p *Poller) score(ctx context.Context, items []Item) ([]scored, error) {
	out := make([]scored, len(items))
	for i := range out {
		out[i] = scored{Index: i, Score: 10, Summary: extract.Truncate(extract.HTMLToText(items[i].Description), 300)}
	}
	if !p.ag.LLM().Enabled() {
		return out, nil
	}
	var b strings.Builder
	for i, it := range items {
		fmt.Fprintf(&b, "[%d] %s — %s\n%s\n\n", i, it.Title, it.Feed, extract.Truncate(extract.HTMLToText(it.Description), 600))
	}
	interests := p.cfg.Get("RSS_INTERESTS")
	if interests == "" {
		interests = "tecnologia, produtividade, ciência e assuntos relevantes para o usuário"
	}
	var res struct {
		Items []scored `json:"items"`
	}
	err := p.ag.LLM().CompleteJSON(ctx, "", llm.Request{
		Purpose: "rss", MaxTokens: 8000, Effort: "low",
		System: `Você é um curador de notícias. Para cada item, dê nota de relevância 0-10 considerando os interesses do usuário, um resumo de 1-2 frases e até 4 tags.
Responda SOMENTE JSON: {"items":[{"i":0,"score":7,"summary":"...","tags":["..."]}]}`,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "Interesses: " + interests + "\n\nItens:\n" + b.String()}},
	}, &res)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Score = 0 // items omitted by the model are treated as irrelevant
	}
	for _, s := range res.Items {
		if s.Index >= 0 && s.Index < len(out) {
			out[s.Index] = s
		}
	}
	return out, nil
}
