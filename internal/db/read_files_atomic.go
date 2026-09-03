package db

import (
	"context"
	"database/sql"
	"errors"
)

// UpdateFileRead read-modify-writes a file's recorded read ranges under
// the session/path key. update receives the row's current read_ranges
// value and whether a row exists at all — a file with no row yet has no
// recorded coverage, which is not the same thing as the empty string a
// fully-read file's row holds.
//
// The read and the write have to be atomic with respect to each other, so
// when this Queries owns a *sql.DB it opens its own transaction. When it
// is bound to a *sql.Tx instead, there already is one: the caller's. That
// case used to be rejected with sql.ErrConnDone — "connection is already
// closed", which is neither true nor actionable — even though the ambient
// transaction gives exactly the atomicity this needs. It now runs the
// statements directly on the caller's handle and lets them commit it.
func (q *Queries) UpdateFileRead(ctx context.Context, sessionID, path string, update func(ranges string, exists bool) string) error {
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
func updateFileReadIn(ctx context.Context, db DBTX, sessionID, path string, update func(ranges string, exists bool) string) error {
	q := New(db)
	var ranges string
	var exists bool
	file, err := q.GetFileRead(ctx, GetFileReadParams{SessionID: sessionID, Path: path})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No row: leave ranges/exists at their zero values.
	case err != nil:
		return err
	default:
		ranges = file.ReadRanges
		exists = true
	}
	return q.RecordFileRead(ctx, RecordFileReadParams{
		SessionID:  sessionID,
		Path:       path,
		ReadRanges: update(ranges, exists),
	})
}
