// Package database implements the SQLite (modernc, CGO-free) storage layer:
// graph nodes/edges, FTS5 index, vectors, metrics, logs and the offline task queue.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// DB wraps *sql.DB with domain helpers.
type DB struct {
	*sql.DB
	path   string
	vec    *vectorIndex
	notify chan struct{}
}

// Open opens (and migrates) the database at path using WAL mode.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)",
		"busy_timeout(10000)",
		"foreign_keys(1)",
		"synchronous(NORMAL)",
		"auto_vacuum(INCREMENTAL)",
		"temp_store(MEMORY)",
	} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	dsn := "file:" + filepath.ToSlash(path) + "?" + q.Encode()
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqldb.SetMaxOpenConns(8)
	sqldb.SetMaxIdleConns(4)
	sqldb.SetConnMaxIdleTime(5 * time.Minute)
	db := &DB{DB: sqldb, path: path, vec: newVectorIndex(), notify: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.migrate(ctx); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := db.loadVectors(ctx); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("load vectors: %w", err)
	}
	return db, nil
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// Notify returns a channel signalled whenever a task is enqueued.
func (db *DB) Notify() <-chan struct{} { return db.notify }

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	for i, m := range migrations {
		v := i + 1
		if v <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range splitSQL(m) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w\n%s", v, err, stmt)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, v, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// splitSQL splits on ";\n" boundaries while keeping trigger bodies intact.
func splitSQL(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(strings.ToUpper(ln))
		if strings.HasPrefix(t, "CREATE TRIGGER") {
			depth++
		}
		cur.WriteString(ln)
		cur.WriteByte('\n')
		if depth > 0 {
			if strings.HasPrefix(t, "END;") {
				depth--
				out = append(out, strings.TrimSpace(cur.String()))
				cur.Reset()
			}
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(ln), ";") {
			if st := strings.TrimSpace(cur.String()); st != ";" && st != "" {
				out = append(out, st)
			}
			cur.Reset()
		}
	}
	if st := strings.TrimSpace(cur.String()); st != "" {
		out = append(out, st)
	}
	return out
}

// TimeLayout is the canonical fixed-width UTC layout stored in the database
// (fixed width keeps lexicographic order equal to chronological order).
const TimeLayout = "2006-01-02T15:04:05.000000Z07:00"

func now() string { return fmtTime(time.Now()) }

func parseTime(s string) time.Time {
	for _, l := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func fmtTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// Snapshot writes a consistent copy of the database to dest (VACUUM INTO).
func (db *DB) Snapshot(ctx context.Context, dest string) error {
	os.Remove(dest)
	_, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest)
	return err
}

// Maintenance runs housekeeping pragmas; full=true also runs VACUUM.
func (db *DB) Maintenance(ctx context.Context, full bool) error {
	stmts := []string{
		`INSERT INTO nodes_fts(nodes_fts) VALUES('optimize')`,
		`PRAGMA incremental_vacuum`,
		`PRAGMA optimize`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	}
	if full {
		stmts = append(stmts, `VACUUM`, `PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// Size returns the on-disk size of the database plus WAL.
func (db *DB) Size() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(db.path + suffix); err == nil {
			total += st.Size()
		}
	}
	return total
}
