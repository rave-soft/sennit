package db

import (
	"context"
	"database/sql"
)

// UpdateFileRead read-modify-writes a file's recorded read ranges under
// the session/path key.
//
// The read and the write have to be atomic with respect to each other, so
// when this Queries owns a *sql.DB it opens its own transaction. When it
// is bound to a *sql.Tx instead, there already is one: the caller's. That
// case used to be rejected with sql.ErrConnDone — "connection is already
// closed", which is neither true nor actionable — even though the ambient
// transaction gives exactly the atomicity this needs. It now runs the
// statements directly on the caller's handle and lets them commit it.
func (q *Queries) UpdateFileRead(ctx context.Context, sessionID, path string, update func(string) string) error {
	beginner, ok := q.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		// Already inside a transaction — see the doc comment.
		return updateFileReadIn(ctx, q.db, sessionID, path, update)
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if err := updateFileReadIn(ctx, tx, sessionID, path, update); err != nil {
		return err
	}
	return tx.Commit()
}

// updateFileReadIn performs the read-modify-write against whichever handle
// is providing atomicity — this Queries' own transaction, or the caller's.
func updateFileReadIn(ctx context.Context, db DBTX, sessionID, path string, update func(string) string) error {
	row := db.QueryRowContext(ctx, `SELECT read_ranges FROM read_files WHERE session_id = ? AND path = ?`, sessionID, path)
	var ranges string
	switch err := row.Scan(&ranges); err {
	case sql.ErrNoRows:
		ranges = ""
	case nil:
	default:
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO read_files (session_id, path, read_at, read_ranges) VALUES (?, ?, strftime('%s', 'now'), ?) ON CONFLICT(path, session_id) DO UPDATE SET read_at = excluded.read_at, read_ranges = excluded.read_ranges`, sessionID, path, update(ranges))
	return err
}
