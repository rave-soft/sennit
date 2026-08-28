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
}

// connEntry holds a shared database connection and its reference
// count, which lets the same process open the same data directory
// concurrently from multiple callers.
//
// An entry is published into the pool *before* its database is open, so
// that opening and migrating happens with poolMu released. ready is what
// makes that safe: it is closed once db and err are final, and every
// caller that finds an existing entry waits on it before reading either.
// The alternative — holding poolMu across the migration chain — makes
// every Connect in the process queue behind every other one, however
// unrelated their databases are. That is invisible in production, where a
// process connects once, and dominates a test binary that stands up
// hundreds of throwaway databases (a fresh one costs ~10ms of migrations,
// and ~210ms under -race, where SQLite is a race-instrumented pure-Go VM).
type connEntry struct {
	ready    chan struct{}
	db       *sql.DB
	err      error
	refCount int
	// discard records that the last reference was released while the
	// entry was still opening. Legitimate use cannot reach it — the
	// opener's own reference is not released until its Connect returns —
	// so it exists for the over-release case Release describes below: the
	// opener closes the database it just finished opening instead of
	// leaving a live connection nothing holds.
	discard bool
}

// wait blocks until the entry's database is final and reports it.
func (e *connEntry) wait() (*sql.DB, error) {
	<-e.ready
	return e.db, e.err
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
	if entry, ok := pool[absPath]; ok {
		entry.refCount++
		poolMu.Unlock()
		// Another caller is opening (or has opened) this database: wait
		// for its result rather than opening a second connection to the
		// same file, which is the invariant the pool exists to keep.
		conn, err := entry.wait()
		if err != nil {
			// The opener already dropped the failed entry from the pool;
			// give back the reference taken above so the ledger Release
			// audits stays honest.
			poolMu.Lock()
			entry.refCount--
			poolMu.Unlock()
			return nil, err
		}
		return conn, nil
	}
	entry := &connEntry{refCount: 1, ready: make(chan struct{})}
	pool[absPath] = entry
	poolMu.Unlock()

	conn, err := openAndMigrate(ctx, dataDir, dbPath)

	poolMu.Lock()
	entry.db, entry.err = conn, err
	discard := entry.discard
	if err != nil || discard {
		// A failed open is not a pool entry: the next Connect must get a
		// fresh attempt rather than the cached failure. Same for an entry
		// nobody is left holding.
		if cur, ok := pool[absPath]; ok && cur == entry {
			delete(pool, absPath)
		}
	}
	poolMu.Unlock()
	close(entry.ready)

	if err != nil {
		return nil, err
	}
	if discard {
		conn.Close()
		return nil, fmt.Errorf("db: the connection to %q was released while it was still opening", dataDir)
	}
	return conn, nil
}

// openAndMigrate opens the database at dbPath and brings its schema up to
// date. It runs with poolMu released (see connEntry), so two callers
// opening two different databases do their migrations concurrently.
func openAndMigrate(ctx context.Context, dataDir, dbPath string) (*sql.DB, error) {
	// Ensuring the data directory exists is required before SQLite can
	// create the database file inside it.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data directory %q: %w", dataDir, err)
	}

	// A test binary can ask for new databases to be stamped from a
	// template migrated once for the whole process (see template.go); in
	// production this is off and does nothing.
	stampFromTemplate(ctx, dbPath)

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

	if err := migrate(conn); err != nil {
		conn.Close()
		slog.Error("Failed to migrate the database", "error", err)
		return nil, err
	}

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

	// The entry is still opening (only reachable through the
	// over-release this function exists to report, since the opener's own
	// reference outlives its Connect): hand the close to the opener,
	// which is the only goroutine that will have a database to close.
	if !isReady(entry) {
		entry.discard = true
		if cur, ok := pool[absPath]; ok && cur == entry {
			delete(pool, absPath)
		}
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
		if isReady(entry) {
			if entry.db != nil {
				entry.db.Close()
			}
		} else {
			// Still opening: the opener closes it (see connEntry.discard).
			entry.discard = true
		}
		delete(pool, path)
	}
}

// isReady reports whether entry's open has finished. Callers hold poolMu,
// which is what makes reading entry.db/err after this safe.
func isReady(entry *connEntry) bool {
	select {
	case <-entry.ready:
		return true
	default:
		return false
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
