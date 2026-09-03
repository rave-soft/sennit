package thread

import (
	"context"
	"time"
)

// MaxActiveTasksPerWorkspaceForTest and MaxActiveTasksPerParentTurnForTest
// re-export tasks.go's own maxActiveTasksPerWorkspace/
// maxActiveTasksPerParentTurn for tests outside this package that need to
// assert against the exact caps (error text, loop bounds). The constants
// themselves stay unexported — see their doc comment on why they are
// deliberately not configuration — this only gives tests a name to read
// the same value through.
const (
	MaxActiveTasksPerWorkspaceForTest  = maxActiveTasksPerWorkspace
	MaxActiveTasksPerParentTurnForTest = maxActiveTasksPerParentTurn
)

// TaskIdleTimeoutForTest re-exports the idle watchdog's silence budget,
// on the same terms as the caps above: the constant stays unexported
// (see watchdog.go for why it is not configuration), and this only lets a
// test outside the package position a clock relative to the real value
// instead of hardcoding a second copy of it.
const TaskIdleTimeoutForTest = taskIdleTimeout

// NewTaskManagerFromManager constructs a TaskManager sharing mgr's own
// lifecycle and context — exactly the wiring threadspawn.Attach performs
// in production. Despite living in this file, it is not test-only:
// threadspawn.Attach calls it directly for the production TaskManager, and
// tests in other packages use it too, for a real, working TaskManager
// without Attach's global-DB-dir and git-toplevel dependencies, which
// assume a real project checkout and a shared process-wide database, both
// awkward in an isolated test.
//
// It exists because NewTaskManager itself requires mgr's unexported
// lc/ctx fields and so can only be called from within this package; every
// other caller — production and test alike — goes through here.
func NewTaskManagerFromManager(mgr *Manager, spawner Spawner, messages MessageService) *TaskManager {
	return NewTaskManager(mgr.store, spawner, messages, mgr.lc, mgr.ctx)
}

// PublishForTest emits a lifecycle event through the manager's own event
// broker, for tests outside this package that need to drive the event
// stream (production emits only through real lifecycle transitions, which
// are hard to reach from the composition seam). It publishes through the
// manager's lifecycle so the event is indistinguishable from a real one.
func (m *Manager) PublishForTest(t EventType, st Thread) {
	m.lc.publish(t, st)
}

// ParentSessionIDForTest exposes the in-memory parent-session link a
// delegation reports its completion to. It exists so tests outside this
// package can prove the link survives the path that created it — the
// field itself stays unexported because nothing in production reads it
// from outside the manager.
func (m *Manager) ParentSessionIDForTest(id string) string {
	c := m.lc.existingControl(id)
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.parentSessionID
}

// RuntimeForTest reports the RunID a delegation's live runtime currently
// treats as the owner of its workspace (see handleRunComplete), and
// whether it has a live runtime at all. It exists so tests outside this
// package can pin exactly when ownership moves without reaching into the
// unexported control/runtime bookkeeping directly.
func (m *Manager) RuntimeForTest(id string) (runID string, live bool) {
	c := m.lc.existingControl(id)
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtime == nil {
		return "", false
	}
	return c.runtime.runID, true
}

// AwaitingDelegationsForTest reports whether a delegation's runtime is
// currently parked — its own turn finished but rt.awaitingDelegations is
// still set because delegations of its own are in flight (see
// lifecycle.parkIfAwaitingDelegations). StatusRunning alone cannot
// distinguish "still running its own goal" from "parked awaiting
// children": both read running from the public API. Tests that need to
// wait for the park itself, rather than for the pre-park running state it
// is indistinguishable from, read this instead.
func (m *Manager) AwaitingDelegationsForTest(id string) bool {
	c := m.lc.existingControl(id)
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtime == nil {
		return false
	}
	return c.runtime.awaitingDelegations
}

// ResolveDeliveryTargetForTest exposes resolveDeliveryTarget — the
// lifecycle's deliveryResolver hook — for tests outside this package that
// need to exercise its branches directly rather than through a full
// run/merge flow.
func (m *Manager) ResolveDeliveryTargetForTest(ctx context.Context, handle Handle, st Thread) (Workspace, string, bool) {
	return m.resolveDeliveryTarget(ctx, handle, st)
}

// SetStatusForTest forces a delegation's status through the lifecycle,
// exactly as a real transition would, for tests outside this package that
// need to put a row into a state no ordinary Manager call reaches (for
// example: a merged status with no completed merge behind it, to drive
// discardMerged in isolation).
func (m *Manager) SetStatusForTest(ctx context.Context, id string, status Status, errText, resultSummary string, completedAt int64) (Thread, error) {
	return m.lc.setStatus(ctx, id, status, errText, resultSummary, completedAt)
}

// DiscardMergedForTest exposes discardMerged for tests outside this
// package that need to drive it directly, independent of the merge flow
// that ordinarily triggers it.
func (m *Manager) DiscardMergedForTest(ctx context.Context, threadID string) {
	m.discardMerged(ctx, threadID)
}

// WorktreeDirForTest exposes the resolved worktree directory NewManager
// computed from ManagerOptions, for tests outside this package that pin
// that resolution (empty/relative/absolute WorktreeDir, DataDir
// fallback).
func (m *Manager) WorktreeDirForTest() string {
	return m.worktreeDir
}

// ShutdownStartedForTest exposes the channel Shutdown closes the moment
// it begins admission-blocking, before its cleanup goroutine runs, for
// tests outside this package that need to synchronize on that exact
// point rather than sleeping.
func (m *Manager) ShutdownStartedForTest() <-chan struct{} {
	return m.shutdownStarted
}

// HandleRunCompleteForTest drives lifecycle.handleRunComplete directly,
// for tests outside this package that need to hand it a context of their
// own choosing. Production always reaches it through a run's own
// completion (see startRun and the RunComplete watcher), whose context a
// test cannot cancel without shutting the manager down — and a canceled
// context is exactly the case the terminal bookkeeping has to survive.
func (m *Manager) HandleRunCompleteForTest(ctx context.Context, id string, rc RunComplete) {
	m.lc.handleRunComplete(ctx, id, rc)
}

// SweepIdleTasksForTest drives exactly one pass of the idle watchdog at
// the instant now, for tests outside this package. The production sweep
// runs on a ticker against the wall clock (see startIdleWatchdog), which
// a test can neither hurry nor position precisely; this is the same
// function that ticker calls, with the clock handed in.
func (t *TaskManager) SweepIdleTasksForTest(ctx context.Context, now time.Time) {
	t.sweepIdleTasks(ctx, now)
}
