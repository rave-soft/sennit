package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/fsext"
)

var (
	pragmas = map[string]string{
		"foreign_keys":  "ON",
		"journal_mode":  "WAL",
		"page_size":     "4096",
		"temp_store":    "MEMORY",
		"cache_size":    "-8000",
		"synchronous":   "NORMAL",
		"secure_delete": "ON",
		"busy_timeout":  "30000",
	}
	gooseInitOnce sync.Once
	gooseInitErr  error
)

//go:embed migrations/*.sql
var FS embed.FS

func init() {
	goose.SetBaseFS(FS)

	if testing.Testing() {
		goose.SetLogger(goose.NopLogger())
	}
}

// connEntry holds a shared database connection and its reference
// count, which lets the same process open the same data directory
// concurrently from multiple callers.
type connEntry struct {
	db       *sql.DB
	refCount int
}

var (
	pool   = make(map[string]*connEntry)
	poolMu sync.Mutex
)

// Connect opens a SQLite database connection for the given data
// directory and runs migrations. If a connection to the same database
// file already exists, the existing connection is returned with its
// reference count incremented. Callers must pair each Connect with a
// [Release] when they no longer need the connection.
func Connect(ctx context.Context, dataDir string) (*sql.DB, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data.dir is not set")
	}

	dbPath := filepath.Join(dataDir, brand.DBFile)

	// Canonicalize dataDir itself, not the joined dbPath: dataDir usually
	// already exists (its caller has typically created it), but sennit.db
	// inside it may not yet, on the very first Connect. Canonicalizing
	// the file path would then fall back to a merely-cleaned spelling for
	// that first call while later calls (once the file exists) resolve
	// the real one, splitting one directory across two pool keys.
	// Canonicalizing the existing directory instead keeps the key stable
	// across a relative path, a trailing separator, or (on Windows) an
	// 8.3 short name vs. the long form, regardless of call order.
	absPath := filepath.Join(fsext.Canonical(dataDir), brand.DBFile)

	poolMu.Lock()
	defer poolMu.Unlock()

	if entry, ok := pool[absPath]; ok {
		entry.refCount++
		return entry.db, nil
	}

	// Ensuring the data directory exists is required before SQLite can
	// create the database file inside it.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data directory %q: %w", dataDir, err)
	}

	conn, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	// Serialize all access through a single connection. SQLite
	// serializes writes at the file level anyway, and allowing multiple
	// pool connections to interleave writes/checkpoints (especially
	// under concurrent sub-agents) has caused WAL/header desync
	// resulting in SQLITE_NOTADB (26) on the next open.
	//
	// One database is now shared by every project a process serves, so
	// this one connection serializes all of them rather than one. What
	// that costs is measured, not assumed — see the benchmarks in
	// connect_bench_test.go, which produced the following on a 32-thread
	// desktop with every writer going flat out (no think time at all,
	// roughly 13k writes/sec through the connection):
	//
	//	projects   write p50   write p95   read p50   read p95
	//	       1       0.06ms      0.10ms     0.015ms    0.02ms
	//	       4       0.24ms      0.69ms     0.23ms     0.85ms
	//	      16       0.88ms      3.70ms     0.87ms     3.72ms
	//
	// Two things to read off it. Per-write cost stays flat (65us at one
	// project, 77us at sixteen): the queue serializes cleanly, it does
	// not collapse, and total throughput is the same however many
	// projects are sharing it. And the latency that does grow grows
	// linearly with the number of writers, with no cliff — it is
	// queueing, nothing more.
	//
	// Reads are the side that pays: WAL would let a reader run
	// alongside a writer, and one pool connection will not. That is the
	// real cost of this line. It only bites at a duty cycle no real
	// workload reaches, though: a session or message write per model
	// turn is single-digit writes per second per project, so sixteen
	// busy projects keep this connection occupied on the order of 1% of
	// the time and a read finds it free. Re-measure if that ever stops
	// being true.
	conn.SetMaxOpenConns(1)

	if err = conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := initGoose(); err != nil {
		conn.Close()
		slog.Error("Failed to initialize goose", "error", err)
		return nil, fmt.Errorf("failed to initialize goose: %w", err)
	}

	if err := goose.Up(conn, "migrations"); err != nil {
		conn.Close()
		slog.Error("Failed to apply migrations", "error", err)
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	pool[absPath] = &connEntry{db: conn, refCount: 1}
	return conn, nil
}

// Release decrements the reference count for the database at the given
// data directory. When the count reaches zero the underlying connection
// is closed and removed from the pool.
//
// A call with nothing left to release — no entry for dataDir at all, or
// one whose refCount has already been drained to zero by earlier calls —
// is misuse: every caller is expected to pair its own Connect with exactly
// one Release (see Connect's doc comment), so there is no legitimate
// reason for a Release to arrive once the ledger already says zero. The
// pool cannot always catch an over-release before it does damage — two
// distinct holders each releasing their own Connect look identical to one
// holder releasing twice, and either transition legitimately drains the
// count to zero (see TECHDEBT.md) — but it can refuse to pretend a call
// past that point was fine instead of silently doing nothing, which is
// what used to happen here and is exactly how an over-release surfaced far
// from its cause: as unrelated query errors in whatever component still
// held the handle, with nothing pointing back at the extra Release that
// caused it. Returning a reported error here — logged at Error level, not
// merely handed back for a caller to notice or not — turns that into a bug
// visible at its own call site instead of one that silently corrupts the
// pool.
func Release(dataDir string) error {
	// Must canonicalize the same way Connect does, or a Release spelled
	// differently than its matching Connect would miss the pool entry.
	absPath := filepath.Join(fsext.Canonical(dataDir), brand.DBFile)

	poolMu.Lock()
	defer poolMu.Unlock()

	entry, ok := pool[absPath]
	if !ok || entry.refCount <= 0 {
		err := fmt.Errorf("db: Release(%q) called with nothing left to release (over-release, or never Connect-ed)", dataDir)
		slog.Error("Database connection over-released", "dataDir", dataDir, "error", err)
		return err
	}

	entry.refCount--
	if entry.refCount > 0 {
		return nil
	}

	delete(pool, absPath)
	return entry.db.Close()
}

// ResetPool closes all pooled connections and clears the pool. This is
// intended for use in tests to ensure a clean state between test cases.
func ResetPool() {
	poolMu.Lock()
	defer poolMu.Unlock()
	for path, entry := range pool {
		entry.db.Close()
		delete(pool, path)
	}
}

func initGoose() error {
	// FS is already set by the package init() above; only the dialect needs
	// to happen here, once, before goose is used.
	gooseInitOnce.Do(func() {
		gooseInitErr = goose.SetDialect("sqlite3")
	})

	return gooseInitErr
}
