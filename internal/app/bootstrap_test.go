package app

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitpkg "github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/workspacelock"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// setBootstrapTestEnv isolates config/skills discovery from the running
// user's real home and XDG directories, so tests can exercise the
// underlying config.Init/db.Connect path without touching the machine's
// real config.
func setBootstrapTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// TestBootstrap_Success covers the happy path: config, data directory,
// DB, skills, and App all come back wired together, and the PostDataDir
// / PostConnect hooks fire in order with the config being built.
func TestWorkspaceLockHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_WORKSPACE_LOCK_HELPER") != "1" {
		return
	}
	lock, err := workspacelock.Acquire(os.Args[len(os.Args)-1])
	if err != nil {
		fmt.Fprintln(os.Stdout, err)
		os.Exit(1)
	}
	defer lock.Release()
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestBootstrap_Success(t *testing.T) {
	setBootstrapTestEnv(t)

	cwd := t.TempDir()
	dataDir := t.TempDir()

	var order []string
	result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
		DataDir: dataDir,
		Debug:   true,
		YOLO:    true,
		PostDataDir: func(cfg *config.ConfigStore) error {
			order = append(order, "post-data-dir")
			require.Equal(t, dataDir, cfg.Config().Options.DataDirectory)
			return nil
		},
		PostConnect: func(cfg *config.ConfigStore) error {
			order = append(order, "post-connect")
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.NotNil(t, result.App)
	require.NotNil(t, result.Config)
	require.NotNil(t, result.Skills)
	require.Equal(t, []string{"post-data-dir", "post-connect"}, order)
	require.True(t, result.Config.Overrides().SkipPermissionRequests)
}

func TestBootstrap_ProjectRuntimeActivationRequiresTrust(t *testing.T) {
	setBootstrapTestEnv(t)
	cwd := t.TempDir()
	// A redirect rather than touch(1), and json.Marshal rather than a
	// string literal: hooks run through Sennit's own embedded POSIX shell,
	// which has no touch on a Windows runner, and a raw Windows path
	// pasted into JSON is not valid JSON. The path is single-quoted so its
	// backslashes survive the shell's word expansion too.
	sideEffect := filepath.Join(t.TempDir(), "hook-ran")
	projectConfig, err := json.Marshal(map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []map[string]any{{"command": "echo ran > '" + sideEffect + "'"}},
		},
		"mcp": map[string]any{"project": map[string]any{"type": "stdio", "command": "false"}},
		"lsp": map[string]any{"project": map[string]any{"command": "false"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "sennit.json"), projectConfig, 0o600))

	untrusted, err := Bootstrap(context.Background(), cwd, BootstrapOptions{DataDir: t.TempDir()})
	require.NoError(t, err)
	untrusted.App.Shutdown()
	require.Empty(t, untrusted.Config.Config().Hooks)
	require.NotContains(t, untrusted.Config.Config().MCP, "project")
	require.NotContains(t, untrusted.Config.Config().LSP, "project")
	runner := hooks.NewRunner(untrusted.Config.Config().Hooks[hooks.EventPreToolUse], cwd, cwd)
	_, err = runner.Run(t.Context(), hooks.EventPreToolUse, "session", "read", `{}`)
	require.NoError(t, err)
	_, err = os.Stat(sideEffect)
	require.ErrorIs(t, err, os.ErrNotExist)

	trusted, err := Bootstrap(context.Background(), cwd, BootstrapOptions{DataDir: t.TempDir(), TrustProject: true})
	require.NoError(t, err)
	t.Cleanup(trusted.App.Shutdown)
	require.Contains(t, trusted.Config.Config().MCP, "project")
	require.Contains(t, trusted.Config.Config().LSP, "project")
	runner = hooks.NewRunner(trusted.Config.Config().Hooks[hooks.EventPreToolUse], cwd, cwd)
	_, err = runner.Run(t.Context(), hooks.EventPreToolUse, "session", "read", `{}`)
	require.NoError(t, err)
	require.FileExists(t, sideEffect)
}

func TestBootstrap_InheritsParentAgents(t *testing.T) {
	setBootstrapTestEnv(t)

	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir: t.TempDir(),
		InheritedAgents: map[string]config.Agent{
			"reviewer": {
				ID:          "reviewer",
				Name:        "Reviewer",
				Description: "Reviews code.",
				Prompt:      "Review the implementation.",
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.Contains(t, result.Config.Config().Agents, "reviewer")
	require.Contains(t, result.Config.Config().Agents[config.AgentCoder].AllowedTools, "reviewer")
}

// TestBootstrap_PostDataDirError verifies a failing PostDataDir hook
// aborts the sequence before the DB connection is opened.
func TestBootstrap_PostDataDirError(t *testing.T) {
	setBootstrapTestEnv(t)

	wantErr := errors.New("boom")
	_, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir: t.TempDir(),
		PostDataDir: func(cfg *config.ConfigStore) error {
			return wantErr
		},
		PostConnect: func(cfg *config.ConfigStore) error {
			t.Fatal("PostConnect must not run when PostDataDir fails")
			return nil
		},
	})
	require.ErrorIs(t, err, wantErr)
}

// TestBootstrap_WorkspaceLockOptionApplies is a smoke test for the
// WorkspaceLock option: Bootstrap must acquire the workspace lock and
// still complete successfully, matching how both the top-level CLI
// (internal/cmd/root.go) and a spawned thread (LocalSpawner) call
// Bootstrap.
func TestBootstrap_WorkspaceLockOptionApplies(t *testing.T) {
	setBootstrapTestEnv(t)

	dataDir := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, dataDir) })
	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       dataDir,
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)
}

// TestBootstrap_WorkspaceLockReleasedOnShutdown confirms the workspace
// lock acquired by WorkspaceLock is released once the resulting App is
// shut down, so a second Bootstrap of the same data directory can
// proceed afterward.
func TestBootstrap_WorkspaceLockReleasedOnShutdown(t *testing.T) {
	setBootstrapTestEnv(t)

	dataDir := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, dataDir) })
	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       dataDir,
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	result.App.Shutdown()

	lock, err := workspacelock.Acquire(dataDir)
	require.NoError(t, err, "workspace lock should be released after Shutdown")
	lock.Release()
}

func TestWorkspaceLock_RepositoryIdentityAcrossProcesses(t *testing.T) {
	setBootstrapTestEnv(t)
	repo := initWorkspaceRepo(t)
	subdir := filepath.Join(repo, "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	worktree := filepath.Join(t.TempDir(), "worktree")
	runWorkspaceGit(t, repo, "worktree", "add", "-b", "thread", worktree, "main")

	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, repo) })
	result, err := Bootstrap(t.Context(), repo, BootstrapOptions{
		DataDir:       t.TempDir(),
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	defer result.App.Shutdown()

	for _, workspace := range []string{subdir, alias, worktree} {
		lockDir, err := workspaceLockDir(t.Context(), workspace, t.TempDir())
		require.NoError(t, err)
		requireWorkspaceLockContended(t, lockDir)
	}
}

func TestWorkspaceLock_DifferentRepositoriesDoNotConflict(t *testing.T) {
	repo := initWorkspaceRepo(t)
	otherRepo := initWorkspaceRepo(t)
	lockDir, err := workspaceLockDir(t.Context(), repo, t.TempDir())
	require.NoError(t, err)
	lock, err := workspacelock.Acquire(lockDir)
	require.NoError(t, err)
	defer lock.Release()

	otherLockDir, err := workspaceLockDir(t.Context(), otherRepo, t.TempDir())
	require.NoError(t, err)
	requireWorkspaceLockAcquired(t, otherLockDir)
}

func TestWorkspaceLock_NonGitUsesDataDirFallback(t *testing.T) {
	workspace := t.TempDir()
	dataDir := t.TempDir()
	lockDir, err := workspaceLockDir(t.Context(), workspace, dataDir)
	require.NoError(t, err)
	require.Equal(t, dataDir, lockDir)

	lock, err := workspacelock.Acquire(lockDir)
	require.NoError(t, err)
	defer lock.Release()
	requireWorkspaceLockContended(t, dataDir)
}

func TestWorkspaceLock_CanceledContextDoesNotFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dataDir := t.TempDir()

	lockDir, err := workspaceLockDir(ctx, t.TempDir(), dataDir)
	require.Error(t, err)
	require.Empty(t, lockDir)
	require.ErrorIs(t, err, context.Canceled)
}

func TestWorkspaceLock_GitCommandFailureDoesNotFallback(t *testing.T) {
	// Command lookup is the git package's command abstraction. Hiding git from
	// PATH makes CommonDir fail operationally, which must never select dataDir.
	t.Setenv("PATH", t.TempDir())
	dataDir := t.TempDir()

	lockDir, err := workspaceLockDir(t.Context(), t.TempDir(), dataDir)
	require.Error(t, err)
	require.Empty(t, lockDir)
	require.NotErrorIs(t, err, gitpkg.ErrNotRepository)
}

func TestBootstrap_ParentAndWorktreeShareLock(t *testing.T) {
	setBootstrapTestEnv(t)
	repo := initWorkspaceRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	runWorkspaceGit(t, repo, "worktree", "add", "-b", "thread", worktree, "main")
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, repo) })
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, worktree) })

	parent, err := Bootstrap(t.Context(), repo, BootstrapOptions{
		DataDir:       t.TempDir(),
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	defer parent.App.Shutdown()
	child, err := Bootstrap(t.Context(), worktree, BootstrapOptions{
		DataDir:       t.TempDir(),
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	child.App.Shutdown()

	commonDir, err := gitpkg.CommonDir(t.Context(), repo)
	require.NoError(t, err)
	requireWorkspaceLockContended(t, commonDir)
}

func TestWorkspaceLock_ReleaseIsRefcounted(t *testing.T) {
	repo := initWorkspaceRepo(t)
	commonDir, err := gitpkg.CommonDir(t.Context(), repo)
	require.NoError(t, err)
	parent, err := workspacelock.Acquire(commonDir)
	require.NoError(t, err)
	child, err := workspacelock.Acquire(commonDir)
	require.NoError(t, err)
	child.Release()
	requireWorkspaceLockContended(t, commonDir)
	parent.Release()
	requireWorkspaceLockAcquired(t, commonDir)
}

func TestBootstrap_PostConnectErrorReleasesPooledDB(t *testing.T) {
	setBootstrapTestEnv(t)
	_, err := Bootstrap(t.Context(), t.TempDir(), BootstrapOptions{
		DataDir: t.TempDir(),
		PostConnect: func(*config.ConfigStore) error {
			return errors.New("post-connect failed")
		},
	})
	require.Error(t, err)

	// A subsequent consumer receives a live pooled connection. Closing the
	// returned *sql.DB instead of releasing the Bootstrap reference would make
	// this connection unusable while the pool still holds it.
	conn, err := db.Connect(t.Context(), config.GlobalDBDir())
	require.NoError(t, err)
	require.NoError(t, conn.PingContext(t.Context()))
	require.NoError(t, db.Release(config.GlobalDBDir()))
}

func TestBootstrap_NewFailureReleasesPooledDB(t *testing.T) {
	setBootstrapTestEnv(t)
	wantErr := errors.New("new failed")
	_, err := Bootstrap(t.Context(), t.TempDir(), BootstrapOptions{
		DataDir: t.TempDir(),
		newApp: func(context.Context, *sql.DB, *config.ConfigStore, *skills.Manager) (*App, error) {
			return nil, wantErr
		},
	})
	require.ErrorIs(t, err, wantErr)

	conn, err := db.Connect(t.Context(), config.GlobalDBDir())
	require.NoError(t, err)
	require.NoError(t, conn.PingContext(t.Context()))
	require.NoError(t, db.Release(config.GlobalDBDir()))
}

// syncLogBuffer is a thread-safe io.Writer, since the concurrent Shutdown
// this test drives can log while the test goroutine reads the buffer back.
type syncLogBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestBootstrap_LateFinalCleanupRegistrationFailureShutsDownAppAndReleasesDBOnce
// is the regression test for Bootstrap leaking a started App when
// AddFinalCleanup fails after newApp has already succeeded (bootstrap.go's
// final cleanup registration, right before the dbConnected handoff). By
// that point MCP initialization, the config/skills watchers and the herdr
// bridge are all live goroutines, and the caller used to get only an error
// back with no *App to shut down itself.
//
// That registration can only fail once Shutdown has already begun on this
// App (see shutdownPhases.addHook), so the test drives exactly that: a
// concurrent Shutdown() is started from inside newApp and this goroutine
// waits for it to actually begin before returning, guaranteeing Bootstrap's
// own AddFinalCleanup call observes it and fails with
// ErrAppShutdownBlocked.
func TestBootstrap_LateFinalCleanupRegistrationFailureShutsDownAppAndReleasesDBOnce(t *testing.T) {
	setBootstrapTestEnv(t)

	var logBuf syncLogBuffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// Captured before Bootstrap starts anything, so the check below reports
	// only what this App's own construction left running.
	ignoreBaseline := goleak.IgnoreCurrent()

	var appInstance *App
	_, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir: t.TempDir(),
		newApp: func(ctx context.Context, conn *sql.DB, store *config.ConfigStore, mgr *skills.Manager) (*App, error) {
			a, err := New(ctx, conn, store, mgr)
			if err != nil {
				return nil, err
			}
			appInstance = a
			go a.Shutdown()
			for {
				a.shutdownMu.Lock()
				started := a.shutdownState >= shutdownStateShuttingDown
				a.shutdownMu.Unlock()
				if started {
					break
				}
				time.Sleep(time.Millisecond)
			}
			return a, nil
		},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAppShutdownBlocked)
	require.NotNil(t, appInstance)

	// By the time Bootstrap returns, its own call into Shutdown (in the
	// AddFinalCleanup failure branch) has either run the teardown itself
	// or joined the one already in flight above — either way, teardown is
	// complete and every goroutine New started (MCP init, watchers, herdr)
	// must be gone.
	deadline := time.Now().Add(2 * time.Second)
	var leakErr error
	for time.Now().Before(deadline) {
		leakErr = goleak.Find(ignoreBaseline)
		if leakErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, leakErr, "a late AddFinalCleanup failure must still shut the App down")

	// Snapshot the log before this test's own extra Release call below,
	// which is guaranteed to find nothing left and log an over-release
	// error of its own regardless of the bug — so it must not pollute the
	// assertion that Bootstrap's own defer did not double-release.
	logsFromBootstrap := logBuf.String()
	require.NotContains(t, logsFromBootstrap, "over-released",
		"the pooled DB reference must be released exactly once, not double-released")

	// The App released the pooled DB reference itself (via mainDBRelease):
	// a bare Release now, with no matching Connect from this test, must
	// find nothing left to release, proving it was not leaked either.
	require.Error(t, db.Release(config.GlobalDBDir()),
		"the App's own DB reference must already be fully released, not leaked, by the time Bootstrap returns")
}

func TestBootstrap_WorkspaceLockReleasedAfterPostConnectError(t *testing.T) {
	setBootstrapTestEnv(t)
	repo := initWorkspaceRepo(t)
	dataDir := t.TempDir()
	_, err := Bootstrap(t.Context(), repo, BootstrapOptions{
		DataDir:       dataDir,
		WorkspaceLock: true,
		PostConnect: func(*config.ConfigStore) error {
			return errors.New("post-connect failed")
		},
	})
	require.Error(t, err)
	lockDir, err := workspaceLockDir(t.Context(), repo, dataDir)
	require.NoError(t, err)
	requireWorkspaceLockAcquired(t, lockDir)
}

func initWorkspaceRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.com")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644))
	runWorkspaceGit(t, repo, "add", "README.md")
	runWorkspaceGit(t, repo, "commit", "-m", "initial")
	return repo
}

func runWorkspaceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, output)
}

func requireWorkspaceLockContended(t *testing.T, lockDir string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestWorkspaceLockHelperProcess", "--", lockDir)
	cmd.Env = append(os.Environ(), "SENNIT_WORKSPACE_LOCK_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, workspacelock.ErrLocked.Error())
	require.NoError(t, stdin.Close())
	require.Error(t, cmd.Wait())
}

func requireWorkspaceLockAcquired(t *testing.T, lockDir string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestWorkspaceLockHelperProcess", "--", lockDir)
	cmd.Env = append(os.Environ(), "SENNIT_WORKSPACE_LOCK_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", line)
	require.NoError(t, stdin.Close())
	require.NoError(t, cmd.Wait())
}

// TestBootstrap_TwoProjectsConcurrentWrites simulates the scenario the
// shared-database migration exists to support: two independent sennit
// "instances" (distinct working directories, distinct project-local
// .sennit data directories, each taking its own WorkspaceLock) bootstrapped
// in the same process against the SAME
// global database (HOME/XDG are shared across both goroutines here,
// unlike setBootstrapTestEnv's usual one-dir-per-test isolation). Both
// then write sessions concurrently. Nothing here should dead-lock or
// error: WAL + busy_timeout must serialize the shared connection's
// single underlying conn (SetMaxOpenConns(1)) without either workspace
// blocking the other's *process-level* startup, and each workspace's
// WorkspaceLock only guards its own project directory, not the shared
// database.
func TestBootstrap_TwoProjectsConcurrentWrites(t *testing.T) {
	setBootstrapTestEnv(t)

	bootOne := func(cwd, dataDir string) *App {
		result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
			DataDir:       dataDir,
			WorkspaceLock: true,
		})
		require.NoError(t, err)
		return result.App
	}

	dataDirA, dataDirB := t.TempDir(), t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, dataDirA) })
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, dataDirB) })
	appA := bootOne(t.TempDir(), dataDirA)
	t.Cleanup(appA.Shutdown)
	appB := bootOne(t.TempDir(), dataDirB)
	t.Cleanup(appB.Shutdown)

	const writesPerApp = 20
	var wg sync.WaitGroup
	errs := make(chan error, writesPerApp*2)
	write := func(a *App, label string) {
		defer wg.Done()
		for i := range writesPerApp {
			if _, err := a.Sessions().Create(context.Background(), fmt.Sprintf("%s-%d", label, i)); err != nil {
				errs <- err
			}
		}
	}

	wg.Add(2)
	go write(appA, "a")
	go write(appB, "b")
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	sessionsA, err := appA.Sessions().List(context.Background())
	require.NoError(t, err)
	require.Len(t, sessionsA, writesPerApp, "project A must see only its own sessions")

	sessionsB, err := appB.Sessions().List(context.Background())
	require.NoError(t, err)
	require.Len(t, sessionsB, writesPerApp, "project B must see only its own sessions")
}

// TestBootstrap_SkippedLockDisablesInterruptedTurnCleanup pins the
// precondition finalizeInterruptedTurns rests on. It stamps error tool
// results and a canceled finish onto every unfinished assistant message
// in the project, which is repair when the process that wrote them is
// gone and corruption when it is still streaming. Only a lock that
// actually excludes a second sennit tells the two apart, and
// SENNIT_SKIP_DATADIR_LOCK hands back one that excludes nothing.
func TestBootstrap_SkippedLockDisablesInterruptedTurnCleanup(t *testing.T) {
	setBootstrapTestEnv(t)
	t.Setenv("SENNIT_SKIP_DATADIR_LOCK", "1")

	dataDir := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, dataDir) })
	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       dataDir,
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.False(t, result.App.WorkspaceLockEnforced(),
		"a skipped lock excludes nobody, so the sweep must not treat unfinished as abandoned")
}

// TestBootstrap_EnforcedLockEnablesInterruptedTurnCleanup is the other
// half: the ordinary path must keep sweeping, or a crashed run leaves a
// session stuck behind a spinner that never stops.
func TestBootstrap_EnforcedLockEnablesInterruptedTurnCleanup(t *testing.T) {
	setBootstrapTestEnv(t)

	dataDir := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, dataDir) })
	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       dataDir,
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.True(t, result.App.WorkspaceLockEnforced())
}
