package tools

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"
)

// TestMain trims the race runtime's exit sleep out of the helper processes
// this package spawns. The LSP end-to-end tests re-exec this very binary as
// a language server (see lspToolHelperProcess), so each of those helpers is
// race-instrumented and would pause a second on its way out, inside the
// client's wait for the server to disconnect. See
// testenv.TrimChildRaceExitSleep.
func TestMain(m *testing.M) {
	testenv.TrimChildRaceExitSleep()
	os.Exit(m.Run())
}
