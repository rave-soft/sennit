package db

import (
	"context"
	"database/sql"
	"fmt"
)

// OpenDB opens a SQLite database at dbPath with braid's standard pragmas
// (WAL, busy_timeout, etc.) but without goose migrations or connection
// pooling/refcounting. Intended for small auxiliary stores (e.g. the
// global model-discovery cache) that manage their own schema via
// CREATE TABLE IF NOT EXISTS instead of goose.
func OpenDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	conn, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	return conn, nil
}
