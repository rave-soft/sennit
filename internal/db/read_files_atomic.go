package db

import (
	"context"
	"database/sql"
)

func (q *Queries) UpdateFileRead(ctx context.Context, sessionID, path string, update func(string) string) error {
	beginner, ok := q.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return sql.ErrConnDone
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	row := tx.QueryRowContext(ctx, `SELECT read_ranges FROM read_files WHERE session_id = ? AND path = ?`, sessionID, path)
	var ranges string
	switch err := row.Scan(&ranges); err {
	case sql.ErrNoRows:
		ranges = ""
	case nil:
	default:
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO read_files (session_id, path, read_at, read_ranges) VALUES (?, ?, strftime('%s', 'now'), ?) ON CONFLICT(path, session_id) DO UPDATE SET read_at = excluded.read_at, read_ranges = excluded.read_ranges`, sessionID, path, update(ranges))
	if err != nil {
		return err
	}
	return tx.Commit()
}
