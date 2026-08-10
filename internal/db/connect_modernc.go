//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64))

package db

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

func openDBReadOnly(dbPath string) (*sql.DB, error) {
	params := url.Values{}
	// Only set safe pragmas for read-only mode - most pragmas require write access.
	// busy_timeout still applies to readers: a live writer holding the WAL
	// briefly (e.g. mid-checkpoint) can otherwise return SQLITE_BUSY
	// immediately instead of letting the read retry.
	params.Set("_txlock", "immediate")
	params.Set("mode", "ro")
	params.Add("_pragma", "busy_timeout(5000)")

	dsn := fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return db, nil
}

func openDB(dbPath string) (*sql.DB, error) {
	// Set pragmas for better performance via _pragma query params.
	// Format: _pragma=name(value)
	params := url.Values{}
	for name, value := range pragmas {
		params.Add("_pragma", fmt.Sprintf("%s(%s)", name, value))
	}
	// Use BEGIN IMMEDIATE so writers acquire the reserved lock up front,
	// preventing deferred-to-writer upgrade deadlocks.
	params.Set("_txlock", "immediate")

	dsn := fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return db, nil
}
