package app

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rave-soft/sennit/internal/fsext"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/session"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/stretchr/testify/require"
)

func ownershipTestApp(t *testing.T, ownerID string, store *sessionstore.OwnershipStore) *App {
	t.Helper()
	a := NewForTest(t.Context())
	a.SetConfigForTest(configtest.NewStore(t, &config.Config{}, configtest.WithWorkingDir("/repo")))
	a.SetOwnershipForTest(store, ownerID)
	return a
}

func TestClaimSessionOwnershipDoesNotStealLiveOwner(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	store := sessionstore.NewOwnershipStore(conn)
	first := ownershipTestApp(t, "first", store)
	first.ReportCurrentSession(sess.ID)
	_, err = first.ClaimSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	second := ownershipTestApp(t, "second", store)
	second.ReportCurrentSession(sess.ID)
	_, err = second.ClaimSessionOwnership(t.Context(), sess.ID)
	require.ErrorIs(t, err, ErrSessionOwnershipLost)
	got, err := store.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "first", got.OwnerID)
}

func TestRecoverSessionOwnershipRequiresDeadRegisteredOwner(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	store := sessionstore.NewOwnershipStore(conn)
	first := ownershipTestApp(t, "first", store)
	first.ReportCurrentSession(sess.ID)
	_, err = first.ClaimSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	second := ownershipTestApp(t, "second", store)
	second.ReportCurrentSession(sess.ID)
	_, err = second.RecoverSessionOwnership(t.Context(), sess.ID)
	require.ErrorIs(t, err, ErrSessionOwnershipLive)
	first.UnregisterSessionOwnership()
	recovered, err := second.RecoverSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "second", recovered.OwnerID)
	require.EqualValues(t, 2, recovered.Epoch)
}

func TestClaimSessionOwnershipNeverImplicitlyRecoversDeadOwner(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	store := sessionstore.NewOwnershipStore(conn)
	first := ownershipTestApp(t, "first", store)
	first.ReportCurrentSession(sess.ID)
	_, err = first.ClaimSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	first.UnregisterSessionOwnership()

	second := ownershipTestApp(t, "second", store)
	second.ReportCurrentSession(sess.ID)
	_, err = second.ClaimSessionOwnership(t.Context(), sess.ID)
	require.ErrorIs(t, err, ErrSessionOwnershipLost)
	got, err := store.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "first", got.OwnerID)
	require.EqualValues(t, 1, got.Epoch)
}

func TestPreparedTargetCannotDispatchOrApplyBeforeCommit(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	owners := sessionstore.NewOwnershipStore(conn)
	source := ownershipTestApp(t, "source", owners)
	source.ReportCurrentSession(sess.ID)
	o, err := source.ClaimSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NoError(t, owners.Prepare(t.Context(), sess.ID, "source", o.Epoch, sessionstore.Ownership{OwnerID: "target", OwnerRoot: "/repo"}))

	target := ownershipTestApp(t, "target", owners)
	target.ReportCurrentSession(sess.ID)
	target.ArmPreparedSession()
	coord := &stubDispatchCoordinator{}
	target.SetAgentCoordinatorForTest(coord)
	err = target.AgentDispatcher().Send(sess.ID, "run", "prompt", nil)
	require.ErrorIs(t, err, ErrSessionOwnershipLost)
	var mutated atomic.Bool
	err = target.WithOwnershipFence(t.Context(), sess.ID, func() error {
		mutated.Store(true)
		return nil
	})
	require.ErrorIs(t, err, ErrSessionOwnershipLost)
	require.False(t, mutated.Load())
	require.Zero(t, coord.runCount.Load())
}

func TestResolveCurrentSessionOwnerRejectsStaleRegistryEntry(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	owners := sessionstore.NewOwnershipStore(conn)
	first := ownershipTestApp(t, "first", owners)
	first.ReportCurrentSession(sess.ID)
	o, err := first.ClaimSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	first.UnregisterSessionOwnership()
	second := ownershipTestApp(t, "second", owners)
	second.ReportCurrentSession(sess.ID)
	recovered, err := second.RecoverSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	first.registerSessionOwner(sess.ID, o)
	require.Nil(t, second.ResolveCurrentSessionOwner(t.Context(), sess.ID))
	second.registerSessionOwner(sess.ID, recovered)
	require.Same(t, second, first.ResolveCurrentSessionOwner(t.Context(), sess.ID))
}

// rootKind selects which of a test's two real directories a case means,
// so the table can name roots without spelling out paths that only exist
// once the subtest has created them.
type rootKind int

const (
	rootUnset rootKind = iota
	rootMain
	rootWorktree
)

func TestRecoverSessionOwnershipCoversStableAndPreparingRoots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source rootKind
		target rootKind
	}{
		{name: "stable main restart", source: rootMain},
		{name: "stable worktree restart", source: rootWorktree},
		{name: "preparing enter rolls back to main", source: rootMain, target: rootWorktree},
		{name: "preparing exit rolls back to worktree", source: rootWorktree, target: rootMain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Real directories rather than "/repo" literals: the app
			// canonicalizes the working dir it is configured with, and on
			// Windows a POSIX-looking literal canonicalizes to a
			// drive-qualified path ("/repo" -> "d:\\repo"), so the literal
			// could never equal what came back.
			repoRoot := fsext.Canonical(t.TempDir())
			worktreeRoot := filepath.Join(repoRoot, "worktree")
			require.NoError(t, os.MkdirAll(worktreeRoot, 0o755))
			resolveRoot := func(kind rootKind) string {
				if kind == rootWorktree {
					return worktreeRoot
				}
				return repoRoot
			}
			sourceRoot := resolveRoot(tc.source)
			targetRoot := ""
			if tc.target != rootUnset {
				targetRoot = resolveRoot(tc.target)
			}

			dataDir := t.TempDir()
			conn, err := db.Connect(t.Context(), dataDir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
			sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
			sess, err := sessions.Create(t.Context(), "ownership")
			require.NoError(t, err)
			owners := sessionstore.NewOwnershipStore(conn)
			first := NewForTest(t.Context())
			first.SetConfigForTest(configtest.NewStore(t, &config.Config{}, configtest.WithWorkingDir(sourceRoot)))
			first.SetOwnershipForTest(owners, "dead")
			first.ReportCurrentSession(sess.ID)
			initial, err := first.ClaimSessionOwnership(t.Context(), sess.ID)
			require.NoError(t, err)
			if targetRoot != "" {
				require.NoError(t, owners.Prepare(t.Context(), sess.ID, initial.OwnerID, initial.Epoch, sessionstore.Ownership{OwnerID: "target", OwnerRoot: targetRoot}))
			}
			first.UnregisterSessionOwnership()

			restarted := NewForTest(t.Context())
			t.Cleanup(restarted.ShutdownForTest)
			restarted.SetConfigForTest(configtest.NewStore(t, &config.Config{}, configtest.WithWorkingDir(sourceRoot)))
			restarted.SetOwnershipForTest(owners, "restarted")
			restarted.ReportCurrentSession(sess.ID)
			recovered, err := restarted.RecoverSessionOwnership(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Equal(t, "restarted", recovered.OwnerID)
			require.Equal(t, sourceRoot, recovered.OwnerRoot)
			require.Equal(t, "stable", recovered.Phase)
			require.Equal(t, initial.Epoch+1, recovered.Epoch)
			var staleMutation atomic.Bool
			err = first.WithOwnershipFence(t.Context(), sess.ID, func() error { staleMutation.Store(true); return nil })
			require.ErrorIs(t, err, ErrSessionOwnershipLost)
			require.False(t, staleMutation.Load())
		})
	}
}

func TestSessionTransferGateRejectsUnfinishedToolsWithoutMutation(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	sessions := sessionstore.NewService(q, conn, dataDir)
	sess, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: "unfinished", Name: "bash", Finished: false}},
	})
	require.NoError(t, err)

	owners := sessionstore.NewOwnershipStore(conn)
	a := ownershipTestApp(t, "main", owners)
	a.SetSessionsForTest(sessions)
	a.SetMessagesForTest(messages)
	a.ReportCurrentSession(sess.ID)
	_, err = a.ClaimSessionOwnership(t.Context(), sess.ID)
	require.NoError(t, err)
	var mutated atomic.Bool
	err = a.WithSessionTransferGate(t.Context(), sess.ID, func() error {
		mutated.Store(true)
		return nil
	})
	require.ErrorIs(t, err, ErrSessionBusy)
	require.False(t, mutated.Load())
	got, err := owners.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "stable", got.Phase)
	require.Equal(t, "main", got.OwnerID)
}

// A session nothing ever transferred has no ownership row at all, and must
// still resolve to the App it is running in: this is what
// thread.Manager.resolveDeliveryTarget asks before handing a finished
// delegation's report to the parent session, and a nil answer there drops
// the report entirely - leaving the parent waiting on an answer that never
// comes.
func TestResolveCurrentSessionOwnerResolvesUnclaimedSessionToItsOwnApp(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "never transferred")
	require.NoError(t, err)
	owners := sessionstore.NewOwnershipStore(conn)
	a := ownershipTestApp(t, "main", owners)
	a.ReportCurrentSession(sess.ID)

	_, err = owners.Get(t.Context(), sess.ID)
	require.ErrorIs(t, err, session.ErrNotFound)
	require.Same(t, a, a.ResolveCurrentSessionOwner(t.Context(), sess.ID))
}
