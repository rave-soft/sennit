package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/pressly/goose/v3"
)

// Stamping a new database from a migrated template.
//
// Creating a database means running the whole migration chain: two dozen
// small DDL transactions. In production that happens once, on a first run,
// and costs about ten milliseconds — nobody has ever noticed it.
//
// A test binary is the opposite case. It stands up hundreds of throwaway
// databases, one per test, and under -race each chain costs ~210ms: the
// SQLite driver is a transpiled C VM in pure Go, so every one of its
// allocations goes through a race-instrumented global allocator lock. That
// single cost dominated the race suite — the agent package alone spent
// over a minute of wall clock on it, most of it queued rather than
// working.
//
// So: migrate once per process, snapshot the result, and stamp every
// later database from that snapshot. The snapshot is produced by SQLite
// itself (VACUUM INTO), so it is a real database file with the schema and
// goose's own version table already in it; the migration chain that runs
// afterwards finds nothing left to apply. The schema is identical by
// construction — it is the schema the chain produced.
//
// Off by default and never enabled in production: a test binary turns it
// on from TestMain with [UseMigratedTemplate].

var (
	templateMu sync.Mutex
	// templateEnabled gates the whole mechanism. Read under templateMu.
	templateEnabled bool
	// templatePath is the snapshot, produced on first use. Empty until
	// then, and left empty if the snapshot could not be made — in which
	// case every Connect simply migrates for itself, as it always did.
	templatePath string
	// templateFailed records that snapshotting was tried and did not
	// work, so it is not retried for every database afterwards.
	templateFailed bool
)

// UseMigratedTemplate turns template stamping on for this process. It is
// for test binaries only — call it from TestMain, before any Connect:
//
//	func TestMain(m *testing.M) {
//		db.UseMigratedTemplate()
//		os.Exit(m.Run())
//	}
//
// The stamped database is byte-for-byte a database this process migrated,
// so a test sees exactly the schema production sees. The one thing it does
// not exercise is the migration chain itself — the tests that are about
// migrations (internal/db's own) do not call this.
func UseMigratedTemplate() {
	templateMu.Lock()
	defer templateMu.Unlock()
	templateEnabled = true
}

// stampFromTemplate writes a migrated database to dbPath and reports
// whether it did. It returns false — leaving the caller to migrate
// normally — when stamping is off, when dbPath already exists, or when
// anything about the snapshot fails. Never returning an error is
// deliberate: this is an optimization, and there is no failure of it that
// should be able to fail a Connect.
func stampFromTemplate(ctx context.Context, dbPath string) bool {
	templateMu.Lock()
	defer templateMu.Unlock()
	if !templateEnabled || templateFailed {
		return false
	}
	if _, err := os.Stat(dbPath); err == nil {
		// An existing database is not ours to overwrite.
		return false
	}
	if templatePath == "" {
		path, err := buildTemplate(ctx)
		if err != nil {
			templateFailed = true
			return false
		}
		templatePath = path
	}
	contents, err := os.ReadFile(templatePath)
	if err != nil {
		return false
	}
	return os.WriteFile(dbPath, contents, 0o600) == nil
}

// buildTemplate migrates one database for real and snapshots it. Caller
// holds templateMu, so this happens exactly once per process.
func buildTemplate(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "sennit-db-template-*")
	if err != nil {
		return "", err
	}
	seed := filepath.Join(dir, "seed.db")
	conn, err := openDB(seed)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	if err := conn.PingContext(ctx); err != nil {
		return "", err
	}
	if err := migrate(conn); err != nil {
		return "", err
	}
	// VACUUM INTO writes a self-contained copy with no WAL alongside it,
	// which is what makes the result safe to hand to a fresh open. A
	// plain file copy of a live database would need its WAL copied too,
	// in the right order, and would still be a snapshot of whatever the
	// checkpoint state happened to be.
	template := filepath.Join(dir, "template.db")
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", template); err != nil {
		return "", fmt.Errorf("snapshot the migrated database: %w", err)
	}
	return template, nil
}

// migrate applies the migration chain to conn. Shared by openAndMigrate
// and by the template seed above so both get the same schema by
// construction, not by two copies of the same three lines.
func migrate(conn *sql.DB) error {
	if err := initGoose(); err != nil {
		return fmt.Errorf("failed to initialize goose: %w", err)
	}
	if err := goose.Up(conn, "migrations"); err != nil {
		return fmt.Errorf("failed to apply migrations: %w", err)
	}
	return nil
}
