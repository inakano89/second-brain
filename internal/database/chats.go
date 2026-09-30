package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Chat is one web conversation (a tab). Its turns live in chat_messages under ChatChannel(ID).
type Chat struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"` // empty = automatic (persona name, then the first message)
	Persona      string    `json:"persona"`
	Instructions string    `json:"instructions,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ChatChannel is the chat_messages channel of a web chat.
func ChatChannel(id int64) string { return "web:" + strconv.FormatInt(id, 10) }

const chatCols = `id, title, persona, instructions, created_at, updated_at`

func scanChat(s scanner) (*Chat, error) {
	var c Chat
	var created, updated string
	if err := s.Scan(&c.ID, &c.Title, &c.Persona, &c.Instructions, &created, &updated); err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = parseTime(created), parseTime(updated)
	return &c, nil
}

// CreateChat opens a new chat tab.
func (db *DB) CreateChat(ctx context.Context, title, persona, instructions string) (*Chat, error) {
	ts := now()
	res, err := db.ExecContext(ctx, `INSERT INTO chats (title, persona, instructions, created_at, updated_at) VALUES (?,?,?,?,?)`,
		strings.TrimSpace(title), persona, strings.TrimSpace(instructions), ts, ts)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return db.GetChat(ctx, id)
}

// GetChat loads a chat or returns ErrNotFound.
func (db *DB) GetChat(ctx context.Context, id int64) (*Chat, error) {
	c, err := scanChat(db.QueryRowContext(ctx, `SELECT `+chatCols+` FROM chats WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListChats returns every chat in creation order (the order of the tabs).
func (db *DB) ListChats(ctx context.Context) ([]Chat, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+chatCols+` FROM chats ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// RenameChat sets the title; an empty title goes back to the automatic one.
func (db *DB) RenameChat(ctx context.Context, id int64, title string) error {
	res, err := db.ExecContext(ctx, `UPDATE chats SET title=? WHERE id=?`, strings.TrimSpace(title), id)
	return affected(res, err)
}

// AutoTitleChat names a chat that has no title yet (the first message wins) and reports
// whether it did.
func (db *DB) AutoTitleChat(ctx context.Context, id int64, title string) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE chats SET title=? WHERE id=? AND title=''`, strings.TrimSpace(title), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// TouchChat marks the chat as used now (the newest one opens first).
func (db *DB) TouchChat(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE chats SET updated_at=? WHERE id=?`, now(), id)
	return err
}

// DeleteChat removes a chat and its whole history.
func (db *DB) DeleteChat(ctx context.Context, id int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE channel=?`, ChatChannel(id)); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM chats WHERE id=?`, id)
	if err := affected(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
