package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestProjectSkillsDir_WorkingDirLast pins ProjectSkillsDir's order: the git
// worktree root's skills directory must come before the working directory's,
// so the working directory — the last entry — wins a same-named conflict
// under skills.Deduplicate's last-occurrence rule (see DiscoverWithStates
// and Deduplicate in internal/skills). workingDir is nested under a
// subdirectory name that sorts before the repo root's own name, so a
// lexicographic sort would put it first and get this backwards.
func TestProjectSkillsDir_WorkingDirLast(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	workingDir := filepath.Join(root, "aaa-subdir")
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	dirs := ProjectSkillsDir(workingDir)

	// The git-root entry comes back as git reports the root, which is the
	// canonical spelling: /private/var/... rather than /var/... on macOS,
	// and the long form rather than an 8.3 short name on Windows. Match on
	// the subdirectory component instead of comparing whole paths, so the
	// assertion is about order rather than about how each side spells the
	// same directory.
	subdirSuffix := filepath.Join("aaa-subdir", ".sennit", "skills")

	gitRootIdx, workingDirIdx := -1, -1
	for i, d := range dirs {
		if strings.HasSuffix(d, subdirSuffix) {
			workingDirIdx = i
		} else if strings.HasSuffix(d, filepath.Join(".sennit", "skills")) {
			gitRootIdx = i
		}
	}
	if gitRootIdx == -1 || workingDirIdx == -1 {
		t.Fatalf("ProjectSkillsDir(%q) = %v, want a git-root entry and a working-directory entry", workingDir, dirs)
	}
	if workingDirIdx < gitRootIdx {
		t.Fatalf("ProjectSkillsDir(%q) = %v, want the working-directory entry after the git-root one", workingDir, dirs)
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

// TestGlobalAccountsFile_SitsBesideGlobalConfig pins that the account store
// always lives in the same directory as sennit.json — including when
// SENNIT_GLOBAL_CONFIG relocates that directory — so a workspace never goes
// looking for accounts.json somewhere the rest of the global state isn't.
func TestGlobalAccountsFile_SitsBesideGlobalConfig(t *testing.T) {
	path := GlobalAccountsFile()
	if filepath.Base(path) != "accounts.json" {
		t.Fatalf("GlobalAccountsFile() = %q, want a file named accounts.json", path)
	}
	if filepath.Dir(path) != filepath.Dir(GlobalConfig()) {
		t.Fatalf("GlobalAccountsFile() = %q, want it beside GlobalConfig() = %q", path, GlobalConfig())
	}

	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	if filepath.Dir(GlobalAccountsFile()) != filepath.Dir(GlobalConfig()) {
		t.Fatalf("GlobalAccountsFile() did not follow SENNIT_GLOBAL_CONFIG to %q", dir)
	}
}

// TestGlobalLogFile_IsPerProcess pins the property the split exists for:
// two sennits running at once must not share a log file. They regularly
// do run at once — a sennit works in one session, and a person works on
// more than one thing — and when ten of them wrote to one file, three
// unrelated top-level sessions appeared to be dispatching delegations
// simultaneously, which is indistinguishable on the page from the wake
// bug that had just been fixed.
func TestGlobalLogFile_IsPerProcess(t *testing.T) {
	t.Setenv("SENNIT_GLOBAL_CONFIG", t.TempDir())

	path := GlobalLogFile()
	if filepath.Dir(path) != GlobalLogDir() {
		t.Fatalf("GlobalLogFile() = %q, want it inside %q", path, GlobalLogDir())
	}
	if want := fmt.Sprintf("sennit-%d.log", os.Getpid()); filepath.Base(path) != want {
		t.Fatalf("GlobalLogFile() = %q, want basename %q", path, want)
	}
}

// TestLatestGlobalLogFile_PicksTheRunningSennitNotThisProcess covers what
// `sennit logs` needs. It runs in a process of its own every time, so its
// own GlobalLogFile names a file nothing ever wrote to; what the person
// asking meant is the sennit that is running, in the next terminal.
//
// The panic dump is the trap: RecoverPanic writes one into this same
// directory, and it is by definition the newest file the moment it lands.
// Without the exclusion, asking for the running sennit's log would answer
// with a single stack trace.
func TestLatestGlobalLogFile_PicksTheRunningSennitNotThisProcess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	logs := GlobalLogDir()
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(name string, age time.Duration) string {
		path := filepath.Join(logs, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("sennit-1.log", time.Hour)
	running := write("sennit-2.log", time.Minute)
	write("sennit-panic-ui-20260831-160000.log", 0)

	if got := LatestGlobalLogFile(); got != running {
		t.Fatalf("LatestGlobalLogFile() = %q, want %q", got, running)
	}
}

// TestLatestGlobalLogFile_FallsBackToThisProcess keeps a caller's "no
// logs yet" message able to name a plausible file on a fresh install.
func TestLatestGlobalLogFile_FallsBackToThisProcess(t *testing.T) {
	t.Setenv("SENNIT_GLOBAL_CONFIG", t.TempDir())

	if got, want := LatestGlobalLogFile(), GlobalLogFile(); got != want {
		t.Fatalf("LatestGlobalLogFile() = %q, want %q", got, want)
	}
}
