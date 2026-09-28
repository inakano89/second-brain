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

// Store persists when each job last succeeded, so routines missed while the
// process was down (PC off, program closed) run once at the next start.
type Store interface {
	KVGet(ctx context.Context, key string) (string, bool, error)
	KVSet(ctx context.Context, key, value string) error
}

const (
	lastRunKey = "scheduler.last."
	lateAfter  = 2 * time.Minute // a due activation older than this was missed (downtime or sleep)
)

type job struct {
	name    string
	spec    string
	sched   *Schedule
	fn      JobFunc
	window  time.Duration // missed activations older than this are skipped; 0 = any age
	next    time.Time
	running atomic.Bool
	lastRun time.Time
	lastErr string
	lastDur time.Duration
}

// JobOption customises a job.
type JobOption func(*job)

// CatchUpWithin runs a missed activation (downtime or sleep) only if it is at most d old.
func CatchUpWithin(d time.Duration) JobOption { return func(j *job) { j.window = d } }

func (j *job) allowed(due, now time.Time) bool { return j.window <= 0 || now.Sub(due) <= j.window }

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

	Store        Store         // optional: enables catch-up of runs missed while stopped
	CatchUpDelay time.Duration // pause before running missed jobs at start
	now          func() time.Time
}

// New creates a scheduler.
func New(loc *time.Location, log *slog.Logger) *Scheduler {
	return &Scheduler{loc: loc, log: log.With("component", "scheduler"), jobs: map[string]*job{}, ctx: context.Background(),
		CatchUpDelay: 2 * time.Minute, now: time.Now}
}

// Add registers a job; an empty/"off" spec disables it.
func (s *Scheduler) Add(name, spec string, fn JobFunc, opts ...JobOption) error {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "off") || spec == "-" {
		return nil
	}
	sc, err := Parse(spec)
	if err != nil {
		return fmt.Errorf("job %s: %w", name, err)
	}
	j := &job{name: name, spec: spec, sched: sc, fn: fn, next: sc.Next(s.now().In(s.loc))}
	for _, o := range opts {
		o(j)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[name] = j
	return nil
}

// latestDue returns the most recent activation in (after, now].
func latestDue(sc *Schedule, after, now time.Time) (time.Time, bool) {
	if first := sc.Next(after); first.IsZero() || first.After(now) {
		return time.Time{}, false
	}
	// Search from a short look-back first so frequent schedules stay cheap after long downtimes.
	for _, back := range []time.Duration{time.Hour, 25 * time.Hour, 8 * 24 * time.Hour, 32 * 24 * time.Hour, 367 * 24 * time.Hour, 0} {
		from := after
		if back > 0 && now.Add(-back).After(after) {
			from = now.Add(-back)
		}
		t := sc.Next(from)
		if t.IsZero() || t.After(now) {
			continue
		}
		for {
			n := sc.Next(t)
			if n.IsZero() || n.After(now) {
				return t, true
			}
			t = n
		}
	}
	return time.Time{}, false
}

// Run ticks every minute until ctx is cancelled, launching due jobs in goroutines.
// With a Store, jobs missed while the process was stopped run once, one at a time, after CatchUpDelay.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	s.log.Info("agendador iniciado", "jobs", len(s.jobs), "tz", s.loc.String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.catchUp(ctx)
	}()
	for {
		now := s.now().In(s.loc)
		wait := now.Truncate(time.Minute).Add(time.Minute).Sub(now) + 50*time.Millisecond
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-time.After(wait):
		}
		s.tick(ctx, s.now().In(s.loc))
	}
}

// tick launches due jobs. After a sleep/hibernation, late activations follow the catch-up window.
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.next.After(now) {
			continue
		}
		due, _ := latestDue(j.sched, j.next.Add(-time.Minute), now)
		j.next = j.sched.Next(now)
		if now.Sub(due) > lateAfter && !j.allowed(due, now) {
			s.log.Info("rotina atrasada ignorada (fora da janela)", "job", j.name, "previsto", due.Format("02/01 15:04"))
			continue
		}
		s.launch(ctx, j)
	}
}

// catchUp runs, oldest first, the activations missed since each job's last successful run.
func (s *Scheduler) catchUp(ctx context.Context) {
	if s.Store == nil {
		return
	}
	now := s.now().In(s.loc)
	type missed struct {
		j   *job
		due time.Time
	}
	var list []missed
	s.mu.Lock()
	jobs := make([]*job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()
	for _, j := range jobs {
		v, ok, err := s.Store.KVGet(ctx, lastRunKey+j.name)
		if err != nil {
			continue
		}
		if !ok { // first start with this job: nothing to catch up
			_ = s.Store.KVSet(ctx, lastRunKey+j.name, now.UTC().Format(time.RFC3339))
			continue
		}
		last, err := time.Parse(time.RFC3339, v)
		if err != nil {
			continue
		}
		due, ok := latestDue(j.sched, last.In(s.loc), now)
		if !ok {
			continue
		}
		if !j.allowed(due, now) {
			s.log.Info("rotina perdida ignorada (fora da janela)", "job", j.name, "previsto", due.Format("02/01 15:04"))
			continue
		}
		list = append(list, missed{j, due})
	}
	if len(list) == 0 {
		return
	}
	sort.Slice(list, func(i, k int) bool { return list[i].due.Before(list[k].due) })
	names := make([]string, len(list))
	for i, m := range list {
		names[i] = m.j.name
	}
	s.log.Info("rotinas perdidas enquanto o servidor estava parado serão executadas", "jobs", strings.Join(names, ","), "em", s.CatchUpDelay)
	select {
	case <-ctx.Done():
		return
	case <-time.After(s.CatchUpDelay):
	}
	for _, m := range list {
		s.mu.Lock()
		ran := !m.j.lastRun.Before(m.due) // a regular tick already covered it
		var done <-chan struct{}
		ok := false
		if !ran {
			done, ok = s.launch(ctx, m.j)
		}
		s.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

// launch starts j unless it is already running; done closes when it finishes. Callers hold s.mu.
func (s *Scheduler) launch(ctx context.Context, j *job) (<-chan struct{}, bool) {
	if !j.running.CompareAndSwap(false, true) {
		s.log.Warn("job ainda em execução, ciclo ignorado", "job", j.name)
		return nil, false
	}
	done := make(chan struct{})
	start := s.now()
	j.lastRun = start
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		defer j.running.Store(false)
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
		j.lastDur = time.Since(start)
		j.lastErr = ""
		if err != nil {
			j.lastErr = err.Error()
		}
		s.mu.Unlock()
		if err != nil {
			s.log.Error("job falhou", "job", j.name, "err", err)
			return
		}
		s.log.Info("job executado", "job", j.name, "ms", time.Since(start).Milliseconds())
		if s.Store != nil { // failures stay "missed" and are retried at the next start
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = s.Store.KVSet(sctx, lastRunKey+j.name, start.UTC().Format(time.RFC3339))
			cancel()
		}
	}()
	return done, true
}

// RunNow triggers a job immediately (async).
func (s *Scheduler) RunNow(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[name]
	if !ok {
		return fmt.Errorf("job %q não encontrado ou desativado", name)
	}
	if _, ok := s.launch(s.ctx, j); !ok {
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
