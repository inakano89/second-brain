// Package scheduler runs internal cron jobs: briefings, reviews, maintenance,
// encrypted backups and periodic sync triggers.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// JobFunc is a scheduled routine.
type JobFunc func(ctx context.Context) error

type job struct {
	name    string
	spec    string
	sched   *Schedule
	fn      JobFunc
	next    time.Time
	running atomic.Bool
	lastRun time.Time
	lastErr string
	lastDur time.Duration
}

// JobInfo is a job status snapshot.
type JobInfo struct {
	Name     string        `json:"name"`
	Spec     string        `json:"spec"`
	Next     time.Time     `json:"next"`
	LastRun  time.Time     `json:"last_run"`
	LastErr  string        `json:"last_err"`
	Duration time.Duration `json:"duration"`
	Running  bool          `json:"running"`
}

// Scheduler triggers jobs on cron schedules in the configured timezone.
type Scheduler struct {
	loc  *time.Location
	log  *slog.Logger
	mu   sync.Mutex
	jobs map[string]*job
	wg   sync.WaitGroup
	ctx  context.Context
}

// New creates a scheduler.
func New(loc *time.Location, log *slog.Logger) *Scheduler {
	return &Scheduler{loc: loc, log: log.With("component", "scheduler"), jobs: map[string]*job{}, ctx: context.Background()}
}

// Add registers a job; an empty/"off" spec disables it.
func (s *Scheduler) Add(name, spec string, fn JobFunc) error {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "off") || spec == "-" {
		return nil
	}
	sc, err := Parse(spec)
	if err != nil {
		return fmt.Errorf("job %s: %w", name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[name] = &job{name: name, spec: spec, sched: sc, fn: fn, next: sc.Next(time.Now().In(s.loc))}
	return nil
}

// Run ticks every minute until ctx is cancelled, launching due jobs in goroutines.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	s.log.Info("agendador iniciado", "jobs", len(s.jobs), "tz", s.loc.String())
	for {
		now := time.Now().In(s.loc)
		wait := now.Truncate(time.Minute).Add(time.Minute).Sub(now) + 50*time.Millisecond
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-time.After(wait):
		}
		now = time.Now().In(s.loc)
		s.mu.Lock()
		for _, j := range s.jobs {
			if !j.next.After(now) {
				j.next = j.sched.Next(now)
				s.launch(ctx, j)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Scheduler) launch(ctx context.Context, j *job) bool {
	if !j.running.CompareAndSwap(false, true) {
		s.log.Warn("job ainda em execução, ciclo ignorado", "job", j.name)
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer j.running.Store(false)
		start := time.Now()
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			jctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			defer cancel()
			return j.fn(jctx)
		}()
		s.mu.Lock()
		j.lastRun, j.lastDur = start, time.Since(start)
		j.lastErr = ""
		if err != nil {
			j.lastErr = err.Error()
		}
		s.mu.Unlock()
		if err != nil {
			s.log.Error("job falhou", "job", j.name, "err", err)
		} else {
			s.log.Info("job executado", "job", j.name, "ms", time.Since(start).Milliseconds())
		}
	}()
	return true
}

// RunNow triggers a job immediately (async).
func (s *Scheduler) RunNow(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[name]
	if !ok {
		return fmt.Errorf("job %q não encontrado ou desativado", name)
	}
	if !s.launch(s.ctx, j) {
		return fmt.Errorf("job %q já está em execução", name)
	}
	return nil
}

// Jobs lists job statuses sorted by next run.
func (s *Scheduler) Jobs() []JobInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JobInfo, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, JobInfo{Name: j.name, Spec: j.spec, Next: j.next, LastRun: j.lastRun, LastErr: j.lastErr, Duration: j.lastDur, Running: j.running.Load()})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Next.Before(out[k].Next) })
	return out
}
