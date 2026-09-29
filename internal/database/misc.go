package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ---- metrics ----

// Metric is a daily health/quantified-self measurement.
type Metric struct {
	Date   string  `json:"date"` // YYYY-MM-DD
	Kind   string  `json:"kind"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Source string  `json:"source"`
}

// UpsertMetric inserts or replaces a metric for (date, kind, source).
func (db *DB) UpsertMetric(ctx context.Context, m Metric) error {
	_, err := db.ExecContext(ctx, `INSERT INTO metrics (date, kind, value, unit, source, created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(date, kind, source) DO UPDATE SET value=excluded.value, unit=excluded.unit`,
		m.Date, m.Kind, m.Value, m.Unit, m.Source, now())
	return err
}

// MetricsRange returns metrics with date in [from, to] (inclusive).
func (db *DB) MetricsRange(ctx context.Context, from, to string) ([]Metric, error) {
	rows, err := db.QueryContext(ctx, `SELECT date, kind, value, unit, source FROM metrics WHERE date >= ? AND date <= ? ORDER BY date DESC, kind`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Metric
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.Date, &m.Kind, &m.Value, &m.Unit, &m.Source); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MetricAverage returns the average of kind over [from, to].
func (db *DB) MetricAverage(ctx context.Context, kind, from, to string) (float64, int, error) {
	var avg sql.NullFloat64
	var n int
	err := db.QueryRowContext(ctx, `SELECT AVG(value), COUNT(*) FROM metrics WHERE kind=? AND date>=? AND date<=?`, kind, from, to).Scan(&avg, &n)
	return avg.Float64, n, err
}

// ---- usage ----

// UsageRecord is one LLM call accounting row.
type UsageRecord struct {
	Provider     string
	Model        string
	Purpose      string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// UsageRow is an aggregated usage line.
type UsageRow struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Calls        int     `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// DailyUsage is cost per day and provider.
type DailyUsage struct {
	Day      string  `json:"day"`
	Provider string  `json:"provider"`
	CostUSD  float64 `json:"cost_usd"`
	Tokens   int64   `json:"tokens"`
}

// RecordUsage stores an accounting row.
func (db *DB) RecordUsage(ctx context.Context, u UsageRecord) error {
	_, err := db.ExecContext(ctx, `INSERT INTO llm_usage (ts, provider, model, purpose, input_tokens, output_tokens, cost_usd) VALUES (?,?,?,?,?,?,?)`,
		now(), u.Provider, u.Model, u.Purpose, u.InputTokens, u.OutputTokens, u.CostUSD)
	return err
}

// UsageSummary aggregates usage since t grouped by provider/model.
func (db *DB) UsageSummary(ctx context.Context, since time.Time) ([]UsageRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT provider, model, COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(cost_usd)
		FROM llm_usage WHERE ts >= ? GROUP BY provider, model ORDER BY SUM(cost_usd) DESC`, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Provider, &r.Model, &r.Calls, &r.InputTokens, &r.OutputTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageDaily aggregates cost per day/provider since t.
func (db *DB) UsageDaily(ctx context.Context, since time.Time) ([]DailyUsage, error) {
	rows, err := db.QueryContext(ctx, `SELECT substr(ts,1,10) d, provider, SUM(cost_usd), SUM(input_tokens+output_tokens)
		FROM llm_usage WHERE ts >= ? GROUP BY d, provider ORDER BY d`, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyUsage
	for rows.Next() {
		var r DailyUsage
		if err := rows.Scan(&r.Day, &r.Provider, &r.CostUSD, &r.Tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- kv ----

// KVGet reads a value.
func (db *DB) KVGet(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// KVSet writes a value.
func (db *DB) KVSet(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, value, now())
	return err
}

// KVDelete removes a key.
func (db *DB) KVDelete(ctx context.Context, key string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, key)
	return err
}

// KVGetJSON decodes a JSON value into out.
func (db *DB) KVGetJSON(ctx context.Context, key string, out any) (bool, error) {
	v, ok, err := db.KVGet(ctx, key)
	if err != nil || !ok {
		return false, err
	}
	return true, json.Unmarshal([]byte(v), out)
}

// KVSetJSON encodes v as JSON.
func (db *DB) KVSetJSON(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return db.KVSet(ctx, key, string(b))
}

// ---- chat history ----

// ChatMessage is a persisted conversation turn.
type ChatMessage struct {
	ID      int64     `json:"id"`
	Channel string    `json:"channel"`
	Role    string    `json:"role"`
	Content string    `json:"content"`
	TS      time.Time `json:"ts"`
	Private bool      `json:"private,omitempty"` // content encrypted by the profile vault
}

// AppendChat stores a chat turn.
func (db *DB) AppendChat(ctx context.Context, channel, role, content string) error {
	return db.AppendChatTurn(ctx, channel, role, content, false)
}

// AppendChatTurn stores a chat turn; private turns (already encrypted by the caller) are
// never returned by ChatSince, so the memory routine does not read them.
func (db *DB) AppendChatTurn(ctx context.Context, channel, role, content string, private bool) error {
	_, err := db.ExecContext(ctx, `INSERT INTO chat_messages (channel, role, content, ts, private) VALUES (?,?,?,?,?)`, channel, role, content, now(), private)
	return err
}

// ChatHistory returns the last limit turns in chronological order.
func (db *DB) ChatHistory(ctx context.Context, channel string, limit int) ([]ChatMessage, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, channel, role, content, ts, private FROM (SELECT * FROM chat_messages WHERE channel=? ORDER BY id DESC LIMIT ?) ORDER BY id`, channel, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var ts string
		if err := rows.Scan(&m.ID, &m.Channel, &m.Role, &m.Content, &ts, &m.Private); err != nil {
			return nil, err
		}
		m.TS = parseTime(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClearChat deletes a channel history.
func (db *DB) ClearChat(ctx context.Context, channel string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM chat_messages WHERE channel=?`, channel)
	return err
}

// ---- seen items (dedupe for RSS/Gmail) ----

// MarkSeen records (ns, key) and reports whether it was new.
func (db *DB) MarkSeen(ctx context.Context, ns, key string) (bool, error) {
	res, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO seen_items (ns, key, seen_at) VALUES (?,?,?)`, ns, key, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Seen reports whether (ns, key) was recorded.
func (db *DB) Seen(ctx context.Context, ns, key string) (bool, error) {
	var x int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM seen_items WHERE ns=? AND key=?`, ns, key).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PurgeSeen removes seen markers older than before.
func (db *DB) PurgeSeen(ctx context.Context, before time.Time) error {
	_, err := db.ExecContext(ctx, `DELETE FROM seen_items WHERE seen_at < ?`, fmtTime(before))
	return err
}
