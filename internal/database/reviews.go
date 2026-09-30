package database

import (
	"context"
	"time"
)

// Review is the spaced-repetition state of one node.
type Review struct {
	NodeID int64
	Step   int       // how many times it was already shown
	Cursor int       // which highlight of the node was shown last (books hold many)
	NextAt time.Time // when it is due again
	LastAt time.Time
}

// reviewCandidateSQL selects what is worth reviewing: insights (not periodic reports), Kindle books
// and the learnings distilled by the memory routine.
const reviewCandidateSQL = `(
	(type = 'insight' AND source NOT IN ('routine', 'memory', 'rss', 'newsletter', 'youtube', 'health'))
	OR source = 'import:kindle'
	OR (source = 'memory' AND (',' || tags || ',') LIKE '%,aprendizado,%'))`

// ReviewCandidates picks random nodes worth reviewing that are not enrolled yet.
func (db *DB) ReviewCandidates(ctx context.Context, limit int) ([]Node, error) {
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE `+reviewCandidateSQL+` AND trim(content) <> ''
		AND id NOT IN (SELECT node_id FROM reviews) ORDER BY RANDOM() LIMIT ?`, limit)
}

// EnrollReview starts reviewing a node now (no-op when already enrolled).
func (db *DB) EnrollReview(ctx context.Context, id int64, at time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO reviews (node_id, step, cursor, next_at) VALUES (?, 0, 0, ?)`, id, fmtTime(at))
	return err
}

// DueReviews lists reviews due at now, oldest due first.
func (db *DB) DueReviews(ctx context.Context, now time.Time, limit int) ([]Review, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.node_id, r.step, r.cursor, r.next_at, COALESCE(r.last_at, '') FROM reviews r
		JOIN nodes n ON n.id = r.node_id WHERE r.next_at <= ? ORDER BY r.next_at, r.node_id LIMIT ?`, fmtTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		var r Review
		var next, last string
		if err := rows.Scan(&r.NodeID, &r.Step, &r.Cursor, &next, &last); err != nil {
			return nil, err
		}
		r.NextAt, r.LastAt = parseTime(next), parseTime(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveReview stores the state after a node was shown.
func (db *DB) SaveReview(ctx context.Context, r Review) error {
	_, err := db.ExecContext(ctx, `UPDATE reviews SET step = ?, cursor = ?, next_at = ?, last_at = ? WHERE node_id = ?`,
		r.Step, r.Cursor, fmtTime(r.NextAt), fmtTime(r.LastAt), r.NodeID)
	return err
}

// ReviewStats counts enrolled nodes and how many are due at now.
func (db *DB) ReviewStats(ctx context.Context, now time.Time) (total, due int, err error) {
	err = db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(next_at <= ?), 0) FROM reviews`, fmtTime(now)).Scan(&total, &due)
	return
}
