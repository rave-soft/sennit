package workspace

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"
)

// TestMain points the global profile at a throwaway directory for the whole
// package. Tests here reach config.GlobalConfigData through the credentials
// singleton; without this they leave a lock file in the developer's real
// profile.
func TestMain(m *testing.M) {
	cleanup := testenv.IsolateGlobalProfile()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
