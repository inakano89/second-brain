package queue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

func TestWorkerProcessesAndMarksPermanentFailures(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var ok atomic.Int32
	w.Handle("ok", func(ctx context.Context, p json.RawMessage) error { ok.Add(1); return nil })
	w.Handle("bad", func(ctx context.Context, p json.RawMessage) error { return Permanent(errors.New("nope")) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx, 4); close(done) }()
	for i := 0; i < 20; i++ {
		db.Enqueue(ctx, "ok", map[string]int{"i": i}, database.EnqueueOpts{})
	}
	db.Enqueue(ctx, "bad", nil, database.EnqueueOpts{})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := db.QueueStats(ctx)
		if st[database.TaskDone] == 20 && st[database.TaskFailed] == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	st, _ := db.QueueStats(context.Background())
	if ok.Load() != 20 || st[database.TaskFailed] != 1 {
		t.Fatalf("processed=%d stats=%v", ok.Load(), st)
	}
}
