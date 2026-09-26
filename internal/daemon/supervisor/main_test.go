package supervisor_test

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"
)

// TestMain isolates this package's tests from the developer's real
// profile and terminal, same as internal/daemon's own TestMain: these
// tests bootstrap full daemon.Run instances (with a database) in
// subprocesses, and re-exec this binary as a daemon helper -- which
// would otherwise sleep a second on exit under -race (see
// testenv.TrimChildRaceExitSleep's doc comment).
//
// The HERDR_* unset below is skipped when this process IS a daemon
// helper (SENNIT_SUPERVISOR_DAEMON_HELPER=1): TestEnsureRunning_StripsHerdrEnv
// sets those vars in the OUTER test process and relies on the helper
// subprocess's own os.Environ() reflecting exactly what
// supervisor.spawnDetached actually inherited and passed through --
// unsetting them here, before TestSupervisorDaemonHelperProcess's env
// dump ever runs, would scrub them regardless of whether production
// code did its job, making that test pass unconditionally either way.
// Every other test in this package (and the outer, non-helper half of
// that one) still gets the safety-net unset.
func TestMain(m *testing.M) {
	if os.Getenv("SENNIT_SUPERVISOR_DAEMON_HELPER") != "1" {
		for _, key := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
			_ = os.Unsetenv(key)
		}
	}
	testenv.TrimChildRaceExitSleep()
	cleanup := testenv.IsolateGlobalProfile()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
