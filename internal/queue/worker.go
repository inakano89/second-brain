// Package queue runs the SQLite-backed offline task queue with a pool of goroutines.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/llm"
)

// Handler processes one task payload.
type Handler func(ctx context.Context, payload json.RawMessage) error

type permanentErr struct{ error }

func (p permanentErr) Unwrap() error { return p.error }

// Permanent marks an error as non-retryable.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentErr{err}
}

// IsPermanent reports whether err was marked non-retryable.
func IsPermanent(err error) bool {
	var p permanentErr
	return errors.As(err, &p)
}

// Permanentf formats a non-retryable error.
func Permanentf(format string, a ...any) error { return Permanent(fmt.Errorf(format, a...)) }

// Worker dispatches queued tasks to registered handlers.
type Worker struct {
	db       *database.DB
	log      *slog.Logger
	mu       sync.RWMutex
	handlers map[string]Handler
	timeouts map[string]time.Duration
	timeout  time.Duration
}

// New creates a worker.
func New(db *database.DB, log *slog.Logger) *Worker {
	return &Worker{db: db, log: log.With("component", "queue"), handlers: map[string]Handler{}, timeouts: map[string]time.Duration{}, timeout: 10 * time.Minute}
}

// Handle registers a handler for kind.
func (w *Worker) Handle(kind string, h Handler) {
	w.mu.Lock()
	w.handlers[kind] = h
	w.mu.Unlock()
}

// HandleTimeout registers a handler whose runs may take up to d (long imports/downloads).
func (w *Worker) HandleTimeout(kind string, d time.Duration, h Handler) {
	w.mu.Lock()
	w.handlers[kind] = h
	w.timeouts[kind] = d
	w.mu.Unlock()
}

// Run starts n goroutines consuming tasks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context, n int) {
	if n <= 0 {
		n = 2
	}
	if c, err := w.db.ResetRunning(ctx); err == nil && c > 0 {
		w.log.Info("tarefas interrompidas reenfileiradas", "count", c)
	}
	wake := make(chan struct{}, n)
	var wg sync.WaitGroup
	// Fan-out DB notifications to all workers.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.db.Notify():
				for i := 0; i < n; i++ {
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w.loop(ctx, wake)
		}(i)
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, wake <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		for {
			if ctx.Err() != nil {
				return
			}
			task, err := w.db.ClaimTask(ctx)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Warn("falha ao buscar tarefa", "err", err)
				}
				break
			}
			if task == nil {
				break
			}
			w.execute(ctx, task)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

func (w *Worker) execute(ctx context.Context, t *database.Task) {
	w.mu.RLock()
	h := w.handlers[t.Kind]
	timeout := w.timeout
	if d, ok := w.timeouts[t.Kind]; ok {
		timeout = d
	}
	w.mu.RUnlock()
	bg := context.WithoutCancel(ctx)
	if h == nil {
		_ = w.db.FailTask(bg, t, fmt.Errorf("sem handler para %q", t.Kind), false)
		return
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	start := time.Now()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = Permanentf("panic: %v", r)
			}
		}()
		return h(tctx, t.Payload)
	}()
	cancel()
	if err == nil || errors.Is(err, database.ErrDeleted) { // deleted by the user: nothing left to do
		_ = w.db.CompleteTask(bg, t.ID)
		w.log.Debug("tarefa concluída", "kind", t.Kind, "id", t.ID, "ms", time.Since(start).Milliseconds())
		return
	}
	if ctx.Err() != nil {
		// Shutting down: leave for retry without burning an attempt.
		_ = w.db.FailTask(bg, &database.Task{ID: t.ID, Attempts: 0, MaxAttempts: t.MaxAttempts}, err, true)
		return
	}
	var perm permanentErr
	retryable := !errors.As(err, &perm)
	if retryable {
		var ae *llm.APIError
		if errors.As(err, &ae) {
			retryable = llm.IsRetryable(err)
		}
	}
	_ = w.db.FailTask(bg, t, err, retryable)
	level := slog.LevelWarn
	if !retryable || t.Attempts >= t.MaxAttempts {
		level = slog.LevelError
	}
	w.log.Log(bg, level, "tarefa falhou", "kind", t.Kind, "id", t.ID, "attempt", t.Attempts, "retry", retryable && t.Attempts < t.MaxAttempts, "err", err)
}
