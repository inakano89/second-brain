package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ProfileRow is one stored profile item. Title and Data are opaque here: the profile
// package encrypts them when the item is sensitive.
type ProfileRow struct {
	ID        int64
	Kind      string
	Title     string
	Data      string
	Sensitive bool
	Archived  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProfileCheck records that a scheduled item (a dose, a habit, a class) was done.
type ProfileCheck struct {
	ItemID int64
	Day    string // YYYY-MM-DD, local time
	Slot   string // "08:00" or "" for items without a time
	At     time.Time
}

const profileCols = `id, kind, title, data, sensitive, archived, created_at, updated_at`

func scanProfile(s scanner) (ProfileRow, error) {
	var r ProfileRow
	var created, updated string
	if err := s.Scan(&r.ID, &r.Kind, &r.Title, &r.Data, &r.Sensitive, &r.Archived, &created, &updated); err != nil {
		return r, err
	}
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	return r, nil
}

// ListProfile returns every profile item, archived ones only when asked.
func (db *DB) ListProfile(ctx context.Context, archived bool) ([]ProfileRow, error) {
	q := `SELECT ` + profileCols + ` FROM profile_items`
	if !archived {
		q += ` WHERE archived = 0`
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY kind, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProfileRow
	for rows.Next() {
		r, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetProfile returns one item.
func (db *DB) GetProfile(ctx context.Context, id int64) (ProfileRow, error) {
	r, err := scanProfile(db.QueryRowContext(ctx, `SELECT `+profileCols+` FROM profile_items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// SaveProfile inserts (ID 0) or updates an item and returns its ID.
func (db *DB) SaveProfile(ctx context.Context, r ProfileRow) (int64, error) {
	ts := now()
	if r.ID == 0 {
		res, err := db.ExecContext(ctx, `INSERT INTO profile_items (kind, title, data, sensitive, archived, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
			r.Kind, r.Title, r.Data, r.Sensitive, r.Archived, ts, ts)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := db.ExecContext(ctx, `UPDATE profile_items SET kind=?, title=?, data=?, sensitive=?, archived=?, updated_at=? WHERE id=?`,
		r.Kind, r.Title, r.Data, r.Sensitive, r.Archived, ts, r.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return r.ID, nil
}

// DeleteProfile removes an item and its check-ins.
func (db *DB) DeleteProfile(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM profile_items WHERE id = ?`, id)
	return err
}

// CheckProfile records a check-in; it reports false when it already existed.
func (db *DB) CheckProfile(ctx context.Context, itemID int64, day, slot string) (bool, error) {
	res, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO profile_log (item_id, day, slot, at) VALUES (?,?,?,?)`, itemID, day, slot, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UncheckProfile removes a check-in; it reports false when there was none.
func (db *DB) UncheckProfile(ctx context.Context, itemID int64, day, slot string) (bool, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM profile_log WHERE item_id = ? AND day = ? AND slot = ?`, itemID, day, slot)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ProfileChecks returns the check-ins between two days (inclusive).
func (db *DB) ProfileChecks(ctx context.Context, fromDay, toDay string) ([]ProfileCheck, error) {
	rows, err := db.QueryContext(ctx, `SELECT item_id, day, slot, at FROM profile_log WHERE day >= ? AND day <= ? ORDER BY day, slot`, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProfileCheck
	for rows.Next() {
		var c ProfileCheck
		var at string
		if err := rows.Scan(&c.ItemID, &c.Day, &c.Slot, &at); err != nil {
			return nil, err
		}
		c.At = parseTime(at)
		out = append(out, c)
	}
	return out, rows.Err()
}
