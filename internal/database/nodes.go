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
	"unicode"

	"github.com/inakano89/second-brain/internal/crypto"
)

// Node types.
const (
	TypeNote    = "note"
	TypeTask    = "task"
	TypePerson  = "person"
	TypeEvent   = "event"
	TypeInsight = "insight"
	TypeArticle = "article"
	TypeHealth  = "health"
)

// NodeTypes lists every valid node type.
var NodeTypes = []string{TypeNote, TypeTask, TypePerson, TypeEvent, TypeInsight, TypeArticle, TypeHealth}

// ValidType reports whether t is a known node type.
func ValidType(t string) bool {
	for _, x := range NodeTypes {
		if x == t {
			return true
		}
	}
	return false
}

// Task statuses.
const (
	StatusOpen = "open"
	StatusDone = "done"
)

// Node is a vertex of the knowledge graph.
type Node struct {
	ID        int64          `json:"id"`
	UID       string         `json:"uid"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Content   string         `json:"content"`
	Summary   string         `json:"summary"`
	Tags      []string       `json:"tags"`
	Source    string         `json:"source"`
	SourceRef string         `json:"source_ref"`
	Status    string         `json:"status"`
	DueAt     *time.Time     `json:"due_at,omitempty"`
	Meta      map[string]any `json:"meta"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// NodeFilter restricts listings and searches.
type NodeFilter struct {
	Types   []string
	From    *time.Time
	To      *time.Time
	Tag     string
	Source  string
	Sources []string // any of these sources
	Status  string
	Text    string // full-text match, every word (listings only; Matches ignores it)
	Batch   string // meta.import_batch
	Special string // "dup" same type and title, "empty" no content, "orphan" no links (listings only)
	Limit   int
	Offset  int
	Order   string // "created" (default), "date", "oldest", "updated", "due", "title"

	Origin         string     // OriginImported, OriginMine or OriginAuto
	KnownDate      bool       // leave out items whose original date is unknown
	HideOldImports *time.Time // leave out imported items with a known date before this (chat archive)
}

// ScoredNode pairs a node with a relevance score.
type ScoredNode struct {
	Node
	Score float64 `json:"score"`
}

const nodeCols = `id, uid, type, title, content, summary, tags, source, source_ref, status, due_at, meta, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanNode(s scanner) (*Node, error) {
	var n Node
	var tags, meta, created, updated string
	var due sql.NullString
	if err := s.Scan(&n.ID, &n.UID, &n.Type, &n.Title, &n.Content, &n.Summary, &tags, &n.Source, &n.SourceRef, &n.Status, &due, &meta, &created, &updated); err != nil {
		return nil, err
	}
	n.Tags = SplitTags(tags)
	if due.Valid && due.String != "" {
		t := parseTime(due.String)
		n.DueAt = &t
	}
	n.Meta = map[string]any{}
	_ = json.Unmarshal([]byte(meta), &n.Meta)
	n.CreatedAt = parseTime(created)
	n.UpdatedAt = parseTime(updated)
	return &n, nil
}

// SplitTags parses the comma-separated tag column.
func SplitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// NormalizeTags lowercases, strips '#', dedupes and caps tags.
func NormalizeTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "#")))
		t = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '/' {
				return r
			}
			if unicode.IsSpace(r) {
				return '-'
			}
			return -1
		}, t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= 16 {
			break
		}
	}
	return out
}

func joinTags(t []string) string { return strings.Join(NormalizeTags(t), ",") }

func metaJSON(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func dueStr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return fmtTime(*t)
}

// CreateNode inserts n and fills ID/UID/timestamps.
func (db *DB) CreateNode(ctx context.Context, n *Node) error {
	if !ValidType(n.Type) {
		return fmt.Errorf("invalid node type %q", n.Type)
	}
	if strings.TrimSpace(n.Title) == "" {
		n.Title = "Sem título"
	}
	if n.UID == "" {
		n.UID = crypto.RandomToken(8)
	}
	if n.Type == TypeTask && n.Status == "" {
		n.Status = StatusOpen
	}
	t := time.Now().UTC()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = t
	}
	n.UpdatedAt = t
	n.Tags = NormalizeTags(n.Tags)
	res, err := db.ExecContext(ctx, `INSERT INTO nodes (uid, type, title, content, summary, tags, source, source_ref, status, due_at, meta, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.UID, n.Type, n.Title, n.Content, n.Summary, joinTags(n.Tags), n.Source, n.SourceRef, n.Status, dueStr(n.DueAt), metaJSON(n.Meta), fmtTime(n.CreatedAt), fmtTime(n.UpdatedAt))
	if err != nil {
		return err
	}
	n.ID, err = res.LastInsertId()
	return err
}

// UpsertBySource creates the node or updates the one with the same (source, source_ref).
func (db *DB) UpsertBySource(ctx context.Context, n *Node) (created bool, err error) {
	if n.SourceRef == "" {
		return true, db.CreateNode(ctx, n)
	}
	existing, err := db.GetNodeBySource(ctx, n.Source, n.SourceRef)
	if errors.Is(err, ErrNotFound) {
		return true, db.CreateNode(ctx, n)
	}
	if err != nil {
		return false, err
	}
	incoming, wasUnknown := n.CreatedAt, existing.DateUnknown()
	dateFound := !incoming.IsZero() && !n.DateUnknown() && wasUnknown // a re-import finally brings the original date
	n.ID, n.UID, n.CreatedAt = existing.ID, existing.UID, existing.CreatedAt
	if n.Status == "" {
		n.Status = existing.Status
	}
	if n.Summary == "" {
		n.Summary = existing.Summary
	}
	if len(n.Tags) == 0 {
		n.Tags = existing.Tags
	}
	merged := existing.Meta
	for k, v := range n.Meta {
		merged[k] = v
	}
	if wasUnknown && !dateFound {
		merged[MetaDateUnknown] = true
	} else {
		delete(merged, MetaDateUnknown) // the stored date stays the original one
	}
	n.Meta = merged
	if err := db.UpdateNode(ctx, n); err != nil {
		return false, err
	}
	if dateFound {
		n.CreatedAt = incoming
		return false, db.SetCreatedAt(ctx, n.ID, incoming)
	}
	return false, nil
}

// GetNode loads a node by id.
func (db *DB) GetNode(ctx context.Context, id int64) (*Node, error) {
	n, err := scanNode(db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// GetNodeBySource loads a node by (source, source_ref).
func (db *DB) GetNodeBySource(ctx context.Context, source, ref string) (*Node, error) {
	n, err := scanNode(db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE source = ? AND source_ref = ?`, source, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// FindByTitle finds a node by case-insensitive title (typ optional).
func (db *DB) FindByTitle(ctx context.Context, typ, title string) (*Node, error) {
	q := `SELECT ` + nodeCols + ` FROM nodes WHERE title = ? COLLATE NOCASE`
	args := []any{strings.TrimSpace(title)}
	if typ != "" {
		q += ` AND type = ?`
		args = append(args, typ)
	}
	q += ` ORDER BY id LIMIT 1`
	n, err := scanNode(db.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// GetNodes loads many nodes by id (order not guaranteed).
func (db *DB) GetNodes(ctx context.Context, ids []int64) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id IN (`+ph+`)`, args...)
}

func (db *DB) queryNodes(ctx context.Context, q string, args ...any) ([]Node, error) {
	rows, err := db.QueryContext(ctx, q, args...)
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

// UpdateNode persists all mutable fields of n.
func (db *DB) UpdateNode(ctx context.Context, n *Node) error {
	if !ValidType(n.Type) {
		return fmt.Errorf("invalid node type %q", n.Type)
	}
	n.UpdatedAt = time.Now().UTC()
	n.Tags = NormalizeTags(n.Tags)
	res, err := db.ExecContext(ctx, `UPDATE nodes SET type=?, title=?, content=?, summary=?, tags=?, source=?, source_ref=?, status=?, due_at=?, meta=?, updated_at=? WHERE id=?`,
		n.Type, n.Title, n.Content, n.Summary, joinTags(n.Tags), n.Source, n.SourceRef, n.Status, dueStr(n.DueAt), metaJSON(n.Meta), fmtTime(n.UpdatedAt), n.ID)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatus updates a node status (tasks).
func (db *DB) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := db.ExecContext(ctx, `UPDATE nodes SET status=?, updated_at=? WHERE id=?`, status, now(), id)
	return err
}

// DeleteNode removes a node and (via cascade) its edges and embedding.
func (db *DB) DeleteNode(ctx context.Context, id int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, id); err != nil {
		return err
	}
	db.vec.remove(id)
	return nil
}

func (f NodeFilter) where(alias string) (string, []any) {
	var conds []string
	var args []any
	col := func(c string) string {
		if alias == "" {
			return c
		}
		return alias + "." + c
	}
	if len(f.Types) > 0 {
		conds = append(conds, col("type")+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Types)), ",")+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	if f.From != nil {
		conds = append(conds, col("created_at")+" >= ?")
		args = append(args, fmtTime(*f.From))
	}
	if f.To != nil {
		conds = append(conds, col("created_at")+" < ?")
		args = append(args, fmtTime(*f.To))
	}
	if f.Tag != "" {
		conds = append(conds, "(','||"+col("tags")+"||',') LIKE ?")
		args = append(args, "%,"+strings.ToLower(strings.TrimPrefix(f.Tag, "#"))+",%")
	}
	if f.Source != "" {
		conds = append(conds, col("source")+" = ?")
		args = append(args, f.Source)
	}
	if len(f.Sources) > 0 {
		conds = append(conds, col("source")+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Sources)), ",")+")")
		for _, s := range f.Sources {
			args = append(args, s)
		}
	}
	if f.Status != "" {
		conds = append(conds, col("status")+" = ?")
		args = append(args, f.Status)
	}
	if m := FTSQueryAll(f.Text); m != "" {
		conds = append(conds, col("id")+" IN (SELECT rowid FROM nodes_fts WHERE nodes_fts MATCH ?)")
		args = append(args, m)
	}
	if f.Batch != "" {
		conds = append(conds, "json_extract(CASE WHEN json_valid("+col("meta")+") THEN "+col("meta")+" ELSE '{}' END, '$.import_batch') = ?")
		args = append(args, f.Batch)
	}
	switch f.Origin {
	case OriginImported:
		conds = append(conds, col("source")+" LIKE 'import:%'")
	case OriginMine:
		conds = append(conds, manualSourcesSQL(col))
	case OriginAuto:
		conds = append(conds, col("source")+" NOT LIKE 'import:%' AND NOT "+manualSourcesSQL(col))
	}
	if f.KnownDate {
		conds = append(conds, "NOT "+dateUnknownSQL(col))
	}
	if f.HideOldImports != nil {
		conds = append(conds, "NOT ("+col("source")+" LIKE 'import:%' AND "+col("created_at")+" < ? AND NOT "+dateUnknownSQL(col)+")")
		args = append(args, fmtTime(*f.HideOldImports))
	}
	switch f.Special {
	case "dup":
		conds = append(conds, "("+col("type")+", lower("+col("title")+")) IN (SELECT type, lower(title) FROM nodes GROUP BY type, lower(title) HAVING COUNT(*) > 1)")
	case "empty":
		conds = append(conds, "trim("+col("content")+") = '' AND trim("+col("summary")+") = ''")
	case "orphan":
		conds = append(conds, "NOT EXISTS (SELECT 1 FROM edges e WHERE e.source_id = "+col("id")+" OR e.target_id = "+col("id")+")")
	}
	if len(conds) == 0 {
		return "1=1", nil
	}
	return strings.Join(conds, " AND "), args
}

// order returns the ORDER BY clause. Items with an unknown original date always come last in
// the date orders: their created_at is only the import moment.
func (f NodeFilter) order() string {
	unknown := dateUnknownSQL(plainCol) + " ASC, "
	switch f.Order {
	case "oldest":
		return unknown + "created_at ASC, id ASC"
	case "updated":
		return "updated_at DESC"
	case "due":
		return "due_at IS NULL, due_at ASC, created_at DESC"
	case "title":
		return "lower(title), type, id"
	case "date": // events by the day they happen, everything else by creation
		return unknown + "CASE WHEN type = 'event' AND due_at IS NOT NULL THEN due_at ELSE created_at END DESC, id DESC"
	}
	return unknown + "created_at DESC, id DESC"
}

// ListNodes lists nodes matching f.
func (db *DB) ListNodes(ctx context.Context, f NodeFilter) ([]Node, error) {
	where, args := f.where("")
	order := f.order()
	limit := f.Limit
	if limit <= 0 || limit > 5000 {
		limit = 100
	}
	args = append(args, limit, f.Offset)
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, args...)
}

// CountNodes counts nodes matching f.
func (db *DB) CountNodes(ctx context.Context, f NodeFilter) (int, error) {
	where, args := f.where("")
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE `+where, args...).Scan(&n)
	return n, err
}

// NodeIDs returns the ids of every node matching f (up to max), in listing order.
func (db *DB) NodeIDs(ctx context.Context, f NodeFilter, max int) ([]int64, error) {
	where, args := f.where("")
	return db.int64s(ctx, `SELECT id FROM nodes WHERE `+where+` ORDER BY `+f.order()+` LIMIT ?`, append(args, max)...)
}

// ImportBatch is one import run as recorded in its nodes.
type ImportBatch struct {
	Batch  string    `json:"batch"`
	Name   string    `json:"name"`
	Source string    `json:"source"`
	Count  int       `json:"count"`
	At     time.Time `json:"at"`
}

// ImportBatches lists import runs that still have nodes, newest first.
func (db *DB) ImportBatches(ctx context.Context, limit int) ([]ImportBatch, error) {
	rows, err := db.QueryContext(ctx, `SELECT b, MAX(name), MIN(source), COUNT(*), MAX(at) FROM (
			SELECT json_extract(meta, '$.import_batch') AS b, COALESCE(json_extract(meta, '$.import_name'), '') AS name,
				source, COALESCE(json_extract(meta, '$.import_at'), created_at) AS at
			FROM nodes WHERE source LIKE 'import:%' AND json_valid(meta))
		WHERE b IS NOT NULL AND b <> '' GROUP BY b ORDER BY MAX(at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportBatch
	for rows.Next() {
		var b ImportBatch
		var at string
		if err := rows.Scan(&b.Batch, &b.Name, &b.Source, &b.Count, &at); err != nil {
			return nil, err
		}
		b.At = parseTime(at)
		out = append(out, b)
	}
	return out, rows.Err()
}

// Matches reports whether n satisfies f (used to post-filter vector hits).
func (f NodeFilter) Matches(n *Node) bool {
	if len(f.Types) > 0 {
		ok := false
		for _, t := range f.Types {
			if n.Type == t {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.From != nil && n.CreatedAt.Before(*f.From) {
		return false
	}
	if f.To != nil && !n.CreatedAt.Before(*f.To) {
		return false
	}
	if f.Source != "" && n.Source != f.Source {
		return false
	}
	if len(f.Sources) > 0 && !slices.Contains(f.Sources, n.Source) {
		return false
	}
	if f.Status != "" && n.Status != f.Status {
		return false
	}
	if f.Batch != "" && n.Meta["import_batch"] != f.Batch {
		return false
	}
	switch f.Origin {
	case OriginImported:
		if !n.Imported() {
			return false
		}
	case OriginMine:
		if !slices.Contains(ManualSources, n.Source) {
			return false
		}
	case OriginAuto:
		if n.Imported() || slices.Contains(ManualSources, n.Source) {
			return false
		}
	}
	if f.KnownDate && n.DateUnknown() {
		return false
	}
	if f.HideOldImports != nil && n.Imported() && !n.DateUnknown() && n.CreatedAt.Before(*f.HideOldImports) {
		return false
	}
	if f.Tag != "" {
		want := strings.ToLower(strings.TrimPrefix(f.Tag, "#"))
		found := false
		for _, t := range n.Tags {
			if t == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// FTSQuery converts free text into a safe FTS5 MATCH expression (prefix OR terms).
func FTSQuery(q string) string {
	var terms []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		w = strings.ToLower(w)
		if len([]rune(w)) < 2 || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"*`)
		if len(terms) >= 16 {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// FTSQueryAll converts free text into an FTS5 expression requiring every word (as prefix).
func FTSQueryAll(q string) string {
	return strings.ReplaceAll(FTSQuery(q), " OR ", " ")
}

// SearchFTS runs a BM25-ranked full-text query.
func (db *DB) SearchFTS(ctx context.Context, query string, f NodeFilter, limit int) ([]ScoredNode, error) {
	m := FTSQuery(query)
	if m == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	where, args := f.where("n")
	q := `SELECT n.id, n.uid, n.type, n.title, n.content, n.summary, n.tags, n.source, n.source_ref, n.status, n.due_at, n.meta, n.created_at, n.updated_at,
		bm25(nodes_fts, 10.0, 1.0, 3.0, 5.0) AS rank
		FROM nodes_fts JOIN nodes n ON n.id = nodes_fts.rowid
		WHERE nodes_fts MATCH ? AND ` + where + ` ORDER BY rank LIMIT ?`
	all := append([]any{m}, args...)
	all = append(all, limit)
	rows, err := db.QueryContext(ctx, q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoredNode
	for rows.Next() {
		var n Node
		var tags, meta, created, updated string
		var due sql.NullString
		var rank float64
		if err := rows.Scan(&n.ID, &n.UID, &n.Type, &n.Title, &n.Content, &n.Summary, &tags, &n.Source, &n.SourceRef, &n.Status, &due, &meta, &created, &updated, &rank); err != nil {
			return nil, err
		}
		n.Tags = SplitTags(tags)
		if due.Valid && due.String != "" {
			t := parseTime(due.String)
			n.DueAt = &t
		}
		n.Meta = map[string]any{}
		_ = json.Unmarshal([]byte(meta), &n.Meta)
		n.CreatedAt, n.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, ScoredNode{Node: n, Score: -rank})
	}
	return out, rows.Err()
}

// CountByType returns node counts grouped by type.
func (db *DB) CountByType(ctx context.Context) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, COUNT(*) FROM nodes GROUP BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err != nil {
			return nil, err
		}
		out[t] = c
	}
	return out, rows.Err()
}

// OpenTasks returns open tasks ordered by due date.
func (db *DB) OpenTasks(ctx context.Context, limit int) ([]Node, error) {
	return db.ListNodes(ctx, NodeFilter{Types: []string{TypeTask}, Status: StatusOpen, Order: "due", Limit: limit})
}

// UpdatedBetween returns nodes of given types updated in [from, to).
func (db *DB) UpdatedBetween(ctx context.Context, types []string, status string, from, to time.Time, limit int) ([]Node, error) {
	f := NodeFilter{Types: types, Status: status}
	where, args := f.where("")
	args = append(args, fmtTime(from), fmtTime(to), limit)
	return db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE `+where+` AND updated_at >= ? AND updated_at < ? ORDER BY updated_at DESC LIMIT ?`, args...)
}

// AllTitles returns a lowercase title → id map (used for wiki-link checks).
func (db *DB) AllTitles(ctx context.Context) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, title FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id int64
		var t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[strings.ToLower(t)] = id
	}
	return out, rows.Err()
}

// IterateNodes streams every node to fn (used by exporters).
func (db *DB) IterateNodes(ctx context.Context, fn func(*Node) error) error {
	rows, err := db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return err
		}
		if err := fn(n); err != nil {
			return err
		}
	}
	return rows.Err()
}

// NodesWithMetaValue lists ids of nodes of type typ whose meta array key contains value (case-insensitive).
func (db *DB) NodesWithMetaValue(ctx context.Context, typ, key, value string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM nodes WHERE type = ?
		AND EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(nodes.meta) THEN nodes.meta ELSE '{}' END, '$.'||?) WHERE lower(json_each.value) = lower(?))`,
		typ, key, strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FindPersonByEmail finds the person node whose meta.emails lists email (case-insensitive).
func (db *DB) FindPersonByEmail(ctx context.Context, email string) (*Node, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, ErrNotFound
	}
	n, err := scanNode(db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE type = ?
		AND EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(nodes.meta) THEN nodes.meta ELSE '{}' END, '$.emails') WHERE lower(json_each.value) = ?)
		ORDER BY id LIMIT 1`, TypePerson, email))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// SourceTypeCounts returns node counts per source and type.
func (db *DB) SourceTypeCounts(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT source, type, COUNT(*) FROM nodes GROUP BY source, type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var src, typ string
		var n int
		if err := rows.Scan(&src, &typ, &n); err != nil {
			return nil, err
		}
		if out[src] == nil {
			out[src] = map[string]int{}
		}
		out[src][typ] = n
	}
	return out, rows.Err()
}

// TagTypeCounts returns node counts per tag and type.
func (db *DB) TagTypeCounts(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, tags FROM nodes WHERE tags <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var typ, tags string
		if err := rows.Scan(&typ, &tags); err != nil {
			return nil, err
		}
		for _, t := range SplitTags(tags) {
			if out[t] == nil {
				out[t] = map[string]int{}
			}
			out[t][typ]++
		}
	}
	return out, rows.Err()
}
