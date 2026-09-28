package database

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogEntry is an audit log row.
type LogEntry struct {
	ID        int64     `json:"id"`
	TS        time.Time `json:"ts"`
	Level     string    `json:"level"`
	Component string    `json:"component"`
	Message   string    `json:"message"`
	Meta      string    `json:"meta"`
}

// LogFilter restricts log listings.
type LogFilter struct {
	Level     string
	Component string
	Query     string
	Before    int64
	Limit     int
}

// InsertLogs writes a batch of log entries in one transaction.
func (db *DB) InsertLogs(ctx context.Context, entries []LogEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs (ts, level, component, message, meta) VALUES (?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		if _, err := stmt.ExecContext(ctx, fmtTime(e.TS), e.Level, e.Component, e.Message, e.Meta); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ListLogs returns log entries newest first.
func (db *DB) ListLogs(ctx context.Context, f LogFilter) ([]LogEntry, error) {
	var conds []string
	var args []any
	if f.Level != "" {
		conds = append(conds, "level = ?")
		args = append(args, strings.ToUpper(f.Level))
	}
	if f.Component != "" {
		conds = append(conds, "component = ?")
		args = append(args, f.Component)
	}
	if f.Query != "" {
		conds = append(conds, "(message LIKE ? OR meta LIKE ?)")
		args = append(args, "%"+f.Query+"%", "%"+f.Query+"%")
	}
	if f.Before > 0 {
		conds = append(conds, "id < ?")
		args = append(args, f.Before)
	}
	q := `SELECT id, ts, level, component, message, meta FROM logs`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Level, &e.Component, &e.Message, &e.Meta); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LogComponents lists distinct components.
func (db *DB) LogComponents(ctx context.Context) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT component FROM logs WHERE component <> '' ORDER BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountLogs counts entries of level since t.
func (db *DB) CountLogs(ctx context.Context, level string, since time.Time) (int, error) {
	var c int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs WHERE level=? AND ts>=?`, strings.ToUpper(level), fmtTime(since)).Scan(&c)
	return c, err
}

// PurgeLogs deletes entries older than before.
func (db *DB) PurgeLogs(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM logs WHERE ts < ?`, fmtTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- slog handler that mirrors records into the audit log ----

// LogHandler is an slog.Handler that forwards to next and asynchronously
// persists records at or above MinLevel into the logs table in batches.
type LogHandler struct {
	next  slog.Handler
	sink  *logSink
	attrs []slog.Attr
	group string
}

type logSink struct {
	db       *DB
	ch       chan LogEntry
	minLevel slog.Level
	once     sync.Once
	done     chan struct{}
}

// NewLogHandler wraps next; call Close on shutdown to flush.
func NewLogHandler(db *DB, next slog.Handler, min slog.Level) *LogHandler {
	s := &logSink{db: db, ch: make(chan LogEntry, 1024), minLevel: min, done: make(chan struct{})}
	go s.run()
	return &LogHandler{next: next, sink: s}
}

func (s *logSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var batch []LogEntry
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.db.InsertLogs(ctx, batch)
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close flushes pending entries.
func (h *LogHandler) Close() {
	h.sink.once.Do(func() { close(h.sink.ch) })
	<-h.sink.done
}

// Enabled implements slog.Handler.
func (h *LogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l) || l >= h.sink.minLevel
}

// Handle implements slog.Handler.
func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r)
	}
	if r.Level < h.sink.minLevel {
		return err
	}
	meta := map[string]any{}
	component := ""
	add := func(a slog.Attr) bool {
		if a.Key == "component" {
			component = a.Value.String()
			return true
		}
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		meta[key] = a.Value.Any()
		if e, ok := a.Value.Any().(error); ok {
			meta[key] = e.Error()
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	var metaStr string
	if len(meta) > 0 {
		if b, e := json.Marshal(meta); e == nil {
			metaStr = string(b)
		}
	}
	entry := LogEntry{TS: r.Time, Level: r.Level.String(), Component: component, Message: r.Message, Meta: metaStr}
	defer func() { recover() }() // channel closed during shutdown
	select {
	case h.sink.ch <- entry:
	default: // drop when saturated rather than block callers
	}
	return err
}

// WithAttrs implements slog.Handler.
func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogHandler{next: h.next.WithAttrs(attrs), sink: h.sink, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...), group: h.group}
}

// WithGroup implements slog.Handler.
func (h *LogHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &LogHandler{next: h.next.WithGroup(name), sink: h.sink, attrs: h.attrs, group: g}
}
