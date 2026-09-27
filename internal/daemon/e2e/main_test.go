// Package e2e drives a real `sennit daemon run` subprocess -- the actual
// binary, a real unix socket, a real *grpcws.Client -- through the
// scenarios CLIENT-SERVER.md's PR 2.4 calls out: a turn surviving client
// detach, a permission or question request that has to wait with nobody
// connected, a killed daemon's next start finalizing the interrupted turn,
// worktree entry/exit through the daemon, and idle exit. Everything in
// this package is test-only; nothing here is imported by production code.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/testenv"
)

// sennitBinary is the path TestMain builds the real sennit binary to,
// once, for every test in this package to launch as a subprocess. Empty
// under -race (see racecheck_off_test.go): this package spawns a
// non-race-instrumented binary regardless of how its own test binary was
// built, so running it under the race job would burn the job's time
// budget without exercising the race detector on the code that matters --
// see racecheck_off_test.go's doc comment for the full reasoning.
var sennitBinary string

// buildTimeout bounds the one `go build` TestMain does for the whole
// package. A build that hangs (a wedged module proxy, a toolchain fetch)
// must fail loudly rather than hang every test in this package.
const buildTimeout = 3 * time.Minute

func TestMain(m *testing.M) {
	// os.Exit does not run deferred functions, so the actual work (and
	// everything that has to be cleaned up afterward -- the build dir,
	// the isolated global profile) lives in run, whose own return runs
	// its defers before TestMain ever calls os.Exit itself.
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if raceDetectorEnabled {
		// See sennitBinary's doc comment: skip the (expensive) binary
		// build entirely under -race, since every test in this package
		// checks raceDetectorEnabled first and skips.
		return m.Run()
	}

	restoreProfile := testenv.IsolateGlobalProfile()
	defer restoreProfile()
	testenv.TrimChildRaceExitSleep()
	for _, v := range []string{"HERDR_SOCKET", "HERDR_TOKEN", "HERDR_LOCAL_MODE"} {
		_ = os.Unsetenv(v)
	}

	dir, err := os.MkdirTemp("", "sennit-e2e-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: create build dir:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: find repo root:", err)
		return 1
	}

	sennitBinary = filepath.Join(dir, "sennit-e2e")
	if runtime.GOOS == "windows" {
		sennitBinary += ".exe"
	}

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	// CGO_ENABLED and GOEXPERIMENT mirror the repo's own build (Taskfile.yaml,
	// AGENTS.md's "CGO disabled" note) so this binary behaves the way a
	// released one would, not the way the *test* toolchain happens to
	// default.
	cmd := exec.CommandContext(ctx, "go", "build", "-o", sennitBinary, ".")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOEXPERIMENT=greenteagc")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build sennit binary:", err)
		fmt.Fprintln(os.Stderr, string(out))
		return 1
	}

	return m.Run()
}

// findRepoRoot walks up from this file's own directory to the module
// root (the directory containing go.mod), independent of the working
// directory `go test` happens to be invoked from.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
