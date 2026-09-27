package cmd

import (
	"io"
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
	// This package re-execs its own test binary as a stand-in daemon
	// (or lock/log helper) process, several times over -- helperCommand,
	// loggingHelperCommand, startLockHelper, and daemonRestartCmd's own
	// production spawn path (os.Executable(), exercised directly by
	// TestDaemonRestartCmd) all do it. Under -race, every one of those
	// children pays a full second of atexit sleep on its way out (see
	// testenv.TrimChildRaceExitSleep's doc comment); left unset, that
	// cost compounds across the helpers a single test run touches and
	// was eating into supervisor.EnsureRunning's readiness budget.
	testenv.TrimChildRaceExitSleep()

	// setupLocalWorkspace's PostConnect and initConfig both call
	// setupProcessLogging on the paths this package's tests exercise
	// in-process (never in a re-exec'd subprocess, which gets its own
	// process and its own singleton) -- see processLogSetup's own doc
	// comment in root.go for why the real sennitlog.Setup must never run
	// here.
	processLogSetup = func(string, bool, ...io.Writer) {}

	// Stamp this package's throwaway databases from one migrated
	// template rather than running the migration chain per test; see
	// db.UseMigratedTemplate.
	db.UseMigratedTemplate()

	// SENNIT_CMD_REUSE_GLOBAL_PROFILE opts a re-exec'd subprocess helper
	// out of a second, independent IsolateGlobalProfile call: this same
	// test binary re-execs itself with -test.run targeting a single
	// helper test (daemon_commands_test.go's TestCmdDaemonLoggingHelperProcess),
	// and TestMain runs there too. Without this, that subprocess's own
	// IsolateGlobalProfile call would stamp a second, different
	// throwaway profile directory over the SENNIT_GLOBAL_CONFIG/DATA the
	// parent test process set (and propagated via os.Environ()) -- fine
	// for every other daemon helper here, which never needs the two
	// processes to agree on where the global profile lives, but wrong
	// for a test asserting on a file (the daemon's own log) whose path
	// is derived from that directory in both processes.
	cleanup := func() {}
	if os.Getenv("SENNIT_CMD_REUSE_GLOBAL_PROFILE") != "1" {
		cleanup = testenv.IsolateGlobalProfile()
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
