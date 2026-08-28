//go:build !((darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64)))

package db

import (
	"errors"

	"github.com/ncruces/go-sqlite3"
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
	return errors.Is(err, sqlite3.CONSTRAINT_UNIQUE)
}

// IsForeignKeyConstraintError reports whether err is a FOREIGN KEY
// constraint violation. See IsUniqueConstraintError for why this check
// exists and why it is implemented once per driver.
func IsForeignKeyConstraintError(err error) bool {
	return errors.Is(err, sqlite3.CONSTRAINT_FOREIGNKEY)
}
