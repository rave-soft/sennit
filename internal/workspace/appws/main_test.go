package appws

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/testenv"
)

// TestMain points the global profile at a throwaway directory for the whole
// package. Tests here reach config.GlobalConfigData through the credentials
// singleton; without this they leave a lock file in the developer's real
// profile.
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
