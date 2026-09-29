package database

import (
	"context"
	"strings"
)

// Transaction is one bank statement line. Amount is negative for money going out.
type Transaction struct {
	ID          int64   `json:"id"`
	Date        string  `json:"date"` // YYYY-MM-DD
	Amount      float64 `json:"amount"`
	Description string  `json:"description"`
	Merchant    string  `json:"merchant"` // normalised description, used to group recurring charges
	Category    string  `json:"category"`
	Account     string  `json:"account"`
	Ref         string  `json:"ref"` // id inside the statement (FITID or a hash of the line)
	Batch       string  `json:"batch"`
}

const txCols = `id, date, amount, description, merchant, category, account, ref, batch`

func scanTx(s scanner) (Transaction, error) {
	var t Transaction
	err := s.Scan(&t.ID, &t.Date, &t.Amount, &t.Description, &t.Merchant, &t.Category, &t.Account, &t.Ref, &t.Batch)
	return t, err
}

// InsertTransactions stores statement lines, skipping the ones already imported (same account
// and ref). It returns how many were new.
func (db *DB) InsertTransactions(ctx context.Context, txs []Transaction) (added int, err error) {
	if len(txs) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO transactions (date, amount, description, merchant, category, account, ref, batch, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	created := now()
	for _, t := range txs {
		res, err := stmt.ExecContext(ctx, t.Date, t.Amount, t.Description, t.Merchant, t.Category, t.Account, t.Ref, t.Batch, created)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// Transactions lists lines with date in [from, to] (either may be empty), newest first.
func (db *DB) Transactions(ctx context.Context, from, to string, limit int) ([]Transaction, error) {
	var conds []string
	var args []any
	if from != "" {
		conds, args = append(conds, "date >= ?"), append(args, from)
	}
	if to != "" {
		conds, args = append(conds, "date <= ?"), append(args, to)
	}
	q := `SELECT ` + txCols + ` FROM transactions`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY date DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		t, err := scanTx(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransactionMonths lists the months (YYYY-MM) that have lines, newest first.
func (db *DB) TransactionMonths(ctx context.Context) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT substr(date, 1, 7) m FROM transactions ORDER BY m DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountTransactions counts every stored line.
func (db *DB) CountTransactions(ctx context.Context) (n int, err error) {
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transactions`).Scan(&n)
	return
}

// SetTransactionCategory changes one line's category.
func (db *DB) SetTransactionCategory(ctx context.Context, id int64, category string) error {
	_, err := db.ExecContext(ctx, `UPDATE transactions SET category = ? WHERE id = ?`, category, id)
	return err
}

// DeleteTransactions removes every stored line (or only the ones of a batch when batch is set).
func (db *DB) DeleteTransactions(ctx context.Context, batch string) (int64, error) {
	q, args := `DELETE FROM transactions`, []any{}
	if batch != "" {
		q, args = q+` WHERE batch = ?`, append(args, batch)
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
