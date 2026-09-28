package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/queue"
)

// TaskImportFile imports a file already on disk through the offline queue
// (.zip dropped in the inbox, Takeout archives downloaded from Google Drive).
const TaskImportFile = "import.file"

// FileTask is the TaskImportFile payload.
type FileTask struct {
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	Origin string `json:"origin,omitempty"` // watcher | drive
	Remove bool   `json:"remove,omitempty"` // delete the file afterwards
}

// Enqueue schedules a queued import of a file on disk.
func (im *Importer) Enqueue(ctx context.Context, t FileTask) error {
	_, err := im.db.Enqueue(ctx, TaskImportFile, t, database.EnqueueOpts{DedupeKey: TaskImportFile + ":" + t.Path, MaxAttempts: 3})
	return err
}

// RegisterTasks wires the queue handler.
func (im *Importer) RegisterTasks(w *queue.Worker) {
	w.HandleTimeout(TaskImportFile, 3*time.Hour, im.handleFile)
}

func (im *Importer) handleFile(ctx context.Context, raw json.RawMessage) error {
	var t FileTask
	if err := json.Unmarshal(raw, &t); err != nil || t.Path == "" {
		return queue.Permanentf("payload inválido: %v", err)
	}
	st, err := os.Stat(t.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // already imported
	}
	if err != nil {
		return err
	}
	if t.Name == "" {
		t.Name = filepath.Base(t.Path)
	}
	rep := im.Run(ctx, []File{{Path: t.Path, Name: t.Name}}, Options{MaxBytes: max(im.MaxBytes(), st.Size())})
	if ctx.Err() != nil {
		return ctx.Err() // shutting down: retry later
	}
	failed := rep.State == StateFailed
	if im.Done != nil {
		im.Done(ctx, t, rep)
	}
	if t.Remove {
		os.Remove(t.Path)
	}
	if failed {
		return queue.Permanentf("importação de %s falhou: %v", t.Name, rep.Errors)
	}
	return nil
}

// Summary describes a finished import in one paragraph (notifications).
func (r Report) Summary() string {
	s := fmt.Sprintf("%d novos, %d atualizados, %d sem mudança", r.Created, r.Updated, r.Skipped)
	if r.Deleted > 0 {
		s += fmt.Sprintf(", %d apagados por você antes", r.Deleted)
	}
	if r.Failed > 0 {
		s += fmt.Sprintf(", %d falhas", r.Failed)
	}
	if f := r.FormatList(); f != "" {
		s += " — " + f
	}
	for i, w := range r.Warnings {
		if i == 3 {
			s += fmt.Sprintf("\n… e mais %d aviso(s)", len(r.Warnings)-3)
			break
		}
		s += "\n⚠️ " + w
	}
	return s
}
