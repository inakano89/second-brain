package database

import (
	"context"
	"strings"
	"time"
)

// Edge is a directed relation between two nodes.
type Edge struct {
	ID        int64     `json:"id"`
	SourceID  int64     `json:"source"`
	TargetID  int64     `json:"target"`
	Relation  string    `json:"relation"`
	Weight    float64   `json:"weight"`
	CreatedAt time.Time `json:"created_at"`
}

// Link is a neighbour as seen from one node.
type Link struct {
	Node     Node    `json:"node"`
	Relation string  `json:"relation"`
	Weight   float64 `json:"weight"`
	Outgoing bool    `json:"outgoing"`
}

// GraphNode is a light node representation for the visualiser.
type GraphNode struct {
	ID     int64    `json:"id"`
	Title  string   `json:"title"`
	Type   string   `json:"type"`
	Tags   []string `json:"tags"`
	Status string   `json:"status,omitempty"`
	Degree int      `json:"degree"`
}

// GraphEdge is a light edge representation for the visualiser.
type GraphEdge struct {
	S   int64   `json:"s"`
	T   int64   `json:"t"`
	Rel string  `json:"rel"`
	W   float64 `json:"w"`
}

// Graph is a subgraph payload.
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// AddEdge inserts or strengthens an edge (keeps max weight).
func (db *DB) AddEdge(ctx context.Context, src, dst int64, relation string, weight float64) error {
	if src == dst || src == 0 || dst == 0 {
		return nil
	}
	if relation == "" {
		relation = "related"
	}
	_, err := db.ExecContext(ctx, `INSERT INTO edges (source_id, target_id, relation, weight, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(source_id, target_id, relation) DO UPDATE SET weight = MAX(weight, excluded.weight)`,
		src, dst, relation, weight, now())
	return err
}

// DeleteEdge removes an edge by id.
func (db *DB) DeleteEdge(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM edges WHERE id=?`, id)
	return err
}

// DeleteEdgesBetween removes the edges linking a and b in either direction (only those with
// relation when it is not empty) and returns how many were removed.
func (db *DB) DeleteEdgesBetween(ctx context.Context, a, b int64, relation string) (int64, error) {
	q := `DELETE FROM edges WHERE ((source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))`
	args := []any{a, b, b, a}
	if relation != "" {
		q += ` AND relation = ?`
		args = append(args, relation)
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Neighbors returns every node linked to id.
func (db *DB) Neighbors(ctx context.Context, id int64) ([]Link, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.target_id, e.relation, e.weight, 1 FROM edges e WHERE e.source_id = ?
		UNION ALL
		SELECT e.source_id, e.relation, e.weight, 0 FROM edges e WHERE e.target_id = ?`, id, id)
	if err != nil {
		return nil, err
	}
	type raw struct {
		id  int64
		rel string
		w   float64
		out bool
	}
	var rs []raw
	var ids []int64
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.id, &r.rel, &r.w, &r.out); err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, r)
		ids = append(ids, r.id)
	}
	rows.Close()
	nodes, err := db.GetNodes(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := map[int64]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	out := make([]Link, 0, len(rs))
	for _, r := range rs {
		if n, ok := byID[r.id]; ok {
			out = append(out, Link{Node: n, Relation: r.rel, Weight: r.w, Outgoing: r.out})
		}
	}
	return out, nil
}

// GraphFilter selects a subgraph.
type GraphFilter struct {
	NodeFilter
	IDs   []int64 // explicit seed ids (search results / focus)
	Hops  int     // expand seeds by N hops (0 or 1)
	Limit int
}

// Subgraph returns nodes (by filter or seeds) and edges among them.
func (db *DB) Subgraph(ctx context.Context, f GraphFilter) (*Graph, error) {
	limit := f.Limit
	if limit <= 0 || limit > 3000 {
		limit = 500
	}
	var nodes []Node
	var err error
	if len(f.IDs) > 0 {
		ids := f.IDs
		if f.Hops > 0 {
			ids, err = db.expand(ctx, ids, limit)
			if err != nil {
				return nil, err
			}
		}
		nodes, err = db.GetNodes(ctx, ids)
	} else {
		nf := f.NodeFilter
		nf.Limit = limit
		nf.Order = "" // by the date of the content, not by the last edit: an import's enrichment touches everything
		nodes, err = db.ListNodes(ctx, nf)
	}
	if err != nil {
		return nil, err
	}
	g := &Graph{Nodes: make([]GraphNode, 0, len(nodes)), Edges: []GraphEdge{}}
	if len(nodes) == 0 {
		return g, nil
	}
	idx := map[int64]int{}
	args := make([]any, 0, len(nodes))
	for i, n := range nodes {
		idx[n.ID] = i
		g.Nodes = append(g.Nodes, GraphNode{ID: n.ID, Title: n.Title, Type: n.Type, Tags: n.Tags, Status: n.Status})
		args = append(args, n.ID)
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := db.QueryContext(ctx, `SELECT source_id, target_id, relation, weight FROM edges WHERE source_id IN (`+ph+`) AND target_id IN (`+ph+`)`, append(args, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.S, &e.T, &e.Rel, &e.W); err != nil {
			return nil, err
		}
		g.Edges = append(g.Edges, e)
		g.Nodes[idx[e.S]].Degree++
		g.Nodes[idx[e.T]].Degree++
	}
	return g, rows.Err()
}

func (db *DB) expand(ctx context.Context, seeds []int64, limit int) ([]int64, error) {
	set := map[int64]bool{}
	out := make([]int64, 0, len(seeds))
	for _, id := range seeds {
		if !set[id] {
			set[id] = true
			out = append(out, id)
		}
	}
	args := make([]any, len(seeds))
	for i, id := range seeds {
		args[i] = id
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(seeds)), ",")
	rows, err := db.QueryContext(ctx, `SELECT target_id FROM edges WHERE source_id IN (`+ph+`)
		UNION SELECT source_id FROM edges WHERE target_id IN (`+ph+`) LIMIT ?`, append(append(args, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !set[id] && len(out) < limit {
			set[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// OrphanNodes returns nodes without any edge.
func (db *DB) OrphanNodes(ctx context.Context, excludeTypes []string, limit int) ([]Node, error) {
	q := `SELECT ` + nodeCols + ` FROM nodes n WHERE NOT EXISTS (SELECT 1 FROM edges e WHERE e.source_id = n.id OR e.target_id = n.id)`
	var args []any
	if len(excludeTypes) > 0 {
		q += ` AND n.type NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(excludeTypes)), ",") + `)`
		for _, t := range excludeTypes {
			args = append(args, t)
		}
	}
	q += ` ORDER BY n.created_at DESC LIMIT ?`
	args = append(args, limit)
	return db.queryNodes(ctx, q, args...)
}

// EdgeCount returns the total number of edges.
func (db *DB) EdgeCount(ctx context.Context) (int, error) {
	var c int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges`).Scan(&c)
	return c, err
}

// RelatedByTags returns recent nodes sharing at least minShared tags with the given tags.
func (db *DB) RelatedByTags(ctx context.Context, exclude int64, tags []string, minShared, limit int) ([]Node, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	var parts []string
	var args []any
	for _, t := range tags {
		parts = append(parts, `(CASE WHEN (','||tags||',') LIKE ? THEN 1 ELSE 0 END)`)
		args = append(args, "%,"+t+",%")
	}
	q := `SELECT ` + nodeCols + ` FROM (SELECT *, (` + strings.Join(parts, "+") + `) AS shared FROM nodes WHERE id <> ?) WHERE shared >= ? ORDER BY shared DESC, created_at DESC LIMIT ?`
	args = append(args, exclude, minShared, limit)
	return db.queryNodes(ctx, q, args...)
}
