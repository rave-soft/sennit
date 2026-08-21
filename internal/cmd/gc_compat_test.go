package cmd

// This file adapts internal/gc's exported Selection/Collect/Delete/
// DeleteWith to the private gcSelection/gcCollect/gcDelete/gcDeleteWith
// names gc_test.go was written against, back when that logic lived in
// this package. It exists purely so those tests -- the safety net for
// the CLI half of `sennit gc` -- keep exercising the real selection and
// deletion behavior (now in internal/gc) without being rewritten:
// production code (gc.go) calls internal/gc directly and never needs
// these names.

import (
	"context"
	"database/sql"

	sennitdb "github.com/rave-soft/sennit/internal/db"
	sennitgc "github.com/rave-soft/sennit/internal/gc"
)

type gcSelection struct {
	sessionIDs        []string
	threadIDs         []string
	messagesDeleted   int64
	filesDeleted      int64
	readFilesDeleted  int64
	orphanedWorktrees []string
}

func fromGCSelection(s sennitgc.Selection) gcSelection {
	return gcSelection{
		sessionIDs:        s.SessionIDs,
		threadIDs:         s.ThreadIDs,
		messagesDeleted:   s.MessagesDeleted,
		filesDeleted:      s.FilesDeleted,
		readFilesDeleted:  s.ReadFilesDeleted,
		orphanedWorktrees: s.OrphanedWorktrees,
	}
}

// gcRowQuerier is the slice of database/sql shared by *sql.DB and *sql.Tx
// gc_test.go passes as gcCollect's third argument. internal/gc.Collect no
// longer needs a raw query handle -- its counting goes through generated
// *db.Queries methods -- so this exists only to preserve that call's
// signature.
type gcRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

//nolint:unparam // projectPath is forwarded to internal/gc.Collect; every existing gc_test.go call happens to pass "", but the parameter is real, not dead
func gcCollect(ctx context.Context, q *sennitdb.Queries, _ gcRowQuerier, cutoff int64, projectPath string) (gcSelection, error) {
	selection, err := sennitgc.Collect(ctx, q, cutoff, projectPath)
	if err != nil {
		return gcSelection{}, err
	}
	return fromGCSelection(selection), nil
}

type gcDeleteFunc func(context.Context, *sennitdb.Queries, gcSelection) error

//nolint:unparam // projectPath is forwarded to internal/gc.Delete; every existing gc_test.go call happens to pass "", but the parameter is real, not dead
func gcDelete(ctx context.Context, conn *sql.DB, q *sennitdb.Queries, cutoff int64, projectPath string) (gcSelection, error) {
	selection, err := sennitgc.Delete(ctx, conn, q, cutoff, projectPath)
	if err != nil {
		return gcSelection{}, err
	}
	return fromGCSelection(selection), nil
}

func gcDeleteWith(ctx context.Context, conn *sql.DB, q *sennitdb.Queries, cutoff int64, projectPath string, deleteFunc gcDeleteFunc) (gcSelection, error) {
	selection, err := sennitgc.DeleteWith(ctx, conn, q, cutoff, projectPath, func(ctx context.Context, q *sennitdb.Queries, s sennitgc.Selection) error {
		return deleteFunc(ctx, q, fromGCSelection(s))
	})
	if err != nil {
		return gcSelection{}, err
	}
	return fromGCSelection(selection), nil
}
