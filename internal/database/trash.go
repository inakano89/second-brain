package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/crypto"
)

// ErrDeleted is returned when content the user deleted (asking not to bring it back) arrives again.
var ErrDeleted = errors.New("conteúdo apagado pelo usuário")

// TrashRetention is how long deleted nodes stay restorable.
const TrashRetention = 30 * 24 * time.Hour

// AutoPersonSource keys tombstones of people auto-created from mentions (ref = lowercase name).
const AutoPersonSource = "agent:person"

// TrashItem is a deleted node waiting in the trash.
type TrashItem struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Source    string    `json:"source"`
	Batch     string    `json:"batch"`
	DeletedAt time.Time `json:"deleted_at"`
}

// TrashBatch summarises one delete operation.
type TrashBatch struct {
	Batch     string    `json:"batch"`
	Count     int       `json:"count"`
	Title     string    `json:"title"` // one of the titles, for display
	DeletedAt time.Time `json:"deleted_at"`
}

type trashedNode struct {
	Node  Node   `json:"node"`
	Edges []Edge `json:"edges"`
}

// idChunk keeps IN (...) lists well below SQLite's variable limit.
const idChunk = 400

func inList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

func chunks(ids []int64, fn func([]int64) error) error {
	for start := 0; start < len(ids); start += idChunk {
		if err := fn(ids[start:min(len(ids), start+idChunk)]); err != nil {
			return err
		}
	}
	return nil
}

// tombstone returns the key that stops n from being re-imported.
func tombstone(n *Node) (source, ref string, ok bool) {
	switch {
	case n.SourceRef != "":
		return n.Source, n.SourceRef, true
	case n.Type == TypePerson && n.Source == "agent":
		return AutoPersonSource, strings.ToLower(strings.TrimSpace(n.Title)), true
	}
	return "", "", false
}

func queryNodesTx(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]Node, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// TrashNodes moves nodes (with their edges) to the trash in one transaction. With forget,
// integrations and imports will not bring them back. It returns the batch id used to undo.
func (db *DB) TrashNodes(ctx context.Context, ids []int64, forget bool) (batch string, count int, err error) {
	if len(ids) == 0 {
		return "", 0, nil
	}
	batch = crypto.RandomToken(6)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	ts := now()
	var removed []int64
	err = chunks(ids, func(part []int64) error {
		ph, args := inList(part)
		nodes, err := queryNodesTx(ctx, tx, `SELECT `+nodeCols+` FROM nodes WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		edges := map[int64][]Edge{}
		rows, err := tx.QueryContext(ctx, `SELECT id, source_id, target_id, relation, weight, created_at FROM edges
			WHERE source_id IN (`+ph+`) OR target_id IN (`+ph+`)`, append(args, args...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e Edge
			var created string
			if err := rows.Scan(&e.ID, &e.SourceID, &e.TargetID, &e.Relation, &e.Weight, &created); err != nil {
				rows.Close()
				return err
			}
			e.CreatedAt = parseTime(created)
			edges[e.SourceID] = append(edges[e.SourceID], e)
			if e.TargetID != e.SourceID {
				edges[e.TargetID] = append(edges[e.TargetID], e)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range nodes {
			n := &nodes[i]
			body, err := json.Marshal(trashedNode{Node: *n, Edges: edges[n.ID]})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO trash (id, type, title, source, node, batch, deleted_at) VALUES (?,?,?,?,?,?,?)`,
				n.ID, n.Type, n.Title, n.Source, string(body), batch, ts); err != nil {
				return err
			}
			if src, ref, ok := tombstone(n); ok && forget {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO deleted_refs (source, source_ref, deleted_at) VALUES (?,?,?)`, src, ref, ts); err != nil {
					return err
				}
			}
			removed = append(removed, n.ID)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM nodes WHERE id IN (`+ph+`)`, args...)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	for _, id := range removed {
		db.vec.remove(id)
	}
	return batch, len(removed), nil
}

// RestoreTrash puts trashed nodes back with their edges (when the other end still exists)
// and lifts their tombstones. A node whose source item came back meanwhile is dropped from
// the trash instead of duplicated. Embeddings are rebuilt by the re-embed job.
func (db *DB) RestoreTrash(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	restored := 0
	var edges []Edge
	err = chunks(ids, func(part []int64) error {
		ph, args := inList(part)
		rows, err := tx.QueryContext(ctx, `SELECT node FROM trash WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		var items []trashedNode
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				rows.Close()
				return err
			}
			var t trashedNode
			if json.Unmarshal([]byte(body), &t) == nil && t.Node.ID != 0 {
				items = append(items, t)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range items {
			n := t.Node
			if src, ref, ok := tombstone(&n); ok {
				if _, err := tx.ExecContext(ctx, `DELETE FROM deleted_refs WHERE source = ? AND source_ref = ?`, src, ref); err != nil {
					return err
				}
			}
			var exists int
			if n.SourceRef != "" {
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE source = ? AND source_ref = ?`, n.Source, n.SourceRef).Scan(&exists); err != nil {
					return err
				}
			}
			if exists == 0 {
				res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO nodes (id, uid, type, title, content, summary, tags, source, source_ref, status, due_at, meta, created_at, updated_at)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					n.ID, n.UID, n.Type, n.Title, n.Content, n.Summary, joinTags(n.Tags), n.Source, n.SourceRef, n.Status, dueStr(n.DueAt), metaJSON(n.Meta), fmtTime(n.CreatedAt), fmtTime(n.UpdatedAt))
				if err != nil {
					return err
				}
				if c, _ := res.RowsAffected(); c > 0 {
					restored++
					edges = append(edges, t.Edges...)
				}
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM trash WHERE id IN (`+ph+`)`, args...)
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, e := range edges { // after every node is back, so links inside the batch survive
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO edges (source_id, target_id, relation, weight, created_at)
			SELECT ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM nodes WHERE id = ?) AND EXISTS (SELECT 1 FROM nodes WHERE id = ?)`,
			e.SourceID, e.TargetID, e.Relation, e.Weight, fmtTime(e.CreatedAt), e.SourceID, e.TargetID); err != nil {
			return 0, err
		}
	}
	return restored, tx.Commit()
}

// RestoreBatch restores every node deleted by one TrashNodes call.
func (db *DB) RestoreBatch(ctx context.Context, batch string) (int, error) {
	ids, err := db.TrashBatchIDs(ctx, batch)
	if err != nil {
		return 0, err
	}
	return db.RestoreTrash(ctx, ids)
}

// TrashBatchIDs lists the node ids deleted by one TrashNodes call.
func (db *DB) TrashBatchIDs(ctx context.Context, batch string) ([]int64, error) {
	return db.int64s(ctx, `SELECT id FROM trash WHERE batch = ?`, batch)
}

func (db *DB) int64s(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListTrash lists deleted nodes, newest first, optionally filtered by title and batch.
func (db *DB) ListTrash(ctx context.Context, query, batch string, limit, offset int) ([]TrashItem, int, error) {
	where, args := "1=1", []any{}
	if q := strings.TrimSpace(query); q != "" {
		where += ` AND title LIKE ?`
		args = append(args, "%"+q+"%")
	}
	if batch != "" {
		where += ` AND batch = ?`
		args = append(args, batch)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trash WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx, `SELECT id, type, title, source, batch, deleted_at FROM trash WHERE `+where+`
		ORDER BY deleted_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []TrashItem
	for rows.Next() {
		var t TrashItem
		var at string
		if err := rows.Scan(&t.ID, &t.Type, &t.Title, &t.Source, &t.Batch, &at); err != nil {
			return nil, 0, err
		}
		t.DeletedAt = parseTime(at)
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// TrashBatches groups the trash by delete operation, newest first.
func (db *DB) TrashBatches(ctx context.Context, limit int) ([]TrashBatch, error) {
	rows, err := db.QueryContext(ctx, `SELECT batch, COUNT(*), MIN(title), MAX(deleted_at) FROM trash GROUP BY batch ORDER BY MAX(deleted_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashBatch
	for rows.Next() {
		var b TrashBatch
		var at string
		if err := rows.Scan(&b.Batch, &b.Count, &b.Title, &at); err != nil {
			return nil, err
		}
		b.DeletedAt = parseTime(at)
		out = append(out, b)
	}
	return out, rows.Err()
}

// PurgeTrash permanently deletes trashed nodes: the given ids, or (ids nil) every node
// deleted before the cutoff. It returns the media files the purged nodes referenced.
func (db *DB) PurgeTrash(ctx context.Context, ids []int64, before time.Time) (files []string, n int, err error) {
	if ids == nil {
		if ids, err = db.int64s(ctx, `SELECT id FROM trash WHERE deleted_at < ?`, fmtTime(before)); err != nil {
			return nil, 0, err
		}
	}
	err = chunks(ids, func(part []int64) error {
		ph, args := inList(part)
		rows, err := db.QueryContext(ctx, `SELECT json_extract(node, '$.node.meta.file') FROM trash WHERE id IN (`+ph+`) AND json_valid(node)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var f sql.NullString
			if err := rows.Scan(&f); err != nil {
				rows.Close()
				return err
			}
			if f.Valid && f.String != "" {
				files = append(files, f.String)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		res, err := db.ExecContext(ctx, `DELETE FROM trash WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n += int(c)
		return nil
	})
	return files, n, err
}

// TrashCount returns how many nodes are in the trash.
func (db *DB) TrashCount(ctx context.Context) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trash`).Scan(&n)
	return n, err
}

// IsDeletedRef reports whether the user deleted (source, ref) and asked not to bring it back.
func (db *DB) IsDeletedRef(ctx context.Context, source, ref string) (bool, error) {
	if ref == "" {
		return false, nil
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_refs WHERE source = ? AND source_ref = ?`, source, ref).Scan(&n)
	return n > 0, err
}

// ForgetDeletedRef lifts one tombstone (the user sent the item again on purpose).
func (db *DB) ForgetDeletedRef(ctx context.Context, source, ref string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM deleted_refs WHERE source = ? AND source_ref = ?`, source, ref)
	return err
}

// DeletedRefCount returns how many items are blocked from coming back.
func (db *DB) DeletedRefCount(ctx context.Context) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_refs`).Scan(&n)
	return n, err
}

// ClearDeletedRefs lets every deleted item be imported again.
func (db *DB) ClearDeletedRefs(ctx context.Context) (int, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM deleted_refs`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// UpdateNodes applies fn to each node and saves the ones it reports as changed, in one
// transaction per chunk. It returns how many nodes changed.
func (db *DB) UpdateNodes(ctx context.Context, ids []int64, fn func(n *Node) bool) (int, error) {
	changed := 0
	err := chunks(ids, func(part []int64) error {
		ph, args := inList(part)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		nodes, err := queryNodesTx(ctx, tx, `SELECT `+nodeCols+` FROM nodes WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		ts := time.Now().UTC()
		for i := range nodes {
			n := &nodes[i]
			if !fn(n) {
				continue
			}
			if !ValidType(n.Type) {
				return fmt.Errorf("invalid node type %q", n.Type)
			}
			n.UpdatedAt = ts
			if _, err := tx.ExecContext(ctx, `UPDATE nodes SET type=?, title=?, content=?, summary=?, tags=?, status=?, due_at=?, meta=?, updated_at=? WHERE id=?`,
				n.Type, n.Title, n.Content, n.Summary, joinTags(n.Tags), n.Status, dueStr(n.DueAt), metaJSON(n.Meta), fmtTime(n.UpdatedAt), n.ID); err != nil {
				return err
			}
			changed++
		}
		return tx.Commit()
	})
	return changed, err
}
