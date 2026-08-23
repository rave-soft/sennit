package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWorktreeRoot_PicksUpGitInitMidSession guards against caching a
// negative worktreeRoot lookup. A plain directory can become a git worktree
// mid-session (e.g. `git init` run from the agent's bash tool), and a
// process-lifetime cache keyed only on "have we seen this dir" would leave
// it permanently outside its own worktree boundary once the first (negative)
// lookup was cached.
func TestWorktreeRoot_PicksUpGitInitMidSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()

	if root := worktreeRoot(dir); root != "" {
		t.Fatalf("worktreeRoot(%q) = %q before git init, want \"\"", dir, root)
	}

	cmd := exec.CommandContext(t.Context(), "git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	root := worktreeRoot(dir)
	if root == "" {
		t.Fatalf("worktreeRoot(%q) = \"\" after git init, want a resolved root", dir)
	}
}

// TestWorktreeRoot_NonGitDirSkipsGitSubprocess pins the cheap path: a
// directory with no .git anywhere up the chain must resolve via findGitEntry
// alone and never shell out to git. worktreeRoot's negative case is
// deliberately uncached (see TestWorktreeRoot_PicksUpGitInitMidSession), and
// it sits behind lookupConfigs on watch.go's 2s external-change poll, so an
// uncached case that still spawns a git subprocess would be a regression in
// its own right.
//
// To observe "git was never invoked" without instrumenting worktreeRoot
// itself, PATH is pointed at a directory containing nothing but a fake git
// script that records its own invocation to a log file. If findGitEntry's
// stat walk didn't short-circuit before reaching computeWorktreeRoot, the
// fake script would run and leave a trace; asserting the log stays absent
// across repeated calls (simulating the poll) is direct evidence the real
// exec.Command("git", ...) path was never taken.
func TestWorktreeRoot_NonGitDirSkipsGitSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git is a POSIX shell script")
	}

	dir := t.TempDir()

	logPath := filepath.Join(t.TempDir(), "git-invocations.log")
	binDir := t.TempDir()
	script := "#!/bin/sh\necho invoked >> " + logPath + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PATH", binDir)

	// Repeat to simulate the watch.go poll calling this on every tick.
	for i := range 5 {
		if root := worktreeRoot(dir); root != "" {
			t.Fatalf("worktreeRoot(%q) call %d = %q, want \"\"", dir, i, root)
		}
	}

	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("fake git was invoked for a non-git directory (stat err=%v); "+
			"findGitEntry should have short-circuited before computeWorktreeRoot", err)
	}
}
