package db

import (
	"context"
	"database/sql"
	"fmt"
)

// InTx runs fn against a *Queries bound to a fresh transaction on conn,
// committing when fn returns nil and rolling back otherwise (including
// when fn panics, via conn.BeginTx's usual defer-Rollback pattern). It
// factors out the begin/WithTx/commit-or-rollback boilerplate that gc,
// sessionstore.Service.Delete and history.Service repeat by hand.
func InTx(ctx context.Context, conn *sql.DB, fn func(*Queries) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := fn(New(tx)); err != nil {
		return err
	}
	return tx.Commit()
}
