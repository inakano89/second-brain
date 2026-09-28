// Package watcher monitors the inbox directory (fsnotify) and ingests dropped
// files (Markdown, TXT, PDF, images, audio, HTML), then archives or deletes them.
package watcher

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/queue"
)

// TaskIngest is the queue kind for a dropped file.
const TaskIngest = "file.ingest"

const archiveDir = ".archive"

var supported = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".pdf": true, ".html": true, ".htm": true, ".json": true,
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true,
	".ogg": true, ".oga": true, ".opus": true, ".mp3": true, ".m4a": true, ".wav": true,
}

// Watcher monitors the inbox.
type Watcher struct {
	cfg *config.Config
	db  *database.DB
	ag  *agent.Agent
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]*time.Timer
}

// New creates a watcher.
func New(cfg *config.Config, db *database.DB, ag *agent.Agent, log *slog.Logger) *Watcher {
	return &Watcher{cfg: cfg, db: db, ag: ag, log: log.With("component", "watcher"), pending: map[string]*time.Timer{}}
}

// Dir returns the monitored directory.
func (w *Watcher) Dir() string { return w.cfg.GetPath("INBOX_DIR") }

// RegisterTasks wires the queue handler.
func (w *Watcher) RegisterTasks(q *queue.Worker) { q.Handle(TaskIngest, w.handle) }

func eligible(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "~") || strings.HasSuffix(base, ".part") || strings.HasSuffix(base, ".crdownload") || strings.HasSuffix(base, ".tmp") {
		return false
	}
	return supported[strings.ToLower(filepath.Ext(base))]
}

// Run watches the directory until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	if !w.cfg.GetBool("WATCHER_ENABLED") {
		return
	}
	dir := w.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.log.Error("não foi possível criar inbox", "dir", dir, "err", err)
		return
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		w.log.Error("fsnotify indisponível", "err", err)
		return
	}
	defer fw.Close()
	if err := fw.Add(dir); err != nil {
		w.log.Error("falha ao monitorar inbox", "dir", dir, "err", err)
		return
	}
	w.log.Info("monitorando inbox", "dir", dir)
	w.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			w.mu.Lock()
			for _, t := range w.pending {
				t.Stop()
			}
			w.mu.Unlock()
			return
		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 && eligible(ev.Name) {
				w.debounce(ctx, ev.Name)
			}
		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			w.log.Warn("erro do fsnotify", "err", err)
		}
	}
}

func (w *Watcher) scan(ctx context.Context) {
	entries, err := os.ReadDir(w.Dir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && eligible(e.Name()) {
			w.debounce(ctx, filepath.Join(w.Dir(), e.Name()))
		}
	}
}

// debounce waits until a file stops changing (size stable) before enqueueing.
func (w *Watcher) debounce(ctx context.Context, path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.pending[path]; ok {
		t.Reset(2 * time.Second)
		return
	}
	var lastSize int64 = -1
	var t *time.Timer
	t = time.AfterFunc(2*time.Second, func() {
		st, err := os.Stat(path)
		if err != nil {
			w.mu.Lock()
			delete(w.pending, path)
			w.mu.Unlock()
			return
		}
		if st.Size() != lastSize {
			lastSize = st.Size()
			t.Reset(time.Second)
			return
		}
		w.mu.Lock()
		delete(w.pending, path)
		w.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		key := fmt.Sprintf("file:%s:%d:%d", path, st.Size(), st.ModTime().UnixNano())
		if _, err := w.db.Enqueue(ctx, TaskIngest, map[string]string{"path": path}, database.EnqueueOpts{DedupeKey: key}); err != nil {
			w.log.Error("falha ao enfileirar arquivo", "path", path, "err", err)
		}
	})
	w.pending[path] = t
}

func (w *Watcher) handle(ctx context.Context, raw json.RawMessage) error {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return queue.Permanent(err)
	}
	data, err := os.ReadFile(p.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // already processed or removed
	}
	if err != nil {
		return err
	}
	sum := sha1.Sum(data)
	ref := hex.EncodeToString(sum[:])
	n, err := w.ag.IngestMedia(ctx, agent.MediaInput{
		Data: data, Filename: filepath.Base(p.Path), Source: "watcher", SourceRef: ref,
		Meta: map[string]any{"path": p.Path},
	})
	if err != nil {
		if queue.IsPermanent(err) {
			w.finish(p.Path, true)
		}
		return err
	}
	w.log.Info("arquivo ingerido", "file", filepath.Base(p.Path), "node", n.ID)
	w.finish(p.Path, false)
	return nil
}

// finish archives (or deletes) a processed file; failed files go to .archive/failed.
func (w *Watcher) finish(path string, failed bool) {
	if w.cfg.Get("WATCHER_ACTION") == "delete" && !failed {
		if err := os.Remove(path); err != nil {
			w.log.Warn("falha ao remover arquivo", "path", path, "err", err)
		}
		return
	}
	sub := time.Now().Format("2006-01")
	if failed {
		sub = "failed"
	}
	dest := filepath.Join(w.Dir(), archiveDir, sub)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return
	}
	target := filepath.Join(dest, filepath.Base(path))
	if _, err := os.Stat(target); err == nil {
		ext := filepath.Ext(path)
		target = filepath.Join(dest, strings.TrimSuffix(filepath.Base(path), ext)+"-"+time.Now().Format("150405")+ext)
	}
	if err := os.Rename(path, target); err != nil {
		w.log.Warn("falha ao arquivar", "path", path, "err", err)
	}
}
