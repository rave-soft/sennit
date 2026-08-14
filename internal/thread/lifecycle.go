package thread

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/pubsub"
)

// runtimeState tracks the in-memory bookkeeping for an entity whose
// workspace is currently spawned: the handle to release on removal, the
// Spawner that produced it (kept per-runtime rather than per-lifecycle,
// since threads and tasks share one lifecycle but use different Spawners —
// releasing via the wrong one is exactly the kind of mistake that would
// either leak a workspace or, for a task, tear down its parent App), and
// the cancel function for its RunComplete watcher goroutine.
type runtimeState struct {
	handle      Handle
	spawner     Spawner
	watchCancel context.CancelFunc
	runID       string
}

// threadControl is permanent while an entity is known to the lifecycle. opMu
// serializes lifecycle operations for one entity without serializing
// unrelated entities or holding the lifecycle map lock across I/O.
type threadControl struct {
	opMu    sync.Mutex
	mu      sync.Mutex
	runtime *runtimeState
	removed bool
	// depth is the background-delegation cascade depth the creating turn
	// stamped at Create (see TaskManager.Create and TaskCreateArgs.Depth
	// — only tasks set this to anything nonzero today). handleRunComplete
	// reads it back to compute an auto-woken continuation's depth.
	depth int
	// parentSessionID is the session this entity's own session nests
	// under, stamped once at Create (see Manager.Create/TaskManager.Create)
	// and never touched again. It exists here — in memory, not persisted —
	// for the same reason depth does: a task can resolve its parent via
	// Sessions.Get(childSessionID).ParentSessionID because it shares its
	// parent's own App and session store (ParentAppSpawner), but a thread
	// spawns its own isolated App with a completely separate database
	// (LocalSpawner) — its child session's row has no reachable path back
	// to a parent session living in a different store, so the link has to
	// be captured directly instead. Empty means no parent (a thread
	// created with no ParentSessionID — CreateArgs.ParentSessionID is
	// optional, unlike a task's) — see Manager.resolveDeliveryTarget.
	parentSessionID string
}

// ErrManagerClosed is returned by mutating manager operations once shutdown
// has started.
var ErrManagerClosed = errors.New("thread: manager is closed")

// runCompleteHook lets an overlay intervene when a run finishes
// successfully, before the generic lifecycle would otherwise rest the
// entity at StatusCompleted. It is called with the entity's opMu held —
// the same lock the generic StatusCompleted write it can replace would run
// under — so a hook that needs to do more work hands off to its own
// goroutine and re-acquires opMu there, exactly as the StatusCompleted
// write it replaces would have run under opMu. See [Manager.onAutoMerge]
// for the concrete case and the invariant that handoff preserves.
// Returning true means the hook has taken over the terminal transition and
// the generic path must not also record StatusCompleted; false falls
// through to the default StatusCompleted write.
type runCompleteHook func(ctx context.Context, c *threadControl, st Thread, resultText string) (handled bool)

// recoverHook lets an overlay reclassify an entity during recover before
// the generic active-status sweep runs. Returning handled=true means the
// hook has already decided st's fate (recording a terminal status of its
// own if needed) and the generic path should leave it alone.
type recoverHook func(ctx context.Context, st Thread) (handled bool, err error)

// deliveryResolver finds where a delegation's terminal completion should
// be delivered once handleRunComplete (or, for a thread's auto-merge
// flow, Manager.deliverMergeOutcome) has a final st to report: the App
// whose AgentCoordinator owns the parent session's completion inbox, and
// the parent session id within it. ok=false means there is nowhere to
// deliver — no overlay claims st.Kind, or the specific entity has no
// resolvable parent (a task always has one; a thread may not, since
// CreateArgs.ParentSessionID is optional) — and is a clean no-op, not an
// error: st's own terminal status is already recorded and pollable
// regardless of whether anything is listening for it.
//
// handle is the entity's own workspace handle, passed through for a kind
// (a task) whose delivery target is reachable through it (ParentAppSpawner
// means handle.App() *is* the parent App). A kind that delivers into a
// different App than its own workspace (a thread, whose LocalSpawner
// gives it a wholly separate App/database) resolves independently of
// handle and may receive nil — see Manager.resolveDeliveryTarget.
type deliveryResolver func(ctx context.Context, handle Handle, st Thread) (target *app.App, parentSessionID string, ok bool)

// Logging note: every slog call in this package (and in the completion/
// continuation path it feeds in internal/agent) is restricted to ids,
// kinds, statuses, and counts. Never log a delegation's Goal or its
// ResultSummary/Error text — that content is the user's own work, and logs
// outlive the session it was generated in.

// lifecycle is the generic delegation-lifecycle machinery shared by every
// kind of background delegation this package drives: admission control,
// per-entity serialization, worker tracking, run dispatch, workspace
// release, and event plumbing. It has no notion of git worktrees or merge
// policy — those live in the onRunSuccess/onRecover hooks an overlay such
// as [Manager] supplies at construction. It also has no notion of a single
// Spawner: unlike store, which every kind shares, each entity carries its
// own Spawner in its runtimeState, since [Manager]'s threads and a
// [TaskManager]'s tasks are spawned (and released) differently even
// though both share this one lifecycle.
type lifecycle struct {
	store           Store
	onRunSuccess    runCompleteHook
	onRecover       recoverHook
	resolveDelivery deliveryResolver
	broker          *pubsub.Broker[Event]

	mu       sync.Mutex
	controls map[string]*threadControl
	closed   bool
	workers  sync.WaitGroup

	// changeCh is closed and replaced on every status-affecting event,
	// giving Wait a broadcast condition to select on without polling.
	changeMu sync.Mutex
	changeCh chan struct{}
}

// newLifecycle constructs a ready-to-use lifecycle backed by store.
// onRunSuccess and onRecover are optional (nil means no overlay: runs
// always rest at StatusCompleted, and recover never reclassifies beyond
// the generic active-status sweep). resolveDelivery is also optional
// (nil means nothing ever delivers), but [Manager] always supplies one
// that covers both kinds sharing this lifecycle — see
// [Manager.resolveDeliveryTarget]. Share one lifecycle between every
// delegation kind driven over the same store — see [Manager] and
// [TaskManager] — rather than constructing one per kind, or admission,
// recovery, and shutdown will each only ever see one kind.
func newLifecycle(store Store, onRunSuccess runCompleteHook, onRecover recoverHook, resolveDelivery deliveryResolver) *lifecycle {
	return &lifecycle{
		store:           store,
		onRunSuccess:    onRunSuccess,
		onRecover:       onRecover,
		resolveDelivery: resolveDelivery,
		broker:          pubsub.NewBroker[Event](),
		controls:        make(map[string]*threadControl),
		changeCh:        make(chan struct{}),
	}
}

// beginOp admits one mutating operation. closeAdmission stops admission and
// the caller then waits for this count, so no operation can attach a
// runtime after teardown.
func (l *lifecycle) beginOp() (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrManagerClosed
	}
	l.workers.Add(1)
	return l.workers.Done, nil
}

func (l *lifecycle) control(id string) *threadControl {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.controls[id]
	if c == nil {
		c = &threadControl{}
		l.controls[id] = c
	}
	return c
}

func (l *lifecycle) existingControl(id string) *threadControl {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.controls[id]
}

// goWorker is called only while an admitted operation or existing worker is
// counted. Consequently its Add cannot race a shutdown's Wait after zero.
func (l *lifecycle) goWorker(fn func()) {
	l.workers.Add(1)
	go func() { defer l.workers.Done(); fn() }()
}

// subscribe returns a per-caller channel of lifecycle events.
func (l *lifecycle) subscribe(ctx context.Context) <-chan pubsub.Event[Event] {
	return l.broker.Subscribe(ctx)
}

// publish emits a lifecycle event and wakes any Wait callers blocked on a
// status change.
func (l *lifecycle) publish(t EventType, st Thread) {
	l.broker.Publish(pubsub.UpdatedEvent, Event{Type: t, Thread: st})
	l.notifyChange()
}

// setStatus is the shared SetStatus + publish helper used by every status
// transition.
func (l *lifecycle) setStatus(ctx context.Context, id string, status Status, errText, resultSummary string, completedAt int64) (Thread, error) {
	st, err := l.store.SetStatus(ctx, id, SetStatusParams{
		Status:        status,
		Error:         errText,
		ResultSummary: resultSummary,
		CompletedAt:   completedAt,
	})
	if err != nil {
		return Thread{}, err
	}
	l.publish(EventStatusChanged, st)
	return st, nil
}

func (l *lifecycle) notifyChange() {
	l.changeMu.Lock()
	close(l.changeCh)
	l.changeCh = make(chan struct{})
	l.changeMu.Unlock()
}

func (l *lifecycle) waitChan() chan struct{} {
	l.changeMu.Lock()
	defer l.changeMu.Unlock()
	return l.changeCh
}

// closeAdmission stops beginOp from admitting further work. Callers join
// the returned count (via a subsequent wait) to know once every admitted
// operation and worker goroutine it may have spawned has finished.
func (l *lifecycle) closeAdmission() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// wait blocks until every admitted operation and worker goroutine has
// finished.
func (l *lifecycle) wait() {
	l.workers.Wait()
}

// snapshotControls returns a copy of every control known so far, letting
// callers iterate and tear entities down without holding the map lock
// across per-entity I/O.
func (l *lifecycle) snapshotControls() map[string]*threadControl {
	l.mu.Lock()
	defer l.mu.Unlock()
	controls := make(map[string]*threadControl, len(l.controls))
	for id, c := range l.controls {
		controls[id] = c
	}
	return controls
}

// startRun registers handle as the running workspace for id and starts its
// RunComplete watcher goroutine, then dispatches prompt against sessionID
// as a fire-and-forget run whose completion this lifecycle picks up via
// handleRunComplete. The subscription is established synchronously, before
// startRun returns: callers ([Manager.Create], [Manager.Send],
// [TaskManager.Create]) dispatch the run right after startRun returns, and
// a RunComplete published before the subscription is registered would
// otherwise be silently missed by the broker's fan-out.
//
// ctx is the long-lived background context the run and the watcher
// goroutine are bound to (Manager's and TaskManager's own m.ctx/t.ctx, not
// a caller's short-lived request context) — cancelling it is what lets a
// shared shutdown reach every kind's in-flight work. spawner is recorded
// on the resulting runtime so it, not any other kind's Spawner, is what
// eventually releases handle.
//
// Callers must hold the entity's opMu: installing c.runtime is a lifecycle
// transition, and without the lock a concurrent teardown could observe "no
// runtime", delete the entity, and leave this runtime stranded on one that
// no longer exists.
func (l *lifecycle) startRun(ctx context.Context, handle Handle, spawner Spawner, id, sessionID, prompt string) {
	rt := l.installRuntime(ctx, handle, spawner, id)
	c := l.control(id)
	runID := uuid.NewString()
	c.mu.Lock()
	rt.runID = runID
	c.mu.Unlock()

	// Reserve acceptance before dispatch so cancellation cannot leave a run
	// unaccounted for between goroutine scheduling and coordinator admission.
	accept := handle.App().AgentCoordinator.BeginAccepted(sessionID)
	l.goWorker(func() {
		if _, err := handle.App().AgentCoordinator.RunAccepted(agent.WithRunID(ctx, runID), accept, sessionID, prompt); err != nil {
			slog.Error("thread: agent run returned an error", "session_id", sessionID, "error", err)
			// backend.runAgent documents this fallback for pre-execution
			// failures. Local coordinators do not provide that wrapper.
			l.handleRunComplete(ctx, id, notify.RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
		}
	})
}

// installRuntime binds handle to id as the entity's live workspace and
// starts the RunComplete watcher for it, without dispatching any run. The
// returned runtime carries an empty runID: nothing is in flight yet, and
// handleRunComplete ignores completions that do not match the current
// runID, so a stray event cannot tear the workspace down.
//
// This is the state an idle entity rests in — a live workspace with no
// run of its own in flight — and the first half of what startRun does.
// Callers must hold the entity's opMu, for the reason startRun documents.
func (l *lifecycle) installRuntime(ctx context.Context, handle Handle, spawner Spawner, id string) *runtimeState {
	c := l.control(id)
	watchCtx, cancel := context.WithCancel(ctx)
	sub := handle.App().RunCompletions().Subscribe(watchCtx)
	rt := &runtimeState{handle: handle, spawner: spawner, watchCancel: cancel}
	c.mu.Lock()
	c.runtime = rt
	c.mu.Unlock()

	l.goWorker(func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				l.handleRunComplete(ctx, id, ev.Payload)
			}
		}
	})
	return rt
}

// send dispatches message into id's session, respawning its workspace
// first if it is not currently live. It is the generic body behind
// [Manager.Send] and [TaskManager.Send]: neither the "queue into a live
// runtime" branch nor the "respawn, then dispatch" branch has any
// git-specific or task-specific logic in it — only spawnPath (the
// worktree path for a thread, "" for a task, since [ParentAppSpawner]
// ignores it) and spawner differ between the two callers.
//
// ctx is the caller's own request context, used for the status writes
// and to release an early-abandoned respawn; bgCtx is the long-lived
// background context (Manager's or TaskManager's own ctx field) the
// dispatched run and its watcher goroutine are bound to, and the one
// checked for the caller's manager having started shutting down
// concurrently with this call — mirroring [lifecycle.startRun]'s same
// split.
//
// Callers must hold no locks: send acquires id's own opMu for its
// duration, the same admission ordering [Manager.Send] always used.
func (l *lifecycle) send(ctx, bgCtx context.Context, id string, spawner Spawner, spawnPath, sessionID, message string) error {
	c := l.control(id)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	removed := c.removed
	c.mu.Unlock()
	if removed {
		return fmt.Errorf("thread: %q has been removed", id)
	}

	if rt != nil {
		// The workspace is live: either a run is in flight, or the entity
		// is idle (created without a goal, or reactivated). Both take the
		// same path — dispatch the message as its own RunID-bearing turn
		// (the dispatcher gives every RunID-bearing queued prompt its own
		// turn and terminal RunComplete) and hand workspace ownership to
		// it: rt.runID is advanced under c.mu, so any in-flight run's
		// completion no longer matches in handleRunComplete and cannot
		// release the workspace out from under the queued turn. For an
		// idle entity rt.runID was empty, so there is no earlier run to
		// displace.
		if _, err := l.setStatus(ctx, id, StatusRunning, "", "", 0); err != nil {
			return err
		}
		runID := uuid.NewString()
		c.mu.Lock()
		rt.runID = runID
		c.mu.Unlock()
		l.goWorker(func() {
			if _, err := rt.handle.App().AgentCoordinator.Run(agent.WithRunID(bgCtx, runID), sessionID, message); err != nil {
				slog.Error("thread: queued agent run returned an error", "session_id", sessionID, "error", err)
				// Mirror startRun's fallback for pre-execution failures so
				// the workspace is not stranded on a run that never
				// published its own RunComplete.
				l.handleRunComplete(bgCtx, id, notify.RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
			}
		})
		return nil
	}

	handle, err := spawner.Spawn(bgCtx, spawnPath)
	if err != nil {
		return fmt.Errorf("thread: respawn workspace: %w", err)
	}
	if err := bgCtx.Err(); err != nil {
		_ = spawner.Release(context.Background(), handle.ID())
		return err
	}
	// This call owns the freshly spawned handle until startRun installs it
	// as the shared runtime; release it on every earlier exit.
	owned := true
	defer func() {
		if owned {
			_ = spawner.Release(ctx, handle.ID())
		}
	}()

	if _, err := l.setStatus(ctx, id, StatusRunning, "", "", 0); err != nil {
		return err
	}
	l.startRun(bgCtx, handle, spawner, id, sessionID, message)
	owned = false // Ownership transferred to the shared runtime state.
	return nil
}

// handleRunComplete is the generic reaction to a RunComplete notification
// for entity id: it matches the completion to the run currently owning the
// entity's workspace, releases the workspace, and records the terminal
// outcome — interrupted on cancellation, failed on error, or (when
// onRunSuccess is unset, or declines to handle it) completed with
// rc.Text. Anything beyond that reaction — Manager's auto-merge follow-up
// is the example — is onRunSuccess's job.
//
// Once a terminal status is actually recorded, this also delivers to the
// entity's parent session (see deliverCompletion) — for every kind
// resolveDelivery covers. The one deliberate exception is a thread whose
// successful run onRunSuccess (Manager's auto-merge overlay) takes over:
// that returns before reaching the delivery call below, because an
// auto-merge thread's useful terminal event is the merge outcome, not the
// run finishing mid-flight — see Manager.onAutoMerge and
// Manager.deliverMergeOutcome, which deliver that event once mergeAttempt
// concludes instead.
func (l *lifecycle) handleRunComplete(ctx context.Context, id string, rc notify.RunComplete) {
	c := l.existingControl(id)
	if c == nil {
		return
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	if rt == nil || rc.RunID == "" || rc.RunID != rt.runID {
		c.mu.Unlock()
		return
	}
	c.runtime = nil
	depth := c.depth
	c.mu.Unlock()
	rt.watchCancel()
	if err := rt.spawner.Release(ctx, rt.handle.ID()); err != nil {
		slog.Error("thread: release completed workspace failed", "thread", id, "error", err)
	}
	st, err := l.store.Get(ctx, id)
	if err != nil {
		return
	}
	// Only react to the session this entity currently owns, and only
	// while a run is actually in flight: Remove or a completed merge can
	// race a straggling RunComplete from a run that no longer matters.
	if rc.SessionID != st.SessionID || st.Status != StatusRunning {
		return
	}

	var finalSt Thread
	switch {
	case rc.Cancelled:
		finalSt, err = l.setStatus(ctx, id, StatusInterrupted, "", "", 0)
		if err != nil {
			slog.Error("thread: recording run cancellation failed", "thread", id, "error", err)
			return
		}
	case rc.Error != "":
		finalSt, err = l.setStatus(ctx, id, StatusFailed, rc.Error, "", 0)
		if err != nil {
			slog.Error("thread: recording run failure failed", "thread", id, "error", err)
			return
		}
	default:
		if l.onRunSuccess != nil && l.onRunSuccess(ctx, c, st, rc.Text) {
			return
		}
		finalSt, err = l.setStatus(ctx, id, StatusCompleted, "", rc.Text, time.Now().Unix())
		if err != nil {
			slog.Error("thread: recording run completion failed", "thread", id, "error", err)
			return
		}
	}

	slog.Info("Delegation reached terminal status", "id", id, "kind", finalSt.Kind, "status", finalSt.Status)
	l.deliverCompletion(ctx, rt.handle, finalSt, depth)
}

// deliverCompletion pushes st (a delegation that just reached a terminal
// status) into its parent session's completion inbox, so the parent's
// next step sees the outcome ahead of any steering message queued at the
// same time — see agent.TaskCompletion and runTurn.prepareStep.
//
// Where to deliver is entirely resolveDelivery's call — see
// Manager.resolveDeliveryTarget for how a task (which shares its
// parent's own App via ParentAppSpawner) and a thread (which spawns a
// wholly separate one via LocalSpawner) resolve differently. ok=false
// (no resolver configured, no overlay claims st.Kind, or no resolvable
// parent) is a clean no-op: st's own terminal status is already recorded
// and still pollable regardless of whether anything is listening for it.
//
// depth is the cascade depth the creating turn stamped on this entity at
// Create (threadControl.depth, read by the caller before releasing
// c.mu) — carried onto the event so an auto-woken continuation can run
// one level deeper, and refuse to cascade further once the hard limit
// is reached (see agent.TaskCompletion.Depth and the "agent" tool's
// background mode). Always 0 for a thread today: the cascade limiter
// only ever applies to tasks created through the "agent" tool.
func (l *lifecycle) deliverCompletion(ctx context.Context, handle Handle, st Thread, depth int) {
	if l.resolveDelivery == nil {
		return
	}
	target, parentSessionID, ok := l.resolveDelivery(ctx, handle, st)
	if !ok {
		slog.Info("Delivery skipped: no resolvable parent", "id", st.ID, "kind", st.Kind)
		return
	}
	if target == nil || target.AgentCoordinator == nil {
		slog.Info("Delivery skipped: no delivery target", "id", st.ID, "kind", st.Kind)
		return
	}
	target.AgentCoordinator.DeliverTaskCompletion(ctx, parentSessionID, agent.TaskCompletion{
		DelegationID:   st.ID,
		Kind:           string(st.Kind),
		Name:           st.Name,
		Goal:           st.Goal,
		Status:         string(st.Status),
		ChildSessionID: st.SessionID,
		ResultText:     st.ResultSummary,
		Error:          st.Error,
		Depth:          depth,
	})
}

// recover reconciles store state against reality after a process restart:
// entities left in an active status (their goroutines are gone with the
// old process) become interrupted. Before that generic sweep runs,
// onRecover (if set) gets a chance to reclassify st itself — Manager's
// overlay uses it to fail entities whose worktree has vanished from disk,
// a check the generic path has no business making.
//
// This sweeps every delegation kind (ListAll), not just threads: it is
// the generic path, and a kind-scoped listing here would leave any other
// kind's interrupted runs silently unreconciled after a restart — exactly
// the "left displayed as running forever" state recovery exists to
// prevent.
func (l *lifecycle) recover(ctx context.Context) error {
	threads, err := l.store.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, st := range threads {
		if l.onRecover != nil {
			handled, err := l.onRecover(ctx, st)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
		}
		if st.Status.Active() {
			if _, err := l.setStatus(ctx, st.ID, StatusInterrupted, "", "", 0); err != nil {
				return err
			}
		}
	}
	return nil
}
