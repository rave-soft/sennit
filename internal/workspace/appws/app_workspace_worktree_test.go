package appws

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, output)
}

func initWorktreeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README"), []byte("root\n"), 0o644))
	runGit(t, repo, "add", "README")
	runGit(t, repo, "commit", "-m", "initial")
	return repo
}

func TestValidateWorktreeTargetRejectsInvalidNamesWithoutSideEffects(t *testing.T) {
	repo := initWorktreeRepo(t)
	base := filepath.Join(t.TempDir(), "worktrees")
	for _, name := range []string{"", ".", "..", "/absolute", `with/slash`, `with\\slash`, "../escape", "UPPER"} {
		_, err := validateWorktreeTarget(t.Context(), repo, base, name)
		require.ErrorIsf(t, err, ErrWorktreeUnavailable, "name %q", name)
	}
	_, err := os.Stat(base)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestValidateWorktreeTargetRejectsOrdinaryAndForeignDirectoriesAndReusesOwn(t *testing.T) {
	repo := initWorktreeRepo(t)
	base := filepath.Join(t.TempDir(), "worktrees")
	require.NoError(t, os.MkdirAll(filepath.Join(base, "ordinary"), 0o755))
	_, err := validateWorktreeTarget(t.Context(), repo, base, "ordinary")
	require.ErrorIs(t, err, ErrWorktreeUnavailable)

	foreign := initWorktreeRepo(t)
	require.NoError(t, os.Rename(foreign, filepath.Join(base, "foreign")))
	_, err = validateWorktreeTarget(t.Context(), repo, base, "foreign")
	require.ErrorIs(t, err, ErrWorktreeUnavailable)

	own, err := validateWorktreeTarget(t.Context(), repo, base, "owned")
	require.NoError(t, err)
	reused, err := validateWorktreeTarget(t.Context(), repo, base, "owned")
	require.NoError(t, err)
	require.Equal(t, own, reused)
}

func TestEnterWorktreeBootstrapFailureRetainsWorktreeAndStableSource(t *testing.T) {
	repo := initWorktreeRepo(t)
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	sessions := sessionstore.NewService(q, conn, repo)
	sess, err := sessions.Create(t.Context(), "top-level")
	require.NoError(t, err)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	store := configtest.NewStore(t, &config.Config{Options: &config.Options{DataDirectory: dataDir}}, configtest.WithWorkingDir(repo))
	a.SetConfigForTest(store)
	a.SetSessionsForTest(sessions)
	a.SetMessagesForTest(messagestore.NewService(q, messagestore.WithDebounce(0)))
	a.SetOwnershipForTest(sessionstore.NewOwnershipStore(conn), "main")
	a.ReportCurrentSession(sess.ID)
	w := NewAppWorkspace(a, store)

	originalBootstrap := bootstrapWorktreeApp
	bootstrapWorktreeApp = func(context.Context, string, app.BootstrapOptions) (*app.BootstrapResult, error) {
		return nil, errors.New("bootstrap failed")
	}
	t.Cleanup(func() { bootstrapWorktreeApp = originalBootstrap })
	_, _, err = w.EnterWorktree(t.Context(), "retained")
	require.ErrorContains(t, err, "bootstrap failed")
	_, statErr := os.Stat(filepath.Join(dataDir, "worktrees", "retained"))
	require.NoError(t, statErr)
	ownership, getErr := a.OwnershipStore().Get(t.Context(), sess.ID)
	require.NoError(t, getErr)
	require.Equal(t, "main", ownership.OwnerID)
	require.Equal(t, "stable", ownership.Phase)
	require.Equal(t, sess.ID, a.CurrentSessionID())
}

func TestEnterExitWorktreeTransfersSameSessionAndPreservesTranscript(t *testing.T) {
	repo := initWorktreeRepo(t)
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	sessions := sessionstore.NewService(q, conn, repo)
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	sess, err := sessions.Create(t.Context(), "top-level")
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "before"}}})
	require.NoError(t, err)

	source := app.NewForTest(t.Context())
	t.Cleanup(source.ShutdownForTest)
	sourceStore := configtest.NewStore(t, &config.Config{Options: &config.Options{DataDirectory: dataDir}}, configtest.WithWorkingDir(repo))
	source.SetConfigForTest(sourceStore)
	source.SetSessionsForTest(sessions)
	source.SetMessagesForTest(messages)
	source.SetOwnershipForTest(sessionstore.NewOwnershipStore(conn), "main")
	source.ReportCurrentSession(sess.ID)
	root := NewAppWorkspace(source, sourceStore)

	originalBootstrap := bootstrapWorktreeApp
	var targetApp *app.App
	bootstrapWorktreeApp = func(_ context.Context, path string, opts app.BootstrapOptions) (*app.BootstrapResult, error) {
		require.Equal(t, sess.ID, opts.ExistingSessionID)
		targetApp = app.NewForTest(t.Context())
		targetStore := configtest.NewStore(t, &config.Config{Options: &config.Options{DataDirectory: dataDir}}, configtest.WithWorkingDir(path))
		targetApp.SetConfigForTest(targetStore)
		targetApp.SetSessionsForTest(sessions)
		targetApp.SetMessagesForTest(messages)
		targetApp.SetOwnershipForTest(sessionstore.NewOwnershipStore(conn), "prepared")
		targetApp.ReportCurrentSession(sess.ID)
		targetApp.ArmPreparedSession()
		return &app.BootstrapResult{App: targetApp, Config: targetStore}, nil
	}
	t.Cleanup(func() { bootstrapWorktreeApp = originalBootstrap })

	targetWorkspace, releaseTarget, err := root.EnterWorktree(t.Context(), "transfer")
	require.NoError(t, err)
	target := targetWorkspace.(*AppWorkspace)
	require.Equal(t, sess.ID, target.app.CurrentSessionID())
	_, err = target.app.Messages().Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "inside"}}})
	require.NoError(t, err)
	rootWorkspace, shutdownTarget, err := target.ExitWorktree(t.Context())
	require.NoError(t, err)
	require.Same(t, root, rootWorkspace)
	require.Equal(t, sess.ID, root.app.CurrentSessionID())
	shutdownTarget()
	releaseTarget()

	history, err := messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, "before", history[0].Content().String())
	require.Equal(t, "inside", history[1].Content().String())
	listed, err := sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, sess.ID, listed[0].ID)
	require.Empty(t, listed[0].ParentSessionID)
	ownership, err := source.OwnershipStore().Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "main", ownership.OwnerID)
	require.Equal(t, "stable", ownership.Phase)
	require.Equal(t, "transfer", ownership.WorktreeName)
	require.NotEmpty(t, ownership.WorktreePath)

	targetWorkspace, releaseAgain, err := root.EnterWorktree(t.Context(), "transfer")
	require.NoError(t, err)
	require.Equal(t, ownership.WorktreePath, targetWorkspace.(interface {
		WorktreeState() workspace.WorktreeState
	}).WorktreeState().Path)
	_, shutdownAgain, err := targetWorkspace.(*AppWorkspace).ExitWorktree(t.Context())
	require.NoError(t, err)
	shutdownAgain()
	releaseAgain()
}

func TestWorktreeReleaseCallbacksOnlyShutdownStaleTarget(t *testing.T) {
	repo := initWorktreeRepo(t)
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	sessions := sessionstore.NewService(q, conn, repo)
	sess, err := sessions.Create(t.Context(), "top-level")
	require.NoError(t, err)
	source := app.NewForTest(t.Context())
	t.Cleanup(source.ShutdownForTest)
	store := configtest.NewStore(t, &config.Config{Options: &config.Options{DataDirectory: dataDir}}, configtest.WithWorkingDir(repo))
	source.SetConfigForTest(store)
	source.SetSessionsForTest(sessions)
	source.SetMessagesForTest(messagestore.NewService(q, messagestore.WithDebounce(0)))
	source.SetOwnershipForTest(sessionstore.NewOwnershipStore(conn), "main")
	source.ReportCurrentSession(sess.ID)
	root := NewAppWorkspace(source, store)

	originalBootstrap := bootstrapWorktreeApp
	var shutdowns atomic.Int32
	bootstrapWorktreeApp = func(_ context.Context, path string, _ app.BootstrapOptions) (*app.BootstrapResult, error) {
		target := app.NewForTest(t.Context())
		targetStore := configtest.NewStore(t, &config.Config{Options: &config.Options{DataDirectory: dataDir}}, configtest.WithWorkingDir(path))
		target.SetConfigForTest(targetStore)
		target.SetSessionsForTest(sessions)
		target.SetMessagesForTest(messagestore.NewService(q, messagestore.WithDebounce(0)))
		target.SetOwnershipForTest(sessionstore.NewOwnershipStore(conn), "prepared")
		target.ReportCurrentSession(sess.ID)
		target.ArmPreparedSession()
		require.NoError(t, target.AddCleanup(func(context.Context) error { shutdowns.Add(1); return nil }))
		return &app.BootstrapResult{App: target, Config: targetStore}, nil
	}
	t.Cleanup(func() { bootstrapWorktreeApp = originalBootstrap })

	targetWorkspace, release, err := root.EnterWorktree(t.Context(), "callback")
	require.NoError(t, err)
	release()
	release()
	require.Zero(t, shutdowns.Load(), "current target must survive repeated release callbacks")
	_, shutdownTarget, err := targetWorkspace.(*AppWorkspace).ExitWorktree(t.Context())
	require.NoError(t, err)
	release()
	release()
	require.EqualValues(t, 1, shutdowns.Load(), "a callback retried after exit must shut down the stale target")
	shutdownTarget()
	shutdownTarget()
	require.EqualValues(t, 1, shutdowns.Load(), "stale target shutdown must be idempotent")
}
