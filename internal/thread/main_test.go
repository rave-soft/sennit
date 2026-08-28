package thread_test

import (
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"go.uber.org/goleak"
)

// TestMain wires goleak's process-wide leak check across this package's
// whole test binary (both this file's package thread_test and
// store_test.go's package thread share one binary, so this covers both).
//
// Manager owns background goroutines (auto-merge, delivery, worktree
// removal) that only stop once Manager.Shutdown has been called and has
// returned - the exact class of bug that let a test's temp directory get
// removed out from under a still-writing goroutine (see
// shutdownManagerOnCleanup in fakes_test.go). goleak catches that class
// directly: any test that builds a Manager and forgets to shut it down
// leaves its goroutines running past the test's return, and goleak reports
// them here instead of the failure surfacing later, non-deterministically,
// as a flaky TempDir cleanup on someone else's CI run.
func TestMain(m *testing.M) {
	// Stamp this package's throwaway databases from one migrated
	// template rather than running the migration chain per test; see
	// db.UseMigratedTemplate.
	db.UseMigratedTemplate()
	goleak.VerifyTestMain(m)
}
