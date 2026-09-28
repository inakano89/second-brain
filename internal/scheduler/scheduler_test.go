package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memStore) KVGet(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *memStore) KVSet(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

func (s *memStore) get(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k]
}

type recorder struct {
	mu  sync.Mutex
	ran []string
}

func (r *recorder) job(name string, err error) JobFunc {
	return func(context.Context) error {
		r.mu.Lock()
		r.ran = append(r.ran, name)
		r.mu.Unlock()
		return err
	}
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...)
}

func TestLatestDue(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Date(2026, 9, 28, 10, 0, 30, 0, loc)
	cases := []struct {
		spec  string
		after time.Time
		want  time.Time
		ok    bool
	}{
		{"0 4 * * *", now.AddDate(0, 0, -3), time.Date(2026, 9, 28, 4, 0, 0, 0, loc), true},
		{"0 4 * * *", time.Date(2026, 9, 28, 4, 0, 5, 0, loc), time.Time{}, false},
		{"*/15 * * * *", now.AddDate(0, -2, 0), time.Date(2026, 9, 28, 10, 0, 0, 0, loc), true},
		{"0 18 * * 0", now.AddDate(0, 0, -20), time.Date(2026, 9, 27, 18, 0, 0, 0, loc), true},
		{"0 0 1 1 *", now.AddDate(-2, 0, 0), time.Date(2026, 1, 1, 0, 0, 0, 0, loc), true},
	}
	for _, c := range cases {
		sc, _ := Parse(c.spec)
		got, ok := latestDue(sc, c.after, now)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s after %v: got %v %v, want %v %v", c.spec, c.after, got, ok, c.want, c.ok)
		}
	}
}

func TestCatchUpAfterDowntime(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, loc) // PC turned on at 10h
	store := &memStore{m: map[string]string{
		lastRunKey + "backup":  now.AddDate(0, 0, -2).Add(-6 * time.Hour).UTC().Format(time.RFC3339), // 2 days ago, 04:00
		lastRunKey + "morning": now.AddDate(0, 0, -1).Add(-3 * time.Hour).UTC().Format(time.RFC3339), // yesterday 07:00
		lastRunKey + "evening": now.AddDate(0, 0, -2).Add(11 * time.Hour).UTC().Format(time.RFC3339), // 2 days ago 21:00
		lastRunKey + "update":  now.AddDate(0, 0, -3).UTC().Format(time.RFC3339),
		lastRunKey + "hourly":  now.UTC().Format(time.RFC3339), // already ran this hour
	}}
	rec := &recorder{}
	start := func() *Scheduler { // a fresh process
		s := New(loc, slog.New(slog.NewTextHandler(io.Discard, nil)))
		s.Store, s.CatchUpDelay, s.now = store, 0, func() time.Time { return now }
		s.Add("backup", "0 4 * * *", rec.job("backup", nil))
		s.Add("morning", "0 7 * * *", rec.job("morning", nil), CatchUpWithin(5*time.Hour))
		s.Add("evening", "0 21 * * *", rec.job("evening", nil), CatchUpWithin(3*time.Hour))
		s.Add("update", "40 4 * * *", rec.job("update", errors.New("offline")))
		s.Add("hourly", "0 * * * *", rec.job("hourly", nil))
		s.Add("fresh", "0 3 * * *", rec.job("fresh", nil))
		return s
	}
	s := start()
	s.catchUp(context.Background())
	s.wg.Wait()
	got := rec.list()
	want := []string{"backup", "update", "morning"} // oldest missed activation first; evening outside its window
	if len(got) != len(want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ran %v, want %v", got, want)
		}
	}
	if v := store.get(lastRunKey + "backup"); v != now.UTC().Format(time.RFC3339) {
		t.Errorf("backup last run = %q", v)
	}
	if v := store.get(lastRunKey + "update"); v != now.AddDate(0, 0, -3).UTC().Format(time.RFC3339) {
		t.Errorf("failed job must stay due, last run = %q", v)
	}
	if store.get(lastRunKey+"fresh") == "" {
		t.Error("new job should get a baseline")
	}

	// Second start: nothing left but the failed update.
	rec.ran = nil
	s = start()
	s.catchUp(context.Background())
	s.wg.Wait()
	if got := rec.list(); len(got) != 1 || got[0] != "update" {
		t.Fatalf("second start ran %v", got)
	}
}

func TestTickAfterSleepHonoursWindow(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Date(2026, 9, 28, 15, 0, 20, 0, loc) // resumed from sleep at 15h
	rec := &recorder{}
	s := New(loc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now.Add(-12 * time.Hour) } // jobs registered before sleeping
	s.Add("morning", "0 7 * * *", rec.job("morning", nil), CatchUpWithin(5*time.Hour))
	s.Add("backup", "0 4 * * *", rec.job("backup", nil))
	s.Add("minute", "* * * * *", rec.job("minute", nil), CatchUpWithin(time.Minute))
	s.now = func() time.Time { return now }
	s.tick(context.Background(), now)
	s.wg.Wait()
	got := map[string]bool{}
	for _, n := range rec.list() {
		got[n] = true
	}
	if got["morning"] || !got["backup"] || !got["minute"] {
		t.Fatalf("ran %v", rec.list())
	}
	for _, j := range s.Jobs() {
		if !j.Next.After(now) {
			t.Errorf("%s not rescheduled: %v", j.Name, j.Next)
		}
	}
}
