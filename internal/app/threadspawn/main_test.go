package threadspawn

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/testenv"
)

// TestMain points the global profile at a throwaway directory for the whole
// package. Tests here reach config.GlobalDBDir and friends; without this a
// test that forgets to isolate writes sessions into the developer's real
// profile, which is exactly what used to happen.
func TestMain(m *testing.M) {
	// Stamp this package's throwaway databases from one migrated
	// template rather than running the migration chain per test; see
	// db.UseMigratedTemplate.
	db.UseMigratedTemplate()
	cleanup := testenv.IsolateGlobalProfile()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
