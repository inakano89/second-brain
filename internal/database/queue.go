package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// Task statuses.
const (
	TaskPending = "pending"
	TaskRunning = "running"
	TaskDone    = "done"
	TaskFailed  = "failed"
)

// Task is a unit of offline work.
type Task struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	LastError   string          `json:"last_error"`
	DedupeKey   string          `json:"dedupe_key"`
	RunAfter    time.Time       `json:"run_after"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// EnqueueOpts customises Enqueue.
type EnqueueOpts struct {
	DedupeKey   string
	Delay       time.Duration
	MaxAttempts int
}

// Enqueue adds a task. With DedupeKey, an identical pending/running task is not duplicated.
func (db *DB) Enqueue(ctx context.Context, kind string, payload any, o EnqueueOpts) (int64, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	if payload == nil {
		b = []byte("{}")
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	var dk any
	if o.DedupeKey != "" {
		dk = o.DedupeKey
	}
	t := time.Now().UTC()
	res, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO task_queue (kind, payload, status, max_attempts, dedupe_key, run_after, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, kind, string(b), TaskPending, o.MaxAttempts, dk, fmtTime(t.Add(o.Delay)), fmtTime(t), fmtTime(t))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	select {
	case db.notify <- struct{}{}:
	default:
	}
	return id, nil
}

const taskCols = `id, kind, payload, status, attempts, max_attempts, last_error, COALESCE(dedupe_key,''), run_after, created_at, updated_at`

func scanTask(s scanner) (*Task, error) {
	var t Task
	var payload, ra, ca, ua string
	if err := s.Scan(&t.ID, &t.Kind, &payload, &t.Status, &t.Attempts, &t.MaxAttempts, &t.LastError, &t.DedupeKey, &ra, &ca, &ua); err != nil {
		return nil, err
	}
	t.Payload = json.RawMessage(payload)
	t.RunAfter, t.CreatedAt, t.UpdatedAt = parseTime(ra), parseTime(ca), parseTime(ua)
	return &t, nil
}

// ClaimTask atomically moves the next ready task to running. Returns nil when idle.
func (db *DB) ClaimTask(ctx context.Context) (*Task, error) {
	t := now()
	row := db.QueryRowContext(ctx, `UPDATE task_queue SET status='running', attempts=attempts+1, updated_at=?
		WHERE id = (SELECT id FROM task_queue WHERE status='pending' AND run_after <= ? ORDER BY run_after, id LIMIT 1)
		RETURNING `+taskCols, t, t)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

// CompleteTask marks a task done.
func (db *DB) CompleteTask(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE task_queue SET status='done', last_error='', updated_at=? WHERE id=?`, now(), id)
	return err
}

// FailTask records an error; retryable tasks are rescheduled with exponential backoff.
func (db *DB) FailTask(ctx context.Context, t *Task, cause error, retryable bool) error {
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if !retryable || t.Attempts >= t.MaxAttempts {
		_, err := db.ExecContext(ctx, `UPDATE task_queue SET status='failed', last_error=?, updated_at=? WHERE id=?`, msg, now(), t.ID)
		return err
	}
	backoff := time.Duration(math.Min(float64(30*time.Second)*math.Pow(2, float64(t.Attempts-1)), float64(6*time.Hour)))
	_, err := db.ExecContext(ctx, `UPDATE task_queue SET status='pending', last_error=?, run_after=?, updated_at=? WHERE id=?`,
		msg, fmtTime(time.Now().Add(backoff)), now(), t.ID)
	return err
}

// ResetRunning returns crashed "running" tasks to pending (call on startup).
func (db *DB) ResetRunning(ctx context.Context) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE task_queue SET status='pending', updated_at=? WHERE status='running'`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RetryFailed requeues failed tasks.
func (db *DB) RetryFailed(ctx context.Context) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE task_queue SET status='pending', attempts=0, run_after=?, updated_at=? WHERE status='failed'`, now(), now())
	if err != nil {
		return 0, err
	}
	select {
	case db.notify <- struct{}{}:
	default:
	}
	return res.RowsAffected()
}

// QueueStats counts tasks by status.
func (db *DB) QueueStats(ctx context.Context) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM task_queue GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{TaskPending: 0, TaskRunning: 0, TaskDone: 0, TaskFailed: 0}
	for rows.Next() {
		var s string
		var c int
		if err := rows.Scan(&s, &c); err != nil {
			return nil, err
		}
		out[s] = c
	}
	return out, rows.Err()
}

// ListTasks lists recent tasks (status optional).
func (db *DB) ListTasks(ctx context.Context, status string, limit int) ([]Task, error) {
	q := `SELECT ` + taskCols + ` FROM task_queue`
	var args []any
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// PurgeTasks deletes finished tasks older than before.
func (db *DB) PurgeTasks(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM task_queue WHERE status IN ('done','failed') AND updated_at < ?`, fmtTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
