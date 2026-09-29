package database

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Meta keys that keep the original date apart from the import date.
const (
	// MetaDateUnknown marks nodes whose source carried no date: created_at then holds the
	// moment they entered the brain, which says nothing about the content.
	MetaDateUnknown = "date_unknown"
	// MetaImportAt is when an import brought the node in (RFC 3339, UTC).
	MetaImportAt = "import_at"
)

// ManualSources are the channels where the user sends content by hand.
var ManualSources = []string{"telegram", "voice", "web", "api", "clip", "watcher"}

// Origins of a node for the Conteúdo filter.
const (
	OriginImported = "imported" // brought in by an importer
	OriginMine     = "mine"     // sent by hand (Telegram, voice, web, API, clipper, inbox)
	OriginAuto     = "auto"     // integrations, routines and the AI
)

// DateUnknown reports whether the node has no reliable original date.
func (n *Node) DateUnknown() bool {
	v, _ := n.Meta[MetaDateUnknown].(bool)
	return v
}

// Imported reports whether the node came from an importer.
func (n *Node) Imported() bool { return strings.HasPrefix(n.Source, "import:") }

// ImportedAt returns when an import brought the node in.
func (n *Node) ImportedAt() (time.Time, bool) {
	s, _ := n.Meta[MetaImportAt].(string)
	if s == "" {
		return time.Time{}, false
	}
	t := parseTime(s)
	return t, !t.IsZero()
}

// EffectiveAt is the date the content belongs to: events happen on their start date, everything
// else was created when it was written.
func (n *Node) EffectiveAt() time.Time {
	if n.Type == TypeEvent && n.DueAt != nil && !n.DueAt.IsZero() {
		return *n.DueAt
	}
	return n.CreatedAt
}

// Freshness ranks nodes by how recent their content is; nodes with an unknown date come last.
func (n *Node) Freshness() time.Time {
	if n.DateUnknown() {
		return time.Time{}
	}
	return n.EffectiveAt()
}

// NewestFirst sorts nodes by content date (unknown dates last), keeping id order on ties.
func NewestFirst(nodes []Node) {
	slices.SortStableFunc(nodes, func(a, b Node) int {
		if c := b.Freshness().Compare(a.Freshness()); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})
}

// dateUnknownSQL is 1 for nodes flagged with MetaDateUnknown (meta is compact JSON written by
// this package, so a substring test is exact and much cheaper than parsing every row).
func dateUnknownSQL(col func(string) string) string {
	return "(instr(" + col("meta") + ", '\"" + MetaDateUnknown + "\":true') > 0)"
}

func plainCol(c string) string { return c }

func manualSourcesSQL(col func(string) string) string {
	return col("source") + " IN ('" + strings.Join(ManualSources, "','") + "')"
}

// OrderNewestFirst returns ids ordered so the node with the most recent content comes first.
func (db *DB) OrderNewestFirst(ctx context.Context, ids []int64) ([]int64, error) {
	nodes, err := db.GetNodes(ctx, ids)
	if err != nil {
		return nil, err
	}
	NewestFirst(nodes)
	out := make([]int64, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out, nil
}

// SetCreatedAt sets the original date of a node (and clears the "unknown date" flag).
func (db *DB) SetCreatedAt(ctx context.Context, id int64, t time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE nodes SET created_at = ?, meta = json_remove(CASE WHEN json_valid(meta) THEN meta ELSE '{}' END, '$.`+MetaDateUnknown+`') WHERE id = ?`, fmtTime(t), id)
	return err
}

// CountImported counts nodes an importer brought in during [from, to), whatever their own date.
func (db *DB) CountImported(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE source LIKE 'import:%' AND json_valid(meta)
		AND json_extract(meta, '$.`+MetaImportAt+`') >= ? AND json_extract(meta, '$.`+MetaImportAt+`') < ?`, fmtTime(from), fmtTime(to)).Scan(&n)
	return n, err
}

// OnThisDay returns nodes with a known date that fall on the same month and day as day, in each
// of the given years back. Events count on the day they happen, everything else on the day it was
// written.
func (db *DB) OnThisDay(ctx context.Context, day time.Time, yearsBack []int, types []string, loc *time.Location, perYear int) (map[int][]Node, error) {
	out := map[int][]Node{}
	day = day.In(loc)
	var plain []string
	events := false
	for _, t := range types {
		if t == TypeEvent {
			events = true
		} else {
			plain = append(plain, t)
		}
	}
	for _, y := range yearsBack {
		d := day.AddDate(-y, 0, 0)
		if d.Month() != day.Month() { // Feb 29 falling on Mar 1
			continue
		}
		start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
		to := start.AddDate(0, 0, 1)
		var nodes []Node
		if len(plain) > 0 {
			f := NodeFilter{Types: plain, From: &start, To: &to, KnownDate: true, Limit: perYear, Order: "oldest"}
			list, err := db.ListNodes(ctx, f)
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, list...)
		}
		if events {
			list, err := db.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes WHERE type = 'event' AND due_at >= ? AND due_at < ? ORDER BY due_at LIMIT ?`,
				fmtTime(start), fmtTime(to), perYear)
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, list...)
		}
		if len(nodes) > 0 {
			out[y] = nodes
		}
	}
	return out, nil
}

// PersonCount is how often a person shows up.
type PersonCount struct {
	ID    int64
	Name  string
	Count int
}

// TopMentionedPeople ranks the people mentioned by nodes written in [from, to).
func (db *DB) TopMentionedPeople(ctx context.Context, from, to time.Time, limit int) ([]PersonCount, error) {
	rows, err := db.QueryContext(ctx, `SELECT p.id, p.title, COUNT(*) AS c FROM edges e
		JOIN nodes n ON n.id = e.source_id JOIN nodes p ON p.id = e.target_id
		WHERE e.relation = 'mentions' AND p.type = 'person' AND n.created_at >= ? AND n.created_at < ? AND NOT `+dateUnknownSQL(func(c string) string { return "n." + c })+`
		GROUP BY p.id ORDER BY c DESC, p.title LIMIT ?`, fmtTime(from), fmtTime(to), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonCount
	for rows.Next() {
		var p PersonCount
		if err := rows.Scan(&p.ID, &p.Name, &p.Count); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LastInteraction returns the date of the most recent dated node (other than the person itself)
// linked to a person, with how many such nodes exist. Nodes dated after now (upcoming events)
// are ignored.
func (db *DB) LastInteraction(ctx context.Context, personID int64, now time.Time) (last time.Time, count int, err error) {
	var s *string
	err = db.QueryRowContext(ctx, `SELECT MAX(d), COUNT(*) FROM (
			SELECT CASE WHEN n.type = 'event' AND n.due_at IS NOT NULL THEN n.due_at ELSE n.created_at END AS d
			FROM edges e JOIN nodes n ON n.id = CASE WHEN e.source_id = ? THEN e.target_id ELSE e.source_id END
			WHERE (e.source_id = ? OR e.target_id = ?) AND n.type <> 'person' AND n.source NOT LIKE 'import:%' AND n.source NOT IN ('routine', 'memory', 'rss', 'newsletter')
				AND NOT `+dateUnknownSQL(func(c string) string { return "n." + c })+`)
		WHERE d <= ?`, personID, personID, personID, fmtTime(now)).Scan(&s, &count)
	if err != nil || s == nil {
		return time.Time{}, count, err
	}
	return parseTime(*s), count, nil
}
