package app

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"
)

// TestMain points the global profile at a throwaway directory for the whole
// package. Tests here reach config.GlobalDBDir and friends; without this a
// test that forgets to isolate writes sessions into the developer's real
// profile, which is exactly what used to happen.
func TestMain(m *testing.M) {
	for _, key := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
		_ = os.Unsetenv(key)
	}
	cleanup := testenv.IsolateGlobalProfile()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
