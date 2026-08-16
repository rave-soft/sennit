package app

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	gitpkg "github.com/rave-soft/sennit/internal/git"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// setBootstrapTestEnv isolates config/skills discovery from the running
// user's real home and XDG directories, matching the pattern backend
// tests use for exercising the same underlying config.Init/db.Connect path.
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
	if os.Getenv("SENNIT_TEST_WORKSPACE_LOCK_HELPER") != "1" {
		return
	}
	lock, err := db.AcquireWorkspaceLock(os.Args[len(os.Args)-1])
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

// TestBootstrap_GlobalSkillsMirror confirms the GlobalSkillsMirror option
// is actually threaded into the skills manager: with it set, discovery
// updates the package-level globals; without it, they are left alone.
func TestBootstrap_GlobalSkillsMirror(t *testing.T) {
	setBootstrapTestEnv(t)

	cwd := t.TempDir()

	// Poison the global cache first so a genuine mirror write is
	// distinguishable from state left over by other tests.
	skills.SetLatestStates(nil)

	result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
		DataDir:            t.TempDir(),
		GlobalSkillsMirror: true,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.NotNil(t, skills.GetLatestStates(),
		"global mirror should have been written to when GlobalSkillsMirror is set")
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
// still complete successfully, matching how the backend calls
// Bootstrap.
func TestBootstrap_WorkspaceLockOptionApplies(t *testing.T) {
	setBootstrapTestEnv(t)

	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       t.TempDir(),
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
	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       dataDir,
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	result.App.Shutdown()

	lock, err := db.AcquireWorkspaceLock(dataDir)
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
	lock, err := db.AcquireWorkspaceLock(lockDir)
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

	lock, err := db.AcquireWorkspaceLock(lockDir)
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
	parent, err := db.AcquireWorkspaceLock(commonDir)
	require.NoError(t, err)
	child, err := db.AcquireWorkspaceLock(commonDir)
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
	cmd.Env = append(os.Environ(), "SENNIT_TEST_WORKSPACE_LOCK_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, db.ErrWorkspaceLocked.Error())
	require.NoError(t, stdin.Close())
	require.Error(t, cmd.Wait())
}

func requireWorkspaceLockAcquired(t *testing.T, lockDir string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestWorkspaceLockHelperProcess", "--", lockDir)
	cmd.Env = append(os.Environ(), "SENNIT_TEST_WORKSPACE_LOCK_HELPER=1")
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
// shared-database migration exists to support: two independent braid
// "instances" (distinct working directories, distinct project-local
// .braid data directories, each with its own WorkspaceLock like the
// backend takes) bootstrapped in the same process against the SAME
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

	bootOne := func(cwd string) *App {
		result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
			DataDir:       t.TempDir(),
			WorkspaceLock: true,
		})
		require.NoError(t, err)
		return result.App
	}

	appA := bootOne(t.TempDir())
	t.Cleanup(appA.Shutdown)
	appB := bootOne(t.TempDir())
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
