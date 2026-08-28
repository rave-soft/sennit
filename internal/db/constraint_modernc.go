//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64))

package db

import (
	"errors"

	"modernc.org/sqlite"
)

// SQLite extended result codes for the two constraint kinds we need to
// tell apart. modernc.org/sqlite does not export named constants for
// them (unlike github.com/ncruces/go-sqlite3's CONSTRAINT_UNIQUE and
// CONSTRAINT_FOREIGNKEY), so these are the numeric values from
// https://sqlite.org/rescode.html. modernc's driver always turns on
// extended result codes when it opens a connection (see
// conn.extendedResultCodes(true) in its newConn), so *sqlite.Error's
// Code() reliably returns the extended code here, not the base
// SQLITE_CONSTRAINT (19) that both violations would otherwise share.
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintForeignKey = 787
)

// IsUniqueConstraintError reports whether err is a UNIQUE constraint
// violation, and IsForeignKeyConstraintError reports the same for a
// FOREIGN KEY violation. Callers that only know "some write failed" and
// need to tell a constraint violation apart from any other error should
// use these instead of matching on err.Error(): the message text belongs
// to the driver, not to us, and this codebase builds against two
// swappable SQLite drivers (see connect_ncruces.go and
// connect_modernc.go) whose messages differ. Each driver gets its own
// implementation, gated on the same build tags as its connect_*.go file.
func IsUniqueConstraintError(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}

// IsForeignKeyConstraintError reports whether err is a FOREIGN KEY
// constraint violation. See IsUniqueConstraintError for why this check
// exists and why it is implemented once per driver.
func IsForeignKeyConstraintError(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintForeignKey
}
