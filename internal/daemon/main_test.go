package daemon_test

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"
)

// TestMain isolates the whole package from the developer's real profile and
// terminal, as internal/app's does. The daemon bootstraps a full App with a
// database, so a test that forgot writeGlobalConfig would otherwise write
// sessions into the real global profile. The herdr variables are cleared so
// no daemon started here reports into the terminal pane running the tests.
func TestMain(m *testing.M) {
	for _, key := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
		_ = os.Unsetenv(key)
	}
	// The lock and concurrency tests re-exec this binary as helper
	// processes; under -race each helper would otherwise sleep a second on
	// exit. See testenv.TrimChildRaceExitSleep.
	testenv.TrimChildRaceExitSleep()
	cleanup := testenv.IsolateGlobalProfile()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
