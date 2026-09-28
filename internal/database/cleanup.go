package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Cleanup suggestion kinds.
const (
	CleanupDuplicate     = "duplicate"      // same title and content, or same URL
	CleanupNearDuplicate = "near_duplicate" // almost identical text (embeddings)
	CleanupEmpty         = "empty"          // no text, no links
	CleanupStaleTask     = "stale_task"     // open for too long or long overdue
	CleanupLonelyPerson  = "lonely_person"  // person auto-created from a single mention
)

// Cleanup actions.
const (
	ActionMerge = "merge"
	ActionTrash = "trash"
	ActionDone  = "done"
)

// Cleanup suggestion statuses.
const (
	CleanupPending   = "pending"
	CleanupApplied   = "applied"
	CleanupDismissed = "dismissed"
)

// CleanupSuggestion is one proposal of the weekly cleanup. For merges the first node is kept.
type CleanupSuggestion struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Action    string    `json:"action"`
	NodeIDs   []int64   `json:"node_ids"`
	Reason    string    `json:"reason"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Nodes     []Node    `json:"nodes,omitempty"` // filled by ListCleanup, in NodeIDs order
}

func (c CleanupSuggestion) fingerprint() string {
	ids := slices.Clone(c.NodeIDs)
	slices.Sort(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return c.Kind + ":" + strings.Join(parts, ",")
}

// minNodes is how many nodes must still exist for the suggestion to make sense.
func minNodes(action string) int {
	if action == ActionMerge {
		return 2
	}
	return 1
}

// ReplaceCleanupSuggestions drops the pending suggestions and stores new ones. A proposal
// the user dismissed (or applied) is never stored again. It returns how many were stored per kind.
func (db *DB) ReplaceCleanupSuggestions(ctx context.Context, list []CleanupSuggestion) (map[string]int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cleanup_suggestions WHERE status = ?`, CleanupPending); err != nil {
		return nil, err
	}
	ts, added := now(), map[string]int{}
	for _, c := range list {
		if len(c.NodeIDs) < minNodes(c.Action) {
			continue
		}
		ids, _ := json.Marshal(c.NodeIDs)
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cleanup_suggestions (kind, action, node_ids, reason, fingerprint, status, created_at)
			VALUES (?,?,?,?,?,?,?)`, c.Kind, c.Action, string(ids), c.Reason, c.fingerprint(), CleanupPending, ts)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added[c.Kind]++
		}
	}
	return added, tx.Commit()
}

const cleanupCols = `id, kind, action, node_ids, reason, status, created_at`

func scanCleanup(s scanner) (CleanupSuggestion, error) {
	var c CleanupSuggestion
	var ids, created string
	if err := s.Scan(&c.ID, &c.Kind, &c.Action, &ids, &c.Reason, &c.Status, &created); err != nil {
		return c, err
	}
	_ = json.Unmarshal([]byte(ids), &c.NodeIDs)
	c.CreatedAt = parseTime(created)
	return c, nil
}

// ListCleanup returns suggestions with their nodes. Pending ones whose nodes were deleted
// meanwhile are discarded.
func (db *DB) ListCleanup(ctx context.Context, status string, limit int) ([]CleanupSuggestion, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+cleanupCols+` FROM cleanup_suggestions WHERE status = ? ORDER BY kind, id LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	var list []CleanupSuggestion
	for rows.Next() {
		c, err := scanCleanup(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var all []int64
	for _, c := range list {
		all = append(all, c.NodeIDs...)
	}
	byID := map[int64]Node{}
	err = chunks(all, func(part []int64) error {
		nodes, err := db.GetNodes(ctx, part)
		for _, n := range nodes {
			byID[n.ID] = n
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out := list[:0]
	var stale []int64
	for _, c := range list {
		for _, id := range c.NodeIDs {
			if n, ok := byID[id]; ok {
				c.Nodes = append(c.Nodes, n)
			}
		}
		if len(c.Nodes) < minNodes(c.Action) {
			stale = append(stale, c.ID)
			continue
		}
		out = append(out, c)
	}
	if status == CleanupPending && len(stale) > 0 {
		ph, args := inList(stale)
		if _, err := db.ExecContext(ctx, `DELETE FROM cleanup_suggestions WHERE id IN (`+ph+`)`, args...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetCleanup loads one suggestion (without nodes).
func (db *DB) GetCleanup(ctx context.Context, id int64) (*CleanupSuggestion, error) {
	c, err := scanCleanup(db.QueryRowContext(ctx, `SELECT `+cleanupCols+` FROM cleanup_suggestions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// ResolveCleanup marks a suggestion as applied or dismissed.
func (db *DB) ResolveCleanup(ctx context.Context, id int64, status string) error {
	_, err := db.ExecContext(ctx, `UPDATE cleanup_suggestions SET status = ?, resolved_at = ? WHERE id = ?`, status, now(), id)
	return err
}

// CleanupPendingCount counts pending suggestions whose nodes still exist.
func (db *DB) CleanupPendingCount(ctx context.Context) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cleanup_suggestions c WHERE c.status = ? AND
		(SELECT COUNT(*) FROM json_each(c.node_ids) j JOIN nodes n ON n.id = j.value) >= CASE c.action WHEN ? THEN 2 ELSE 1 END`,
		CleanupPending, ActionMerge).Scan(&n)
	return n, err
}

// ---- detectors ----

// DuplicateGroups returns groups of nodes of the same type with the same title and text, or
// the same URL; each group is ordered oldest first.
func (db *DB) DuplicateGroups(ctx context.Context, limit int) ([][]int64, error) {
	queries := []string{
		`SELECT group_concat(id) FROM (SELECT id, type, lower(trim(title)) AS t, trim(content) AS c FROM nodes WHERE type <> 'health' ORDER BY created_at, id)
			GROUP BY type, t, c HAVING COUNT(*) > 1 LIMIT ?`,
		`SELECT group_concat(id) FROM (SELECT id, type, json_extract(meta, '$.url') AS u FROM nodes
			WHERE type <> 'health' AND json_valid(meta) AND json_extract(meta, '$.url') <> '' ORDER BY created_at, id)
			GROUP BY type, u HAVING COUNT(*) > 1 LIMIT ?`,
	}
	parent := map[int64]int64{}
	var find func(int64) int64
	find = func(x int64) int64 {
		if p, ok := parent[x]; ok && p != x {
			parent[x] = find(p)
			return parent[x]
		}
		parent[x] = x
		return x
	}
	var order []int64
	for _, q := range queries {
		rows, err := db.QueryContext(ctx, q, limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			var ids []int64
			for _, p := range strings.Split(s, ",") {
				var id int64
				if _, err := fmt.Sscan(p, &id); err == nil {
					ids = append(ids, id)
				}
			}
			for _, id := range ids {
				if _, seen := parent[id]; !seen {
					order = append(order, id)
				}
				find(id)
			}
			for _, id := range ids[1:] {
				if a, b := find(ids[0]), find(id); a != b {
					parent[b] = a
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	idx := map[int64]int{}
	var groups [][]int64
	for _, id := range order { // order = first seen, oldest first inside each group
		r := find(id)
		i, ok := idx[r]
		if !ok {
			i = len(groups)
			idx[r] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], id)
	}
	out := groups[:0]
	for _, g := range groups {
		if len(g) > 1 {
			out = append(out, g)
		}
	}
	return out, nil
}

// EmptyNodes lists notes, articles and insights with no text and no links, older than before.
func (db *DB) EmptyNodes(ctx context.Context, before time.Time, limit int) ([]Node, error) {
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE type IN ('note','article','insight')
		AND trim(content) = '' AND trim(summary) = '' AND created_at < ?
		AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.source_id = nodes.id OR e.target_id = nodes.id)
		ORDER BY created_at LIMIT ?`, fmtTime(before), limit)
}

// StaleTasks lists open tasks untouched since idleBefore (no due date) or due before overdueBefore.
func (db *DB) StaleTasks(ctx context.Context, idleBefore, overdueBefore time.Time, limit int) ([]Node, error) {
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE type = 'task' AND status = 'open' AND (
		(due_at IS NULL AND updated_at < ?) OR (due_at IS NOT NULL AND due_at < ?))
		ORDER BY COALESCE(due_at, updated_at) LIMIT ?`, fmtTime(idleBefore), fmtTime(overdueBefore), limit)
}

// LonelyAutoPersons lists people auto-created from mentions that have no text and at most one link.
func (db *DB) LonelyAutoPersons(ctx context.Context, before time.Time, limit int) ([]Node, error) {
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE type = 'person' AND source = 'agent'
		AND trim(content) = '' AND created_at < ?
		AND (SELECT COUNT(*) FROM edges e WHERE e.source_id = nodes.id OR e.target_id = nodes.id) <= 1
		ORDER BY created_at LIMIT ?`, fmtTime(before), limit)
}

// ---- merge and small helpers ----

// MergeNodes folds others into keep: text that keep does not already contain is appended,
// tags are united, missing meta keys copied and every link re-pointed to keep. The others
// stay in place so the caller can move them to the trash (which keeps the merge undoable).
func (db *DB) MergeNodes(ctx context.Context, keep int64, others []int64) (*Node, error) {
	others = slices.DeleteFunc(slices.Clone(others), func(id int64) bool { return id == keep })
	if len(others) == 0 {
		return db.GetNode(ctx, keep)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ph, args := inList(append([]int64{keep}, others...))
	nodes, err := queryNodesTx(ctx, tx, `SELECT `+nodeCols+` FROM nodes WHERE id IN (`+ph+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	var k *Node
	var rest []Node
	for i := range nodes {
		if nodes[i].ID == keep {
			k = &nodes[i]
		} else {
			rest = append(rest, nodes[i])
		}
	}
	if k == nil {
		return nil, ErrNotFound
	}
	merged, _ := k.Meta["merged_from"].([]any)
	for _, o := range rest {
		if c := strings.TrimSpace(o.Content); c != "" && !strings.Contains(k.Content, c) {
			k.Content = strings.TrimSpace(k.Content + fmt.Sprintf("\n\n---\n_Mesclado de “%s” (#%d)_\n\n", o.Title, o.ID) + c)
		}
		if k.Summary == "" {
			k.Summary = o.Summary
		}
		if k.DueAt == nil {
			k.DueAt = o.DueAt
		}
		k.Tags = append(k.Tags, o.Tags...)
		for key, v := range o.Meta {
			if _, ok := k.Meta[key]; !ok {
				k.Meta[key] = v
			}
		}
		merged = append(merged, o.ID)
	}
	k.Meta["merged_from"] = merged
	k.Tags = NormalizeTags(k.Tags)
	k.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET content=?, summary=?, tags=?, due_at=?, meta=?, updated_at=? WHERE id=?`,
		k.Content, k.Summary, joinTags(k.Tags), dueStr(k.DueAt), metaJSON(k.Meta), fmtTime(k.UpdatedAt), k.ID); err != nil {
		return nil, err
	}
	oph, oargs := inList(others)
	// Copy (not move) the links: the trashed nodes keep theirs for an eventual restore.
	q := `INSERT OR IGNORE INTO edges (source_id, target_id, relation, weight, created_at)
		SELECT s, t, relation, weight, created_at FROM (
			SELECT CASE WHEN source_id IN (` + oph + `) THEN ? ELSE source_id END AS s,
			       CASE WHEN target_id IN (` + oph + `) THEN ? ELSE target_id END AS t, relation, weight, created_at
			FROM edges WHERE source_id IN (` + oph + `) OR target_id IN (` + oph + `))
		WHERE s <> t`
	qargs := append(append(append([]any{}, oargs...), keep), oargs...)
	qargs = append(qargs, keep)
	qargs = append(append(qargs, oargs...), oargs...)
	if _, err := tx.ExecContext(ctx, q, qargs...); err != nil {
		return nil, err
	}
	return k, tx.Commit()
}

// SetMetaKey sets one meta key without touching updated_at (bookkeeping by routines).
func (db *DB) SetMetaKey(ctx context.Context, id int64, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE nodes SET meta = json_set(CASE WHEN json_valid(meta) THEN meta ELSE '{}' END, '$.'||?, json(?)) WHERE id = ?`,
		key, string(b), id)
	return err
}

// ChatSince returns chat turns (every channel) from since on, oldest first.
func (db *DB) ChatSince(ctx context.Context, since time.Time, limit int) ([]ChatMessage, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, channel, role, content, ts FROM chat_messages WHERE ts >= ? ORDER BY id LIMIT ?`, fmtTime(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var ts string
		if err := rows.Scan(&m.ID, &m.Channel, &m.Role, &m.Content, &ts); err != nil {
			return nil, err
		}
		m.TS = parseTime(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChangedSince lists nodes of the given types created or updated in [from, to), oldest first.
func (db *DB) ChangedSince(ctx context.Context, types []string, from, to time.Time, limit int) ([]Node, error) {
	f := NodeFilter{Types: types}
	where, args := f.where("")
	args = append(args, fmtTime(from), fmtTime(to), limit)
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE `+where+` AND updated_at >= ? AND updated_at < ? ORDER BY updated_at LIMIT ?`, args...)
}

// DerivedTaskCount counts tasks linked to id by "derived_from" (tasks extracted from it).
func (db *DB) DerivedTaskCount(ctx context.Context, id int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges e JOIN nodes t ON t.id = e.source_id
		WHERE e.target_id = ? AND e.relation = 'derived_from' AND t.type = 'task'`, id).Scan(&n)
	return n, err
}

// DerivedOrigins maps each task id to the node it was extracted from (first "derived_from" link).
func (db *DB) DerivedOrigins(ctx context.Context, ids []int64) (map[int64]Node, error) {
	out := map[int64]Node{}
	err := chunks(ids, func(part []int64) error {
		ph, args := inList(part)
		rows, err := db.QueryContext(ctx, `SELECT e.source_id, n.`+strings.ReplaceAll(nodeCols, ", ", ", n.")+` FROM edges e JOIN nodes n ON n.id = e.target_id
			WHERE e.relation = 'derived_from' AND e.source_id IN (`+ph+`) ORDER BY e.id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var task int64
			var n Node
			var tags, meta, created, updated string
			var due sql.NullString
			if err := rows.Scan(&task, &n.ID, &n.UID, &n.Type, &n.Title, &n.Content, &n.Summary, &tags, &n.Source, &n.SourceRef, &n.Status, &due, &meta, &created, &updated); err != nil {
				return err
			}
			if _, ok := out[task]; ok {
				continue
			}
			n.Tags = SplitTags(tags)
			n.Meta = map[string]any{}
			_ = json.Unmarshal([]byte(meta), &n.Meta)
			n.CreatedAt, n.UpdatedAt = parseTime(created), parseTime(updated)
			if due.Valid && due.String != "" {
				t := parseTime(due.String)
				n.DueAt = &t
			}
			out[task] = n
		}
		return rows.Err()
	})
	return out, err
}

// CreatedPerDay counts nodes created per local day from from on (keys "2006-01-02").
func (db *DB) CreatedPerDay(ctx context.Context, from time.Time, loc *time.Location) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT created_at FROM nodes WHERE created_at >= ?`, fmtTime(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[parseTime(s).In(loc).Format("2006-01-02")]++
	}
	return out, rows.Err()
}
