package thread_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/stretchr/testify/require"
)

// newTestParentApp builds a minimal App standing in for a workspace's own
// running App: the thing a task shares instead of spawning its own,
// wired with the same fakeSessions/fakeCoordinator doubles fakeSpawner
// uses for a thread's isolated App, so a task's run can be driven and
// observed the same way. Messages is left nil (as app.NewForTest leaves
// it): nothing here reads it except TestTaskManager_Output, which needs
// real session/message rows satisfying the messages table's FK to
// sessions — see newTestTaskManagerWithRealMessages — not this fake.
func newTestParentApp(t *testing.T) *app.App {
	t.Helper()
	a := app.NewForTest(context.Background())
	t.Cleanup(a.ShutdownForTest)
	a.SetSessionsForTest(&fakeSessions{})
	a.SetAgentCoordinatorForTest(&fakeCoordinator{})
	return a
}

// newTestTaskManagerWithRealMessages is like newTestTaskManager, but
// wires the parent App's Sessions and Messages to a real, shared,
// sqlite-backed pair instead of fakeSessions: TestTaskManager_Output
// inserts real message rows, and the messages table has a FK to
// sessions that fakeSessions (which fabricates a Session value without
// persisting it) would violate.
func newTestTaskManagerWithRealMessages(t *testing.T) (*thread.TaskManager, *app.App, sessionstore.Service, messagestore.Service) {
	t.Helper()
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:    store,
		Spawner:  newFakeSpawner(t),
		RepoRoot: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	q := db.New(conn)
	sessions := sessionstore.NewService(q, conn, "/test/project")
	messages := messagestore.NewService(q)

	parentApp := app.NewForTest(context.Background())
	t.Cleanup(parentApp.ShutdownForTest)
	parentApp.SetSessionsForTest(sessions)
	parentApp.SetMessagesForTest(messages)
	parentApp.SetAgentCoordinatorForTest(&fakeCoordinator{})

	tasks := thread.NewTaskManagerFromManager(mgr, NewTestParentAppSpawner(parentApp), NewTestMessageService(messages))
	return tasks, parentApp, sessions, messages
}

// newTestTaskManager wires a TaskManager sharing a Manager's lifecycle and
// base context, over a fresh parent App, the way [NewTaskManager]
// requires. The Manager itself never creates a thread in these tests; it
// exists only as the shared lc/ctx source and as the thread-facing entry
// point (Handle, Shutdown) that also reaches task entries, since both
// kinds register in the one lifecycle both share.
func newTestTaskManager(t *testing.T, store thread.Store) (*thread.Manager, *thread.TaskManager, *app.App) {
	t.Helper()
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:    store,
		Spawner:  newFakeSpawner(t),
		RepoRoot: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)
	parentApp := newTestParentApp(t)
	tasks := thread.NewTaskManagerFromManager(mgr, NewTestParentAppSpawner(parentApp), NewTestMessageService(parentApp.Messages()))
	return mgr, tasks, parentApp
}

func TestTaskManager_CreateRunsToCompletion(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Equal(t, thread.KindTask, st.Kind)
	require.Empty(t, st.WorktreePath)
	require.Empty(t, st.Branch)
	require.Empty(t, st.BaseBranch)
	require.Equal(t, thread.StatusRunning, st.Status)
	require.NotEmpty(t, st.SessionID)

	// The child session nests under the caller's session, the same
	// relationship a thread's child session gets from Manager.Create.
	sessions := parentApp.SessionsForTest().(*fakeSessions)
	sessions.mu.Lock()
	created := sessions.createdSession
	sessions.mu.Unlock()
	require.Equal(t, st.SessionID, created.ID)
	require.Equal(t, "parent-sess", created.ParentSessionID)

	coord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)

	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, eventuallyTimeout, eventuallyTick)
	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.KindTask, got.Kind)
	require.Equal(t, "finished", got.ResultSummary)
}

// TestTaskManager_CompletionDeliveredToParentNotChild proves the
// completion-inbox delivery target: once a task's run finishes, the
// structured completion goes to its *parent* session (the one that
// created it), resolved via the child session's ParentSessionID — never
// to the task's own child session, which is the one thing a delivery
// bug here could plausibly get backwards (both ids are available at the
// call site, and st.SessionID is far closer at hand than the parent
// link).
func TestTaskManager_CompletionDeliveredToParentNotChild(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	coord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)

	require.Eventually(t, func() bool { return len(coord.deliveredCompletions()) > 0 }, eventuallyTimeout, eventuallyTick)

	delivered := coord.deliveredCompletions()
	require.Len(t, delivered, 1)
	got := delivered[0]

	require.Equal(t, "parent-sess", got.sessionID,
		"the completion must be delivered to the parent session, not the task's own")
	require.NotEqual(t, st.SessionID, got.sessionID,
		"the completion must never land in the task's own child session")

	require.Equal(t, st.ID, got.completion.DelegationID)
	require.Equal(t, string(thread.KindTask), got.completion.Kind)
	require.Equal(t, st.Name, got.completion.Name)
	require.Equal(t, "do the thing", got.completion.Goal)
	require.Equal(t, string(thread.StatusCompleted), got.completion.Status)
	require.Equal(t, st.SessionID, got.completion.ChildSessionID,
		"the child session id travels inside the event for the model's benefit, even though delivery targets the parent")
	require.Equal(t, "finished", got.completion.ResultText)
	require.Empty(t, got.completion.Error)
}

// TestTaskManager_CreateRegistersDelegationParent proves Create wires up
// where a running task's own mid-run ask (a later ask_parent tool, not
// part of this step) should be delivered: registered on the task's own
// coordinator (the same shared parent coordinator here — see
// DelegationParent's doc comment), keyed by the task's own child session
// id, pointing back at the caller's session.
func TestTaskManager_CreateRegistersDelegationParent(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess", Depth: 2})
	require.NoError(t, err)

	coord := parentApp.Coordinator().(*fakeCoordinator)
	registered := coord.registeredDelegationParents()
	require.Len(t, registered, 1)
	got := registered[0]

	require.Equal(t, st.SessionID, got.sessionID,
		"a task's parent must be registered under its own child session id")
	require.Equal(t, coord, got.parent.Parent.(*testCoordinatorAdapter).inner,
		"a task shares its parent's own coordinator")
	require.Equal(t, "parent-sess", got.parent.ParentSessionID)
	require.Equal(t, st.ID, got.parent.DelegationID)
	require.Equal(t, string(thread.KindTask), got.parent.Kind)
	require.Equal(t, st.Name, got.parent.Name)
	require.Equal(t, 2, got.parent.Depth)
}

// TestTaskManager_CompletionCarriesTerminalAtStamp proves
// deliverCompletion stamps TaskCompletion.TerminalAt at delivery time —
// the one place internal/agent's own "Completion delivered" log measures
// against to report how long a completion sat before reaching the model.
// A zero or stale TerminalAt would silently make that duration wrong (or,
// for the zero case, wildly large) without any test noticing, since
// nothing else here depends on the field's value.
func TestTaskManager_CompletionCarriesTerminalAtStamp(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	coord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	before := time.Now()
	publishSuccess(t, parentApp, st.SessionID)
	require.Eventually(t, func() bool { return len(coord.deliveredCompletions()) > 0 }, eventuallyTimeout, eventuallyTick)
	after := time.Now()

	delivered := coord.deliveredCompletions()
	require.Len(t, delivered, 1)
	stamp := delivered[0].completion.TerminalAt

	require.False(t, stamp.IsZero(), "TerminalAt must be stamped, not left at its zero value")
	require.False(t, stamp.Before(before), "TerminalAt must not predate the run completing")
	require.False(t, stamp.After(after), "TerminalAt must not postdate delivery being observed")
}

// TestTaskManager_CreateDoesNotSpawnAppAndReleaseLeavesParentUsable is the
// failure mode that matters most for this delegation kind: a task must
// never get its own isolated App (that is the entire reason it is
// cheaper than a thread), and releasing its runtime once the run
// completes must never tear the shared parent App down.
func TestTaskManager_CreateDoesNotSpawnAppAndReleaseLeavesParentUsable(t *testing.T) {
	store := thread.NewStoreForTest(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	// No second App was spawned: the task's Handle wraps the exact same
	// App instance the test constructed as the parent.
	h := mgr.Handle(st.ID)
	require.NotNil(t, h)
	ph, ok := h.(*parentAppTestHandle)
	require.True(t, ok)
	require.Same(t, parentApp, ph.app)

	coord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)

	// The run completing releases the task's runtime (rt.spawner, a
	// ParentAppSpawner, not the Manager's own thread Spawner).
	require.Eventually(t, func() bool { return mgr.Handle(st.ID) == nil }, eventuallyTimeout, eventuallyTick)

	// Prove the parent App itself is still alive and usable, not torn
	// down by that release.
	_, err = parentApp.Sessions().Create(t.Context(), "still alive")
	require.NoError(t, err)
}

// TestTaskManager_ShutdownJoinsInFlightRun proves Manager.Shutdown joins a
// task's in-flight run exactly the way it already does a thread's: both
// kinds register in the one shared lifecycle, so the same
// closeAdmission-then-wait sequence covers both without Manager knowing
// tasks exist.
func TestTaskManager_ShutdownJoinsInFlightRun(t *testing.T) {
	store := thread.NewStoreForTest(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	coord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	// Deliberately no RunComplete published: the run is left in flight,
	// the way an equivalent thread shutdown test leaves a thread's run.

	require.NoError(t, mgr.Shutdown(context.Background()))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, got.Status)
	require.Nil(t, mgr.Handle(st.ID))

	// Shutdown released the task's runtime via its own Spawner
	// (ParentAppSpawner.Release), not the Manager's thread Spawner — and,
	// critically, did not shut the parent App down.
	_, err = parentApp.Sessions().Create(t.Context(), "still alive after shutdown")
	require.NoError(t, err)
}

// TestTaskManager_CreateAttributesRunToDelegation proves a task's run
// context carries its DelegationRef: a permission request raised by a
// tool the task's run invokes would reach permission.DelegationFromContext
// with the task's identity, not the zero ref a visible turn's tools see.
// The fake coordinator is the same seam other tests use to inspect what a
// dispatched run actually received — no real tool is needed to prove the
// context is threaded through correctly.
func TestTaskManager_CreateAttributesRunToDelegation(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	coord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	coord.mu.Lock()
	got := coord.runs[0].delegation
	origin := coord.runs[0].origin
	coord.mu.Unlock()

	require.Equal(t, permission.DelegationRef{ID: st.ID, Name: st.Name, Kind: "task"}, got,
		"a permission request raised by this run must be attributable to the task, not the parent's own turn")
	require.Equal(t, message.OriginAgent, origin,
		"a task's goal must be dispatched as agent-origin")
}

// TestTaskManager_ShutdownCancelsOnlyItsOwnSessionNotParentWork is the
// sharp test for the CancelAll-vs-Cancel(sessionID) fix: a task's App is
// its parent's, so tearing a task's runtime down must never cancel other
// work already running in that same App. Before the fix, Shutdown's
// per-runtime teardown called AgentCoordinator.CancelAll() — harmless for
// a thread (its App is its own) but, for a task, indistinguishable from
// cancelling the user's own foreground turn.
func TestTaskManager_ShutdownCancelsOnlyItsOwnSessionNotParentWork(t *testing.T) {
	store := thread.NewStoreForTest(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	// The user's own foreground turn, running in the same App a task
	// shares. This is the work a blunt CancelAll would have caught.
	_, err := coord.Run(t.Context(), "foreground-session", "the user's own prompt")
	require.NoError(t, err)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)
	// Deliberately no RunComplete published for the task: its run is left
	// in flight, so Shutdown's teardown has something live to cancel.

	require.NoError(t, mgr.Shutdown(context.Background()))

	require.False(t, coord.cancelAllWasCalled(),
		"shutdown must never call CancelAll on a coordinator shared with the parent's own work")
	require.Equal(t, []string{st.SessionID}, coord.canceledSessions(),
		"shutdown must cancel only the task's own session — the foreground session must never appear here")
}

// publishSuccessForSession is publishSuccess's multi-run-safe sibling:
// publishSuccess assumes the most recently dispatched run is the one to
// complete, which does not hold once several tasks are active
// concurrently (the concurrency-cap tests below routinely are) - it looks
// up sessionID's own run by id instead of assuming it is the last one.
func publishSuccessForSession(t *testing.T, a *app.App, sessionID string) {
	t.Helper()
	coord := a.Coordinator().(*fakeCoordinator)
	var runID string
	require.Eventually(t, func() bool {
		coord.mu.Lock()
		defer coord.mu.Unlock()
		for _, r := range coord.runs {
			if r.sessionID == sessionID {
				runID = r.runID
				return true
			}
		}
		return false
	}, eventuallyTimeout, eventuallyTick)
	a.RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: sessionID, RunID: runID, Text: "finished"})
}

// TestTaskManager_CreateRefusedAtWorkspaceCap proves MaxActiveTasksPerWorkspaceForTest:
// once that many tasks are active, a further Create is refused with a
// clear message naming the count and the limit, and finishing one of the
// existing tasks frees a slot for the next Create to succeed. Each task
// gets its own ParentSessionID, so this exercises the workspace cap alone
// — every one of them is well under the per-parent cap individually.
func TestTaskManager_CreateRefusedAtWorkspaceCap(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	var created []thread.Thread
	for i := range thread.MaxActiveTasksPerWorkspaceForTest {
		st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{
			Goal:            fmt.Sprintf("task %d", i),
			ParentSessionID: fmt.Sprintf("parent-%d", i),
		})
		require.NoError(t, err)
		created = append(created, st)
	}
	require.Eventually(t, func() bool { return coord.runCount() == thread.MaxActiveTasksPerWorkspaceForTest }, eventuallyTimeout, eventuallyTick)

	_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "one too many", ParentSessionID: "parent-overflow"})
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("%d background tasks already running", thread.MaxActiveTasksPerWorkspaceForTest))
	require.Contains(t, err.Error(), fmt.Sprintf("limit %d", thread.MaxActiveTasksPerWorkspaceForTest))
	require.Equal(t, thread.MaxActiveTasksPerWorkspaceForTest, coord.runCount(), "the refused call must never dispatch a run")

	// Finish one of the existing tasks - its slot is freed for the next
	// Create.
	publishSuccessForSession(t, parentApp, created[0].SessionID)
	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), created[0].ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, eventuallyTimeout, eventuallyTick)

	_, err = tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "now it fits", ParentSessionID: "parent-overflow"})
	require.NoError(t, err, "completing one task must free a slot for the next Create")
}

// TestTaskManager_PerParentCapIndependentOfWorkspaceCap proves
// MaxActiveTasksPerParentTurnForTest is its own, tighter limit: one parent
// session can be refused for having too many of its own tasks active
// while the workspace as a whole is nowhere near MaxActiveTasksPerWorkspaceForTest,
// and a *different* parent is unaffected by the first one's refusal.
func TestTaskManager_PerParentCapIndependentOfWorkspaceCap(t *testing.T) {
	require.Less(t, thread.MaxActiveTasksPerParentTurnForTest, thread.MaxActiveTasksPerWorkspaceForTest,
		"this test only proves something if the per-parent cap is reached well before the workspace cap")

	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	for i := range thread.MaxActiveTasksPerParentTurnForTest {
		_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{
			Goal:            fmt.Sprintf("task %d", i),
			ParentSessionID: "busy-parent",
		})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return coord.runCount() == thread.MaxActiveTasksPerParentTurnForTest }, eventuallyTimeout, eventuallyTick)

	_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "one too many for this turn", ParentSessionID: "busy-parent"})
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("this turn already has %d background tasks running", thread.MaxActiveTasksPerParentTurnForTest))
	require.Contains(t, err.Error(), fmt.Sprintf("limit %d", thread.MaxActiveTasksPerParentTurnForTest))

	// A different parent, with none of its own tasks active yet, is
	// unaffected - the workspace cap has plenty of headroom left.
	_, err = tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "unrelated turn's own task", ParentSessionID: "idle-parent"})
	require.NoError(t, err, "the per-parent cap must not leak across different parent sessions")
}

// TestTaskManager_InFlightTaskStillHoldsSlot proves the state the plan
// deliberately preserves: a task waiting on something mid-run - most
// notably a permission prompt the user has not yet answered - is still
// StatusRunning, not some other status the cap could be tempted to
// exclude, and it must keep holding its slot for exactly as long as that
// status holds. This never publishes a RunComplete for the task it
// creates - the same "run left in flight" shape
// TestTaskManager_ShutdownJoinsInFlightRun and the permission-attribution
// tests use for a task blocked mid-run - and shows that in-flight task
// alone is enough to exhaust MaxActiveTasksPerWorkspaceForTest.
func TestTaskManager_InFlightTaskStillHoldsSlot(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	// One task, left running indefinitely (e.g. blocked on a permission
	// prompt the user has not answered yet) - never completed.
	blocked, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "waiting on a prompt", ParentSessionID: "parent-blocked"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	got, err := store.Get(t.Context(), blocked.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status, "blocked mid-run is still StatusRunning, not a distinct 'waiting' status")

	// Fill every remaining slot with other, distinct-parent tasks.
	for i := range thread.MaxActiveTasksPerWorkspaceForTest - 1 {
		_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{
			Goal:            fmt.Sprintf("filler %d", i),
			ParentSessionID: fmt.Sprintf("parent-filler-%d", i),
		})
		require.NoError(t, err)
	}

	// The workspace is now at its cap purely because of one still-blocked
	// task plus the filler - the blocked one never finished and never
	// stopped counting.
	_, err = tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "over the cap", ParentSessionID: "parent-overflow"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "background tasks already running")
}

// TestTaskManager_TerminalTasksDoNotOccupySlots proves the other half of
// "active, not rows": MaxActiveTasksPerWorkspaceForTest many tasks that have all
// since finished do not, together, block a new one - only tasks still
// Status.Active() count.
func TestTaskManager_TerminalTasksDoNotOccupySlots(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	for i := range thread.MaxActiveTasksPerWorkspaceForTest {
		st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{
			Goal:            fmt.Sprintf("task %d", i),
			ParentSessionID: fmt.Sprintf("parent-%d", i),
		})
		require.NoError(t, err)
		publishSuccessForSession(t, parentApp, st.SessionID)
		require.Eventually(t, func() bool {
			got, err := store.Get(t.Context(), st.ID)
			return err == nil && got.Status == thread.StatusCompleted
		}, eventuallyTimeout, eventuallyTick)
	}
	require.Equal(t, thread.MaxActiveTasksPerWorkspaceForTest, coord.runCount())

	_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "workspace is actually idle", ParentSessionID: "parent-fresh"})
	require.NoError(t, err, "MaxActiveTasksPerWorkspaceForTest terminal tasks must not occupy any slots")
}

// seedThreadRow inserts a bare kind = KindThread row directly through
// store, bypassing Manager.Create's git machinery entirely: these tests
// only need a thread's id to exist in the shared table, to prove the
// task_* guards reject it.
func seedThreadRow(t *testing.T, store thread.Store) thread.Thread {
	t.Helper()
	st, err := store.Create(t.Context(), thread.CreateParams{
		Name:         "a-thread",
		Goal:         "x",
		BaseBranch:   "main",
		Branch:       "thread/a-thread",
		WorktreePath: t.TempDir(),
		Kind:         thread.KindThread,
	})
	require.NoError(t, err)
	return st
}

func TestTaskManager_ListReturnsOnlyTasks(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)

	task1, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "a", ParentSessionID: "p1"})
	require.NoError(t, err)
	task2, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "b", ParentSessionID: "p2"})
	require.NoError(t, err)
	threadRow := seedThreadRow(t, store)

	got, err := tasks.List(t.Context())
	require.NoError(t, err)
	ids := make([]string, len(got))
	for i, st := range got {
		ids[i] = st.ID
	}
	require.ElementsMatch(t, []string{task1.ID, task2.ID}, ids)
	require.NotContains(t, ids, threadRow.ID, "a thread row must never appear in a task listing")
}

func TestTaskManager_GetHappyPath(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)

	created, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "look into it", ParentSessionID: "p1"})
	require.NoError(t, err)

	got, err := tasks.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, "look into it", got.Goal)
	require.Equal(t, thread.KindTask, got.Kind)
}

func TestTaskManager_GetRejectsThreadID(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)
	threadRow := seedThreadRow(t, store)

	_, err := tasks.Get(t.Context(), threadRow.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a task")
}

// TestTaskManager_CancelLeavesTerminalWithReasonAndParentUntouched is the
// sharp test for task_cancel: it must produce a real terminal status
// transition (not merely stop a goroutine), and — since a task's App is
// its parent's — it must reach only the task's own session, never the
// foreground work sharing that same App and coordinator.
func TestTaskManager_CancelLeavesTerminalWithReasonAndParentUntouched(t *testing.T) {
	store := thread.NewStoreForTest(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	_, err := coord.Run(t.Context(), "foreground-session", "the user's own prompt")
	require.NoError(t, err)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)
	// Deliberately no RunComplete published: the run is left in flight.

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, "no longer needed"))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCancelled, got.Status)
	require.Equal(t, "no longer needed", got.Error)

	require.False(t, coord.cancelAllWasCalled())
	require.Equal(t, []string{st.SessionID}, coord.canceledSessions(),
		"cancel must reach only the task's own session, not the foreground one sharing its App")
	require.Nil(t, mgr.Handle(st.ID), "the runtime must be released")
}

func TestTaskManager_CancelDefaultsReasonWhenEmpty(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, ""))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCancelled, got.Status)
	require.Equal(t, "cancelled", got.Error)
}

// TestTaskManager_CancelAlreadyFinishedIsNoop proves a finished task's
// real outcome is never clobbered by a Cancel call that arrives too late.
func TestTaskManager_CancelAlreadyFinishedIsNoop(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)
	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, eventuallyTimeout, eventuallyTick)

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, "too late"))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, got.Status, "a finished task's real outcome must not be overwritten")
	require.Empty(t, coord.canceledSessions(), "no live runtime means nothing to cancel")
}

func TestTaskManager_CancelRejectsThreadID(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)
	threadRow := seedThreadRow(t, store)
	_, err := store.SetStatus(t.Context(), threadRow.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)

	err = tasks.Cancel(t.Context(), threadRow.ID, "nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a task")

	got, err := store.Get(t.Context(), threadRow.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status, "a rejected call must not touch the thread's row")
}

// TestTaskManager_SendReachesLiveTask proves the live-runtime branch of
// the shared lifecycle.send is reached: a task with a run still in
// flight (no RunComplete published yet) gets the follow-up queued into
// that same runtime, not a respawned one.
func TestTaskManager_SendReachesLiveTask(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	require.NoError(t, sendErr(tasks.Send(t.Context(), st.ID, "follow up")))
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)

	coord.mu.Lock()
	last := coord.runs[len(coord.runs)-1]
	coord.mu.Unlock()
	require.Equal(t, st.SessionID, last.sessionID)
	require.Equal(t, "follow up", last.prompt)
	require.Equal(t, message.OriginAgent, last.origin,
		"a task_send follow-up must be dispatched as agent-origin")
}

// TestTaskManager_SendReactivatesUnspawnedTask proves the not-live branch:
// once a task's run completes and its runtime is released, Send
// respawns — for a task, that only means rebinding to the same parent
// App via ParentAppSpawner — and dispatches there.
func TestTaskManager_SendReactivatesUnspawnedTask(t *testing.T) {
	store := thread.NewStoreForTest(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)
	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, eventuallyTimeout, eventuallyTick)
	require.Nil(t, mgr.Handle(st.ID), "runtime must have been released once the run completed")

	require.NoError(t, sendErr(tasks.Send(t.Context(), st.ID, "one more thing")))

	h := mgr.Handle(st.ID)
	require.NotNil(t, h, "Send must reactivate the task")
	ph, ok := h.(*parentAppTestHandle)
	require.True(t, ok)
	require.Same(t, parentApp, ph.app, "reactivating must rebind to the same parent App, not spawn a new one")

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status)
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)
}

// TestTaskManager_SendReactivationPreservesCascadeDepth is the regression
// test for Send's re-registration of DelegationParent on reactivation: it
// used to hardcode Depth: 0 regardless of the depth stamped at Create,
// which weakens maxTaskCascadeDepth's guard against runaway background
// task chains once a task is reactivated through task_send.
func TestTaskManager_SendReactivationPreservesCascadeDepth(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess", Depth: 2})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)
	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, eventuallyTimeout, eventuallyTick)

	require.NoError(t, sendErr(tasks.Send(t.Context(), st.ID, "one more thing")))

	registered := coord.registeredDelegationParents()
	require.Len(t, registered, 2, "Create and the reactivating Send must each register a DelegationParent")
	last := registered[len(registered)-1]
	require.Equal(t, 2, last.parent.Depth,
		"Send must re-register the task's own stored cascade depth, not reset it to 0")
}

// TestTaskManager_SendRefusesCancelledTask is the constraint the
// coordinator called out explicitly: a task task_cancel stopped must not
// be silently resumed by task_send, because cancelling was a decision,
// not a pause.
func TestTaskManager_SendRefusesCancelledTask(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, "no longer needed"))

	err = sendErr(tasks.Send(t.Context(), st.ID, "please continue"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "cancelled")
	require.Equal(t, 1, coord.runCount(), "a refused Send must never dispatch")
}

func TestTaskManager_SendRejectsThreadID(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)
	threadRow := seedThreadRow(t, store)

	err := sendErr(tasks.Send(t.Context(), threadRow.ID, "hi"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a task")
}

// seedMessage inserts a real message row for sessionID, satisfying the
// messages table's FK to sessions — see
// newTestTaskManagerWithRealMessages for why this needs a real session.
func seedMessage(t *testing.T, messages messagestore.Service, sessionID string, role message.MessageRole, text string) {
	t.Helper()
	_, err := messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  role,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
}

// TestTaskManager_OutputReturnsUserAndAssistantTextOnly proves Output's
// content filter: tool traffic is exactly what would let a long-running
// task quietly fill the parent's context on every check, so it is
// dropped, leaving only user/assistant text in order.
func TestTaskManager_OutputReturnsUserAndAssistantTextOnly(t *testing.T) {
	tasks, parentApp, sessions, messages := newTestTaskManagerWithRealMessages(t)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	parentSess, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "investigate X", ParentSessionID: parentSess.ID})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	seedMessage(t, messages, st.SessionID, message.User, "investigate X")
	seedMessage(t, messages, st.SessionID, message.Tool, "tool output that should not appear")
	seedMessage(t, messages, st.SessionID, message.Assistant, "here is what I found")

	out, err := tasks.Output(t.Context(), st.ID, 0)
	require.NoError(t, err)
	require.Equal(t, 2, out.Total)
	require.Len(t, out.Messages, 2)
	require.Equal(t, "investigate X", out.Messages[0].Text)
	require.Equal(t, "here is what I found", out.Messages[1].Text)
}

// TestTaskManager_OutputReportsTruncation is the bound the coordinator
// asked to be explicit rather than silent: a task with more messages
// than the limit must report how many were omitted, not just hand back
// a suspiciously-round-looking tail.
func TestTaskManager_OutputReportsTruncation(t *testing.T) {
	tasks, parentApp, sessions, messages := newTestTaskManagerWithRealMessages(t)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	parentSess, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "go", ParentSessionID: parentSess.ID})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	for i := range 5 {
		seedMessage(t, messages, st.SessionID, message.User, fmt.Sprintf("message %d", i))
	}

	out, err := tasks.Output(t.Context(), st.ID, 2)
	require.NoError(t, err)
	require.Equal(t, 5, out.Total, "Total must report the full count even when truncated")
	require.Len(t, out.Messages, 2, "Messages must be capped at the requested limit")
	require.Equal(t, "message 3", out.Messages[0].Text, "the tail is the most recent messages, in chronological order")
	require.Equal(t, "message 4", out.Messages[1].Text)
}

func TestTaskManager_OutputRejectsThreadID(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)
	threadRow := seedThreadRow(t, store)

	_, err := tasks.Output(t.Context(), threadRow.ID, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a task")
}

// setSessionErrStore wraps a real Store and makes every SetSession call
// fail, for driving TaskManager.Create's SetSession-failure path
// (regression coverage for the st-clobbered-on-error bug fixed alongside
// Manager.Create's identical one - see manager.go's own SetSession call).
type setSessionErrStore struct {
	thread.Store
	err error
}

func (s *setSessionErrStore) SetSession(ctx context.Context, id, sessionID string) (thread.Thread, error) {
	return thread.Thread{}, s.err
}

// TestTaskManager_CreateFailsWithRealIDWhenSetSessionErrors is the
// regression test for tasks.go's Create: `st, err =
// t.store.SetSession(...)` used to overwrite st with the store's zero
// value on error, so the ensuing t.failCreate(ctx, st, err) recorded the
// failure against an EMPTY task id instead of the real one. The actual
// task row was left stuck at its pre-failure status forever (and kept
// occupying a checkActiveCaps slot until process restart), because
// nothing ever wrote StatusFailed to its real row. Create itself returns
// (Thread{}, err) on every failure path, so this asserts against the
// store directly rather than Create's return value.
func TestTaskManager_CreateFailsWithRealIDWhenSetSessionErrors(t *testing.T) {
	setSessionErr := fmt.Errorf("set-session boom")
	store := &setSessionErrStore{Store: thread.NewStoreForTest(t), err: setSessionErr}
	_, tasks, _ := newTestTaskManager(t, store)

	_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.Error(t, err)
	require.ErrorIs(t, err, setSessionErr)

	all, listErr := store.ListAll(t.Context())
	require.NoError(t, listErr)
	require.Len(t, all, 1, "the task row created before SetSession failed must still exist")

	got := all[0]
	require.NotEmpty(t, got.ID, "the failure must be recorded against the real task id, not an empty one")
	require.Equal(t, thread.StatusFailed, got.Status,
		"the real task row must be marked failed, not left stuck pending forever")
	require.Contains(t, got.Error, setSessionErr.Error())
}

// setStatusRunningErrStore wraps a real Store and fails only the
// SetStatus(StatusRunning) call Create makes right before startRun,
// leaving every other SetStatus call (notably failCreate's own
// SetStatus(StatusFailed)) to hit the real store.
type setStatusRunningErrStore struct {
	thread.Store
	err error
}

func (s *setStatusRunningErrStore) SetStatus(ctx context.Context, id string, params thread.SetStatusParams) (thread.Thread, error) {
	if params.Status == thread.StatusRunning {
		return thread.Thread{}, s.err
	}
	return s.Store.SetStatus(ctx, id, params)
}

// TestTaskManager_CreateFailsThroughFailCreateWhenSetStatusRunningErrors is
// the regression test for tasks.go's Create: the setStatus(StatusRunning)
// error used to be returned directly, unlike every neighbouring failure
// path in Create, so the task row was left at its pre-failure status
// (still occupying a checkActiveCaps slot) forever instead of being
// recorded as failed via failCreate.
func TestTaskManager_CreateFailsThroughFailCreateWhenSetStatusRunningErrors(t *testing.T) {
	setStatusErr := fmt.Errorf("set-status-running boom")
	store := &setStatusRunningErrStore{Store: thread.NewStoreForTest(t), err: setStatusErr}
	_, tasks, _ := newTestTaskManager(t, store)

	_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.Error(t, err)
	require.ErrorIs(t, err, setStatusErr)

	all, listErr := store.ListAll(t.Context())
	require.NoError(t, listErr)
	require.Len(t, all, 1)

	got := all[0]
	require.Equal(t, thread.StatusFailed, got.Status,
		"a setStatus(Running) failure must go through failCreate, not strand the row at its prior status")
	require.Contains(t, got.Error, setStatusErr.Error())
}

// cancelOnSetSessionStore wraps a real Store and, on the SetSession call
// TaskManager.Create makes right after CreateTaskSession, cancels the
// caller's own ctx and fails with ctx.Err() - reproducing the case
// failCreate must handle: the cause of the failure IS the context that
// just got cancelled, not some unrelated store error.
type cancelOnSetSessionStore struct {
	thread.Store
	cancel context.CancelFunc
}

func (s *cancelOnSetSessionStore) SetSession(ctx context.Context, id, sessionID string) (thread.Thread, error) {
	s.cancel()
	return thread.Thread{}, ctx.Err()
}

// TestTaskManager_FailCreateRecordsFailureOnCancelledContext pins
// TaskManager.failCreate against the same shape of bug already fixed in
// [Manager.failCreate] and handleRunComplete: writing the terminal
// failure status with the same ctx that just got cancelled means that
// write fails too, leaving the task row stuck at its transient status
// (StatusPending here) forever instead of being recorded as failed.
func TestTaskManager_FailCreateRecordsFailureOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelOnSetSessionStore{Store: thread.NewStoreForTest(t), cancel: cancel}
	_, tasks, _ := newTestTaskManager(t, store)

	_, err := tasks.Create(ctx, thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.Error(t, err)

	all, listErr := store.ListAll(t.Context())
	require.NoError(t, listErr)
	require.Len(t, all, 1)

	got := all[0]
	require.Equal(t, thread.StatusFailed, got.Status,
		"the terminal failure must be recorded even though the context that caused it was already cancelled")
}

// flakyGetStore wraps a real Store and fails the first failsLeft calls to
// Get(id) before letting the real store answer, for driving
// handleRunComplete's post-teardown store.Get retry.
type flakyGetStore struct {
	thread.Store
	id        string
	failsLeft int32
}

func (s *flakyGetStore) Get(ctx context.Context, id string) (thread.Thread, error) {
	if id == s.id && atomic.AddInt32(&s.failsLeft, -1) >= 0 {
		return thread.Thread{}, fmt.Errorf("transient get boom")
	}
	return s.Store.Get(ctx, id)
}

// TestTaskManager_RunCompleteSurvivesTransientStoreGetFailure is the
// regression test for handleRunComplete's post-teardown store.Get: the
// runtime is already released by the time it runs, so nothing else would
// ever retry this terminal transition. A bare `if err != nil { return }`
// used to strand the row in StatusRunning until a restart on any transient
// store hiccup; it must now retry and still land the terminal status.
func TestTaskManager_RunCompleteSurvivesTransientStoreGetFailure(t *testing.T) {
	flaky := &flakyGetStore{Store: thread.NewStoreForTest(t)}
	_, tasks, parentApp := newTestTaskManager(t, flaky)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	// Only start failing Get(st.ID) now: handleRunComplete's own Get,
	// right after run completion, is the one under test, not any of the
	// Gets Create itself may have made.
	flaky.id = st.ID
	atomic.StoreInt32(&flaky.failsLeft, 2)

	publishSuccessForSession(t, parentApp, st.SessionID)

	require.Eventually(t, func() bool {
		got, err := flaky.Store.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, 2*time.Second, 10*time.Millisecond,
		"a transient store.Get failure right after run completion must not strand the task in StatusRunning")
}

// TestTaskManager_CreateHonorsCallerSuppliedSessionID proves a caller can
// name the task's child session. Delegations launched from a tool call
// pass the "<messageID>$$<toolCallID>" identity the transcript derives for
// that call, which is the only reason opening the delegation from the
// parent's chat resolves to anything at all.
func TestTaskManager_CreateHonorsCallerSuppliedSessionID(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{
		Goal:            "do the thing",
		ParentSessionID: "parent-sess",
		SessionID:       "msg-7$$call-1",
	})
	require.NoError(t, err)
	require.Equal(t, "msg-7$$call-1", st.SessionID)

	sessions := parentApp.SessionsForTest().(*fakeSessions)
	sessions.mu.Lock()
	created := sessions.createdSession
	sessions.mu.Unlock()
	require.Equal(t, "msg-7$$call-1", created.ID)
	require.Equal(t, "parent-sess", created.ParentSessionID)
}

// TestTaskManager_CancelFinalizesOnACanceledContext is the cancel-side
// twin of TestManager_RunCompleteRecordsAndDeliversOnACanceledContext.
// The bookkeeping ran on the caller's context, and the contexts a cancel
// arrives on are exactly the ones a cancel kills: the finalizing
// transaction never opened ("cancel finalization: beginning transaction:
// context canceled"), so the task stayed at running, its workspace was
// never released, and the parent was never told the delegation ended.
func TestTaskManager_CancelFinalizesOnACanceledContext(t *testing.T) {
	store := thread.NewStoreForTest(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	dead, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, tasks.Cancel(dead, st.ID, "no longer needed"))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCancelled, got.Status,
		"the terminal status must be recorded even though the context was canceled")
	require.Equal(t, "no longer needed", got.Error)
	require.Nil(t, mgr.Handle(st.ID), "the runtime must still be released")
}

// TestTaskManager_WaitingOnItsOwnDelegationIsNotFinished is the
// regression test for a pipeline reviewing work that was still being
// written.
//
// A delegation that itself delegates ends its turn to wait — that is how
// an agent waits, and its completion inbox wakes it to carry on. The
// lifecycle read that turn's end as the delegation's answer: it recorded
// completed and told the parent, so a coordinator running
// developer-then-reviewer started the reviewer while the developer was
// still working. The delegation now stays running until its own
// delegations are accounted for.
func TestTaskManager_WaitingOnItsOwnDelegationIsNotFinished(t *testing.T) {
	store := thread.NewStoreForTest(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	developer, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "implement it", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	// The developer delegates in turn, and ends its turn to wait for it.
	child, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "a piece of it", ParentSessionID: developer.SessionID})
	require.NoError(t, err)
	publishSuccessForSession(t, parentApp, developer.SessionID)

	got, err := store.Get(t.Context(), developer.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status,
		"a delegation waiting on its own delegation has not answered its goal")
	require.Empty(t, coord.deliveredCompletions(),
		"the parent must not be told its delegation is done while it is waiting")
	require.NotNil(t, mgr.Handle(developer.ID), "the workspace it will be woken into must stay live")

	// The child answers and its result is delivered, so the developer has
	// nothing outstanding; the continuation that resumes it carries no run
	// id of its own, which is exactly what the park has to accept.
	publishSuccessForSession(t, parentApp, child.SessionID)
	require.Eventually(t, func() bool {
		st, getErr := store.Get(t.Context(), child.ID)
		return getErr == nil && st.Status == thread.StatusCompleted && !st.CompletionPending
	}, eventuallyTimeout, eventuallyTick)

	parentApp.RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{
		SessionID: developer.SessionID, Text: "implemented",
	})

	require.Eventually(t, func() bool {
		st, getErr := store.Get(t.Context(), developer.ID)
		return getErr == nil && st.Status == thread.StatusCompleted
	}, eventuallyTimeout, eventuallyTick, "the delegation must finalize once it really is done")
	require.Eventually(t, func() bool {
		for _, d := range coord.deliveredCompletions() {
			if d.completion.DelegationID == developer.ID {
				return true
			}
		}
		return false
	}, eventuallyTimeout, eventuallyTick, "and the parent must be told then")
}

// TestTaskManager_CancelReachesTheWholeSubtree proves a cancel follows
// the delegation tree down rather than stopping at the task named. A
// task's own delegations exist to answer it, and one that has been
// cancelled will never read their answers — but left running they go on
// editing the same workspace with nothing above them, which is exactly
// what happened the night this was written.
//
// The bystander is the other half of the claim: the sweep must follow
// parent pointers, not cancel every task in the workspace.
func TestTaskManager_CancelReachesTheWholeSubtree(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	parent, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "the delegated work", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	child, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "a piece of it", ParentSessionID: parent.SessionID})
	require.NoError(t, err)
	grandchild, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "a piece of that", ParentSessionID: child.SessionID})
	require.NoError(t, err)
	bystander, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "unrelated work", ParentSessionID: "someone-elses-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 4 }, eventuallyTimeout, eventuallyTick)

	require.NoError(t, tasks.Cancel(t.Context(), parent.ID, "wrong approach"))

	for _, id := range []string{parent.ID, child.ID, grandchild.ID} {
		got, err := store.Get(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, thread.StatusCancelled, got.Status,
			"a delegation of a cancelled task has nobody left to report to and must be stopped too")
	}
	childRow, err := store.Get(t.Context(), child.ID)
	require.NoError(t, err)
	require.Contains(t, childRow.Error, parent.ID,
		"a task cancelled along with its parent must say which cancel took it")
	require.Contains(t, childRow.Error, "wrong approach",
		"and must carry the reason the person or agent actually gave")

	stillRunning, err := store.Get(t.Context(), bystander.ID)
	require.NoError(t, err)
	require.NotEqual(t, thread.StatusCancelled, stillRunning.Status,
		"the sweep must follow parent pointers, not cancel the whole workspace")
}

// TestTaskManager_CancelTellsTheParentSession proves cancelling a task
// delivers the outcome to the session waiting on it, the way every other
// terminal status already did.
//
// Cancel was the one terminal outcome that used to be silent here, and
// silence is the worst of them to pick: a parent that has been taught to
// end its turn and wait for a report — which is how waiting for a
// delegation is spelled — then waits for one that is never coming. It is
// no answer that the canceller is usually the parent and gets the tool
// result: nothing says the canceller is the parent (a delegation can
// cancel its own child, and once famously cancelled itself), and a
// duplicate line in a transcript costs incomparably less than a session
// idle until morning.
func TestTaskManager_CancelTellsTheParentSession(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	require.Empty(t, coord.deliveredCompletions(), "nothing is reported while the task is still running")

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, "no longer needed"))

	require.Eventually(t, func() bool { return len(coord.deliveredCompletions()) == 1 }, eventuallyTimeout, eventuallyTick)
	delivered := coord.deliveredCompletions()[0]
	require.Equal(t, "parent-sess", delivered.sessionID)
	require.Equal(t, st.ID, delivered.completion.DelegationID)
	require.Equal(t, string(thread.StatusCancelled), delivered.completion.Status)
	require.Contains(t, delivered.completion.Error, "no longer needed")
}

// TestTaskManager_SecondReportIsMarkedAsARepeat covers what a follow-up
// conversation with a delegation looks like from the parent's side.
//
// task_send reactivates a finished task, so it runs again, finishes
// again, and reports again — and each report wakes the parent, whose
// woken turn tends to dispatch the next piece of work. One task in
// production reported "completed" three times in thirteen minutes for
// this reason, and read from outside it was indistinguishable from the
// same item being redone from the top. The count is what lets the reader
// tell "here is the answer to what you just asked me" from "here is that
// finished, again".
func TestTaskManager_SecondReportIsMarkedAsARepeat(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.Coordinator().(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)
	require.Eventually(t, func() bool { return len(coord.deliveredCompletions()) == 1 }, eventuallyTimeout, eventuallyTick)
	require.Zero(t, coord.deliveredCompletions()[0].completion.PriorReports,
		"a delegation's first report is nobody's repeat")

	// The parent asks a follow-up, which is what puts a finished task
	// back to work.
	_, err = tasks.Send(t.Context(), st.ID, "one more thing")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)

	require.Eventually(t, func() bool { return len(coord.deliveredCompletions()) == 2 }, eventuallyTimeout, eventuallyTick)
	require.Equal(t, 1, coord.deliveredCompletions()[1].completion.PriorReports,
		"the answer to a follow-up must not read as the original goal finishing all over again")
}
