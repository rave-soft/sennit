package testenv

import (
	"os"
	"strings"
)

// TrimChildRaceExitSleep removes the one-second pause that a
// race-instrumented process takes on its way out, for every child process
// this one spawns.
//
// The race runtime's atexit_sleep_ms defaults to 1000: a binary built with
// -race sleeps a full second at exit so that races reported by goroutines
// still running can surface before the process is gone. Tests that spawn
// helper processes re-exec the test binary itself, so every one of those
// helpers is race-instrumented too and pays the second — invisibly, since
// it is spent inside the parent's wait for the child to disconnect. In
// internal/lsp, where a single restart test starts and stops two servers,
// that pause was the entire cost of the package under -race.
//
// GORACE is parsed once at process start, so setting it here does not
// change how this process reports races; it only reaches children, which
// inherit the environment. Their exit-time reporting is what is traded
// away, and helper processes exist to be driven by the test, not to be
// raced against on their way out.
//
// Call it from TestMain before m.Run, in the parent branch of a self-exec
// helper (never in the child branch — by then the child's own race runtime
// has already read GORACE).
func TrimChildRaceExitSleep() {
	const option = "atexit_sleep_ms=0"
	current := os.Getenv("GORACE")
	if strings.Contains(current, "atexit_sleep_ms=") {
		return
	}
	if current == "" {
		_ = os.Setenv("GORACE", option)
		return
	}
	_ = os.Setenv("GORACE", current+" "+option)
}
