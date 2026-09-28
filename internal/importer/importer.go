// Package importer brings data from other apps into the graph: note apps (Obsidian,
// Logseq, Notion, Evernote, Google Keep, Joplin, Bear), browser bookmarks and Pocket,
// spreadsheets (CSV, Todoist, Readwise), JSON, Kindle highlights, contacts (vCard),
// calendars (iCalendar) and OPML feed lists. Parsing and ingestion run on goroutine pools.
package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
)

// SourcePrefix prefixes the node source of imported items ("import:evernote").
const SourcePrefix = "import:"

const ingestWorkers = 4

// Options control one import.
type Options struct {
	Format string   // "" / "auto" detects per file
	LLM    bool     // run LLM analysis (summary, tags, entities) on each item
	Fetch  bool     // bookmarks: download the page text
	Tags   []string // extra tags applied to every item

	MaxBytes int64 // per-file limit; 0 uses IMPORT_MAX_MB
}

// File is an input file on disk; Name is the original file name.
type File struct {
	Path string
	Name string
}

// Job states.
const (
	StateQueued    = "queued"
	StateParsing   = "parsing"
	StateImporting = "importing"
	StateLinking   = "linking"
	StateDone      = "done"
	StateFailed    = "failed"
)

// Report summarizes an import.
type Report struct {
	ID       string         `json:"id"`
	Files    []string       `json:"files"`
	State    string         `json:"state"`
	Formats  map[string]int `json:"formats"`
	Total    int            `json:"total"`
	Done     int            `json:"done"`
	Created  int            `json:"created"`
	Updated  int            `json:"updated"`
	Skipped  int            `json:"skipped"`
	Deleted  int            `json:"deleted"` // deleted before by the user ("não trazer de volta")
	Failed   int            `json:"failed"`
	Ignored  int            `json:"ignored"`
	Links    int            `json:"links"`
	Feeds    int            `json:"feeds"`
	Queued   int            `json:"queued"`
	Errors   []string       `json:"errors,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished,omitzero"`
}

// Running reports whether the job is still in progress.
func (r Report) Running() bool { return r.State != StateDone && r.State != StateFailed }

// Percent is the ingestion progress (0-100).
func (r Report) Percent() int {
	if r.Total == 0 {
		if r.Running() {
			return 0
		}
		return 100
	}
	return r.Done * 100 / r.Total
}

// FormatList returns "evernote (12), markdown (3)" sorted by count.
func (r Report) FormatList() string {
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range r.Formats {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v || (kvs[i].v == kvs[j].v && kvs[i].k < kvs[j].k) })
	var out []string
	for _, e := range kvs {
		out = append(out, fmt.Sprintf("%s (%d)", e.k, e.v))
	}
	return strings.Join(out, ", ")
}

// Job is an import running in the background.
type Job struct {
	mu  sync.Mutex
	rep Report
}

// Report returns a snapshot of the job state.
func (j *Job) Report() Report {
	j.mu.Lock()
	defer j.mu.Unlock()
	r := j.rep
	r.Files = append([]string(nil), r.Files...)
	r.Errors = append([]string(nil), r.Errors...)
	r.Warnings = append([]string(nil), r.Warnings...)
	fm := make(map[string]int, len(r.Formats))
	for k, v := range r.Formats {
		fm[k] = v
	}
	r.Formats = fm
	return r
}

func (j *Job) update(fn func(r *Report)) {
	j.mu.Lock()
	fn(&j.rep)
	j.mu.Unlock()
}

func (j *Job) errorf(format string, a ...any) {
	j.update(func(r *Report) {
		if len(r.Errors) < 30 {
			r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
		}
	})
}

// Importer parses files and ingests their items through the agent.
type Importer struct {
	cfg *config.Config
	ag  *agent.Agent
	db  *database.DB
	log *slog.Logger

	mu    sync.Mutex
	jobs  map[string]*Job
	order []string

	// Done runs after a queued file import (archive inbox files, notify).
	Done func(ctx context.Context, t FileTask, rep Report)
}

// New creates an importer and removes stale uploads from previous runs.
func New(cfg *config.Config, ag *agent.Agent, log *slog.Logger) *Importer {
	im := &Importer{cfg: cfg, ag: ag, db: ag.DB(), log: log.With("component", "import"), jobs: map[string]*Job{}}
	if entries, err := os.ReadDir(im.UploadDir()); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				os.Remove(filepath.Join(im.UploadDir(), e.Name()))
			}
		}
	}
	return im
}

// UploadDir is where web uploads wait for their background job.
func (im *Importer) UploadDir() string { return filepath.Join(im.cfg.GetPath("DATA_DIR"), "imports") }

// MaxBytes is the per-file size limit (IMPORT_MAX_MB).
func (im *Importer) MaxBytes() int64 {
	mb := im.cfg.GetInt("IMPORT_MAX_MB", 200)
	if mb <= 0 {
		mb = 200
	}
	return int64(mb) << 20
}

// Start runs an import in the background; files are deleted when remove is true.
func (im *Importer) Start(files []File, opt Options, remove bool) *Job {
	j := im.newJob(files)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		im.run(ctx, j, files, opt)
		if remove {
			for _, f := range files {
				os.Remove(f.Path)
			}
		}
	}()
	return j
}

// Run imports synchronously (API and CLI).
func (im *Importer) Run(ctx context.Context, files []File, opt Options) Report {
	j := im.newJob(files)
	im.run(ctx, j, files, opt)
	return j.Report()
}

// Job returns a job by id.
func (im *Importer) Job(id string) (*Job, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	j, ok := im.jobs[id]
	return j, ok
}

// Jobs returns snapshots of recent jobs, newest first.
func (im *Importer) Jobs() []Report {
	im.mu.Lock()
	jobs := make([]*Job, 0, len(im.order))
	for i := len(im.order) - 1; i >= 0; i-- {
		jobs = append(jobs, im.jobs[im.order[i]])
	}
	im.mu.Unlock()
	out := make([]Report, len(jobs))
	for i, j := range jobs {
		out[i] = j.Report()
	}
	return out
}

func (im *Importer) newJob(files []File) *Job {
	j := &Job{rep: Report{ID: crypto.RandomToken(6), State: StateQueued, Formats: map[string]int{}, Started: time.Now()}}
	for _, f := range files {
		j.rep.Files = append(j.rep.Files, f.Name)
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	im.jobs[j.rep.ID] = j
	im.order = append(im.order, j.rep.ID)
	for len(im.order) > 20 {
		delete(im.jobs, im.order[0])
		im.order = im.order[1:]
	}
	return j
}

func (im *Importer) run(ctx context.Context, j *Job, files []File, opt Options) {
	defer func() {
		if r := recover(); r != nil {
			j.errorf("falha interna: %v", r)
			j.update(func(rep *Report) { rep.State, rep.Finished = StateFailed, time.Now() })
		}
	}()
	j.update(func(r *Report) { r.State = StateParsing })
	batch, err := im.parse(ctx, files, opt)
	if err != nil && (batch == nil || len(batch.Items)+len(batch.Feeds) == 0) {
		j.errorf("%v", err)
		j.update(func(r *Report) { r.State, r.Finished = StateFailed, time.Now() })
		im.log.Warn("importação falhou", "files", j.rep.Files, "err", err)
		return
	}
	if err != nil {
		j.errorf("%v", err)
	}
	im.ingest(ctx, j, batch, opt)
	rep := j.Report()
	im.log.Info("importação concluída", "files", rep.Files, "formats", rep.FormatList(), "created", rep.Created, "updated", rep.Updated,
		"skipped", rep.Skipped, "failed", rep.Failed, "links", rep.Links, "feeds", rep.Feeds)
}

// parse reads every file concurrently and merges the batches.
func (im *Importer) parse(ctx context.Context, files []File, opt Options) (*Batch, error) {
	limit := opt.MaxBytes
	if limit <= 0 {
		limit = im.MaxBytes()
	}
	p := newParser(im.cfg.Location(), limit)
	batches := make([]*Batch, len(files))
	errs := make([]error, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batches[i], errs[i] = p.parseFile(ctx, f.Path, f.Name, opt.Format)
		}()
	}
	wg.Wait()
	out := newBatch()
	for _, b := range batches {
		out.merge(b)
	}
	out.merge(p.takeoutItems())
	return out, errors.Join(errs...)
}

type result struct {
	id      int64
	item    *Item
	outcome int
}

const (
	outCreated = iota + 1
	outUpdated
	outSkipped
	outDeleted
)

// ingest stores items (goroutine pool), resolves links, then queues enrichment so
// wiki-links find their targets.
func (im *Importer) ingest(ctx context.Context, j *Job, b *Batch, opt Options) {
	items := dedupe(b.Items)
	j.update(func(r *Report) {
		r.State, r.Total, r.Ignored, r.Warnings = StateImporting, len(items), b.Ignored, b.Warnings
		for k, v := range b.Formats {
			r.Formats[k] = v
		}
	})
	if len(b.Feeds) > 0 {
		n, err := im.addFeeds(b.Feeds)
		if err != nil {
			j.errorf("feeds RSS: %v", err)
		}
		j.update(func(r *Report) { r.Feeds = n })
	}

	rep := j.Report()
	stamp := map[string]any{ // lets the Conteúdo page list and undo this import
		"import_batch": rep.ID,
		"import_name":  extract.Truncate(strings.Join(rep.Files, ", "), 120),
		"import_at":    rep.Started.UTC().Format(database.TimeLayout),
	}
	results := make([]result, len(items))
	var done atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(ingestWorkers)
	for i := range items {
		it := &items[i]
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}
			id, outcome, err := im.ingestOne(gctx, it, opt, stamp)
			n := done.Add(1)
			j.update(func(r *Report) {
				r.Done = int(n)
				switch {
				case err != nil:
					r.Failed++
					if len(r.Errors) < 30 {
						r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", extract.Truncate(it.Title, 60), err))
					}
				case outcome == outCreated:
					r.Created++
				case outcome == outUpdated:
					r.Updated++
				case outcome == outDeleted:
					r.Deleted++
				default:
					r.Skipped++
				}
			})
			results[i] = result{id: id, item: it, outcome: outcome}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		j.errorf("importação interrompida: %v", err)
	}

	j.update(func(r *Report) { r.State = StateLinking })
	links := im.link(ctx, results)
	queued := im.queue(ctx, results, opt)
	j.update(func(r *Report) {
		r.Links, r.Queued, r.State, r.Finished = links, queued, StateDone, time.Now()
		if ctx.Err() != nil {
			r.State = StateFailed
		}
	})
}

// dedupe keeps the last occurrence of each (format, ref) and merges persons by name.
func dedupe(items []Item) []Item {
	seen := map[string]int{}
	var out []Item
	for _, it := range items {
		key := it.Format + "\x00" + it.Ref
		if it.Type == database.TypePerson {
			key = "person\x00" + strings.ToLower(strings.TrimSpace(it.Title))
		}
		if i, ok := seen[key]; ok {
			if it.Type == database.TypePerson && !strings.Contains(out[i].Content, it.Content) {
				out[i].Content = strings.TrimSpace(out[i].Content + "\n\n" + it.Content)
				out[i].Tags = append(out[i].Tags, it.Tags...)
				continue
			}
			out[i] = it
			continue
		}
		seen[key] = len(out)
		out = append(out, it)
	}
	return out
}

func (it *Item) normalize() {
	if it.Meta == nil {
		it.Meta = map[string]any{}
	}
	if !database.ValidType(it.Type) {
		it.Type = database.TypeNote
	}
	it.Content = strings.TrimSpace(it.Content)
	it.Title = strings.TrimSpace(it.Title)
	if it.Title == "" {
		it.Title = extract.FirstLine(it.Content)
	}
	it.Title = extract.Truncate(it.Title, 200)
	if it.URL != "" {
		it.Meta["url"] = it.URL
	}
	if it.Type == database.TypeTask && it.Status == "" {
		it.Status = database.StatusOpen
	}
	if it.CreatedAt.After(time.Now().Add(24 * time.Hour)) {
		it.CreatedAt = time.Time{}
	}
	if it.Ref == "" {
		it.Ref = hashRef(it.Title, it.Content)
	}
}

func (im *Importer) ingestOne(ctx context.Context, it *Item, opt Options, stamp map[string]any) (int64, int, error) {
	it.normalize()
	it.Tags = append(it.Tags, opt.Tags...)
	it.Meta[agent.MetaNoLLM] = !opt.LLM
	for k, v := range stamp {
		it.Meta[k] = v
	}
	source := SourcePrefix + it.Format
	ex, err := im.db.GetNodeBySource(ctx, source, it.Ref)
	switch {
	case err == nil:
		if ex.Title == it.Title && ex.Content == it.Content {
			return ex.ID, outSkipped, nil
		}
		it.Tags = append(ex.Tags, it.Tags...) // keep tags added by enrichment or by hand
	case !errors.Is(err, database.ErrNotFound):
		return 0, 0, err
	case it.Type == database.TypePerson:
		if p, err := im.db.FindByTitle(ctx, database.TypePerson, it.Title); err == nil {
			return im.mergePerson(ctx, p, it)
		}
	}
	n, created, err := im.ag.Ingest(ctx, agent.IngestInput{
		Type: it.Type, Title: it.Title, Content: it.Content, Summary: it.Summary, Source: source, SourceRef: it.Ref,
		Status: it.Status, Tags: it.Tags, Meta: it.Meta, DueAt: it.DueAt, CreatedAt: it.CreatedAt,
	})
	if errors.Is(err, database.ErrDeleted) {
		return 0, outDeleted, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if created {
		return n.ID, outCreated, nil
	}
	return n.ID, outUpdated, nil
}

// mergePerson enriches an existing person (e.g. auto-created from a mention) with contact data.
func (im *Importer) mergePerson(ctx context.Context, p *database.Node, it *Item) (int64, int, error) {
	if it.Content == "" || strings.Contains(p.Content, it.Content) {
		return p.ID, outSkipped, nil
	}
	p.Content = strings.TrimSpace(p.Content + "\n\n" + it.Content)
	p.Tags = append(p.Tags, it.Tags...)
	for k, v := range it.Meta {
		if _, ok := p.Meta[k]; !ok {
			p.Meta[k] = v
		}
	}
	if err := im.db.UpdateNode(ctx, p); err != nil {
		return 0, 0, err
	}
	return p.ID, outUpdated, nil
}

// link resolves [[wiki-links]] and typed links by file name, alias or title among the
// imported items (falling back to existing nodes), creating edges concurrently.
func (im *Importer) link(ctx context.Context, results []result) int {
	names := map[string]int64{}
	for _, r := range results {
		if r.id == 0 {
			continue
		}
		for _, a := range append([]string{r.item.Title}, r.item.Aliases...) {
			if k := strings.ToLower(strings.TrimSpace(a)); k != "" {
				if _, ok := names[k]; !ok {
					names[k] = r.id
				}
			}
		}
	}
	var mu sync.Mutex
	resolve := func(name string) int64 {
		k := strings.ToLower(strings.TrimSpace(name))
		mu.Lock()
		id, ok := names[k]
		mu.Unlock()
		if ok {
			return id
		}
		if n, err := im.db.FindByTitle(ctx, "", name); err == nil {
			id = n.ID
		}
		mu.Lock()
		names[k] = id
		mu.Unlock()
		return id
	}
	var count atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(ingestWorkers)
	for _, r := range results {
		if r.id == 0 || r.outcome == outSkipped {
			continue
		}
		g.Go(func() error {
			for _, t := range agent.WikiLinks(r.item.Content) {
				if dst := resolve(t); dst != 0 && dst != r.id {
					if im.db.AddEdge(gctx, r.id, dst, "links_to", 1) == nil {
						count.Add(1)
					}
				}
			}
			for _, l := range r.item.Links {
				if dst := resolve(l.Target); dst != 0 && dst != r.id {
					if im.db.AddEdge(gctx, r.id, dst, l.Relation, 1) == nil {
						count.Add(1)
					}
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	return int(count.Load())
}

// queue schedules enrichment (tags, embeddings, auto-links) or page downloads.
func (im *Importer) queue(ctx context.Context, results []result, opt Options) int {
	var count atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(ingestWorkers)
	for _, r := range results {
		if r.id == 0 || r.outcome == outSkipped {
			continue
		}
		g.Go(func() error {
			var err error
			if opt.Fetch && r.item.Format == FormatBookmarks {
				_, err = im.db.Enqueue(gctx, agent.TaskClipFetch, map[string]int64{"id": r.id},
					database.EnqueueOpts{DedupeKey: fmt.Sprintf("clip:%d", r.id), MaxAttempts: 3})
			} else {
				err = im.ag.QueueEnrich(gctx, r.id)
			}
			if err == nil {
				count.Add(1)
			}
			return nil
		})
	}
	_ = g.Wait()
	return int(count.Load())
}

// addFeeds merges OPML subscriptions into RSS_FEEDS, returning how many were new.
func (im *Importer) addFeeds(feeds []string) (int, error) {
	current := im.cfg.GetList("RSS_FEEDS")
	have := map[string]bool{}
	for _, f := range current {
		have[strings.TrimSpace(f)] = true
	}
	added := 0
	for _, f := range feeds {
		f = strings.TrimSpace(f)
		if f == "" || have[f] || (!strings.HasPrefix(f, "http://") && !strings.HasPrefix(f, "https://")) {
			continue
		}
		have[f] = true
		current = append(current, f)
		added++
	}
	if added == 0 {
		return 0, nil
	}
	return added, im.cfg.Update(map[string]string{"RSS_FEEDS": strings.Join(current, "\n")})
}
