package thread

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// runtimeState tracks the in-memory bookkeeping for an entity whose
// workspace is currently spawned: the handle to release on removal, the
// Spawner that produced it (kept per-runtime rather than per-lifecycle,
// since threads and tasks share one lifecycle but use different Spawners —
// releasing via the wrong one is exactly the kind of mistake that would
// either leak a workspace or, for a task, tear down its parent App), and
// the cancel function for its RunComplete watcher goroutine.
// terminalBookkeepingTimeout bounds the detached context
// handleRunComplete does its terminal work on. The work is local store
// I/O and a worktree release, so this is a backstop against a wedged
// store rather than a budget anything normally approaches — and it has
// to be a backstop, since shutdown joins the worker goroutine running it.
const terminalBookkeepingTimeout = 15 * time.Second

// detachForTerminalWork strips ctx of its cancellation and bounds it by
// terminalBookkeepingTimeout, for work that has to land whatever happened
// to the context that asked for it: recording a terminal status, telling
// the parent, releasing a workspace.
//
// Both callers are handed contexts that a cancellation kills — the run's
// own, and a shutting-down app's — which is precisely the case they most
// need to work in. Values (the delegation tag, the session id) are kept;
// only the cancellation is dropped.
func detachForTerminalWork(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalBookkeepingTimeout)
}

type runtimeState struct {
	handle      Handle
	spawner     Spawner
	watchCancel context.CancelFunc
	runCancel   context.CancelFunc
	runID       string
	// person marks runID as a turn the person is driving by hand, rather
	// than one this package dispatched on a delegation's behalf. The two
	// end very differently — see handleRunComplete — so the flag is set
	// and cleared together with runID, under the same mutex.
	person bool
	// awaitingDelegations marks a delegation whose dispatched run ended
	// while delegations of its own were still in flight: it is not
	// finished, it is waiting, and its agent will be woken by its own
	// completion inbox to carry on (see handleRunComplete's park branch).
	//
	// While it is set the entity has no run id to match against — the
	// turn that resumes it is an auto-woken continuation, which carries
	// none — so handleRunComplete accepts that session's next completion
	// on the strength of this flag instead. parkedSession is the session
	// that identity rests on: the ordinary path's session check happens
	// only after the workspace has been released, which is too late for a
	// parked entity that must ignore every other session's completions
	// without tearing itself down.
	awaitingDelegations bool
	parkedSession       string
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
	// parentSessionID is an in-memory admission-check cache only now,
	// stamped once at Create (see Manager.Create/TaskManager.Create) and
	// never touched again. It exists purely to give
	// TaskManager.checkActiveCaps a cheap in-memory read across every
	// currently-active task's control under createMu, with no
	// restart-survival requirement (a process restart sweeps every active
	// task to interrupted via Recover, so there is nothing "active" left
	// in a fresh process for this cache to be wrong about until a new
	// Create repopulates it). The durable source of truth for delivery/ask
	// resolution is Delegation.ParentSessionID on the row, read directly
	// by resolveDeliveryTarget — not this field.
	parentSessionID string
}

// ErrManagerClosed is returned by mutating manager operations once shutdown
// has started.
var ErrManagerClosed = errors.New("thread: manager is closed")

// ErrNotFound is returned when no delegation matches the id or name a
// caller asked for. A thread that merged is deleted along with its
// worktree and branch, so this is the ordinary answer for one that has
// already landed, not only for a name that never existed — callers that
// can say so should (see the thread_status tool).
var ErrNotFound = errors.New("thread: no such thread")

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
// (a task) whose delivery target is reachable through it (threadspawn's
// ParentAppSpawner means handle.Workspace() *is* the parent workspace). A
// kind that delivers into a different workspace than its own (a thread,
// whose LocalSpawner gives it a wholly separate App/database) resolves
// independently of handle and may receive nil — see
// Manager.resolveDeliveryTarget.
type deliveryResolver func(ctx context.Context, handle Handle, st Thread) (target Workspace, parentSessionID string, ok bool)

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

	// parentApp is the workspace this lifecycle's delegations belong
	// to, and the event stream a user is actually watching. A thread runs
	// in an isolated App whose events nobody outside it sees unless the
	// user has drilled into that thread, so permission traffic raised
	// there is relayed here — see forwardPermissions. Nil in tests that
	// construct a lifecycle without one; forwarding is then skipped.
	parentApp Workspace
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
func newLifecycle(store Store, onRunSuccess runCompleteHook, onRecover recoverHook, resolveDelivery deliveryResolver, parentApp Workspace) *lifecycle {
	return &lifecycle{
		store:           store,
		onRunSuccess:    onRunSuccess,
		onRecover:       onRecover,
		resolveDelivery: resolveDelivery,
		broker:          pubsub.NewBroker[Event](),
		controls:        make(map[string]*threadControl),
		changeCh:        make(chan struct{}),
		parentApp:       parentApp,
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
	ctx = l.withDelegation(ctx, id)
	rt := l.installRuntime(ctx, handle, spawner, id)
	c := l.control(id)
	runID := uuid.NewString()
	c.mu.Lock()
	rt.runID = runID
	rt.awaitingDelegations = false
	c.mu.Unlock()

	// Reserve acceptance before dispatch so cancellation cannot leave a run
	// unaccounted for between goroutine scheduling and coordinator admission.
	// Coordinator() is documented as possibly nil (a workspace with no
	// agent configured). Dispatching into it would panic in a worker
	// goroutine and take the process with it, so this ends the run the
	// way any other pre-execution failure ends it — via handleRunComplete
	// on a worker goroutine, exactly like the RunAccepted failure path
	// below. handleRunComplete takes c.opMu itself, and startRun's own
	// caller is still holding that lock: calling it inline here would
	// deadlock against that non-reentrant mutex.
	coord := handle.Workspace().Coordinator()
	if coord == nil {
		l.goWorker(func() {
			l.handleRunComplete(ctx, id, RunComplete{
				SessionID: sessionID,
				RunID:     runID,
				Error:     "workspace has no agent coordinator",
			})
		})
		return
	}
	accept := coord.BeginAccepted(sessionID)
	l.goWorker(func() {
		if err := coord.RunAccepted(WithRunID(ctx, runID), accept, sessionID, prompt, nil); err != nil {
			closeAccepted(accept)
			slog.Error("Agent run returned an error", "component", "thread", "session_id", sessionID, "error", err)
			// AgentDispatcher.run documents this fallback for pre-execution
			// failures. This direct RunAccepted call bypasses that wrapper,
			// so the thread lifecycle provides its own fallback here.
			l.handleRunComplete(ctx, id, RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
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
func (l *lifecycle) startFactoryRun(ctx context.Context, handle Handle, spawner Spawner, id, sessionID string, factory TaskRunFactory) {
	rt := l.installRuntime(ctx, handle, spawner, id)
	c := l.control(id)
	runID := uuid.NewString()
	runCtx, runCancel := context.WithCancel(ctx)
	c.mu.Lock()
	rt.runID = runID
	rt.runCancel = runCancel
	rt.awaitingDelegations = false
	c.mu.Unlock()

	l.goWorker(func() {
		run, cleanup, err := factory(runCtx, sessionID)
		if cleanup != nil {
			defer cleanup()
		}
		var result TaskRunResult
		if err == nil {
			if run == nil {
				err = errors.New("task run factory returned no runner")
			} else {
				result, err = run(runCtx)
			}
		}
		rc := RunComplete{SessionID: sessionID, RunID: runID, Text: result.Text}
		if err != nil {
			rc.Error = err.Error()
			rc.Cancelled = errors.Is(err, context.Canceled)
		}
		l.handleRunComplete(runCtx, id, rc)
	})
}

func (l *lifecycle) installRuntime(ctx context.Context, handle Handle, spawner Spawner, id string) *runtimeState {
	c := l.control(id)
	watchCtx, cancel := context.WithCancel(ctx)
	sub := handle.Workspace().RunCompletions().Subscribe(watchCtx)
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
	l.forwardPermissions(watchCtx, handle)
	return rt
}

// forwardPermissions relays a delegation workspace's permission traffic
// into the parent workspace's event stream, for as long as that workspace
// is live.
//
// A thread runs in an isolated app.App with its own permission service and
// its own event broker. The user's TUI is subscribed to the parent
// workspace, so without this relay a prompt raised inside a thread reaches
// nobody: permission.Service.Request blocks on its response channel with
// no timeout, and the thread hangs there forever with no visible sign of
// why. Anything that needs approval - bash, above all - stops the thread
// dead.
//
// Requests carry their delegation's identity (see withDelegation), which
// is what lets the parent both label the prompt and route the answer back
// to the service that is actually waiting for it - see
// [Manager.PermissionsFor].
//
// Bound to the runtime's watch context, so the relay stops when the
// workspace is released. A task needs none of this: its handle wraps the
// parent's own App, so its prompts are already on the stream the user is
// watching.
func (l *lifecycle) forwardPermissions(ctx context.Context, handle Handle) {
	if l.parentApp == nil {
		return
	}
	a := handle.Workspace()
	if a == nil || a == l.parentApp {
		return
	}
	forwardInto(ctx, l, a.Permissions().Subscribe)
	forwardInto(ctx, l, a.Permissions().SubscribeNotifications)

	// A request already waiting when the relay starts was published
	// before anything was listening, and a request is announced exactly
	// once - so without this it would sit there unanswerable, which is
	// the failure this whole relay exists to prevent. Reachable whenever
	// a workspace is re-installed under a delegation that still has a
	// prompt outstanding.
	if req, ok := a.Permissions().ActiveRequest(); ok {
		l.parentApp.SendEvent(pubsub.Event[permission.PermissionRequest]{
			Type:    pubsub.CreatedEvent,
			Payload: req,
		})
	}
}

// steer dispatches the person's own message into a live delegation's
// session as a steering follow-up: if a turn is already in flight the
// message folds into that turn's next step instead of waiting for it to
// end, and if the session is idle it starts its own run under runID,
// exactly as an agent's send always does.
//
// Only the person gets this. A delegation's turn can sit inside a
// sub-agent call for many minutes, and a course correction that is read
// only after those minutes has corrected nothing — but the same
// interruption from another agent is not a correction, it is one agent
// derailing another's work, which is why [SenderAgent] keeps queueing.
//
// The two outcomes need different bookkeeping, and which one happened
// cannot be probed for afterwards without racing the very turn it is
// about: a folded message extends the run already in flight, so that
// run's own completion stays the entity's terminal event and rt.runID
// must not move, while a message that started a run must take ownership
// before the displaced run's completion is processed. So the decision is
// taken inside the coordinator's own dispatch mutex and reported back
// through the steering hook, and this waits for it while still holding
// opMu — which is what keeps handleRunComplete (which takes opMu too)
// from acting on the older run's completion in between.
func (l *lifecycle) steer(bgCtx context.Context, c *threadControl, rt *runtimeState, id, sessionID, msg, runID string, attachments []Attachment, disp SendDisposition) (SendDisposition, error) {
	var (
		decided = make(chan DispatchOutcome, 1)
		failed  = make(chan error, 1)
		ranOwn  atomic.Bool
	)
	steerCtx := WithSteering(WithRunID(bgCtx, runID), func(outcome DispatchOutcome) {
		ranOwn.Store(outcome == DispatchRan)
		select {
		case decided <- outcome:
		default:
		}
	})
	coord := rt.handle.Workspace().Coordinator()
	// Reserved before dispatch for the reason startRun reserves: this call
	// reaches the coordinator on a goroutine, and a cancel arriving in
	// between must resolve to a definite answer rather than leaving a run
	// unaccounted for. The reservation is what makes DispatchCancelled
	// reachable at all.
	accept := coord.BeginAccepted(sessionID)
	l.goWorker(func() {
		err := coord.RunAccepted(steerCtx, accept, sessionID, msg, attachments)
		if err != nil {
			closeAccepted(accept)
		}
		select {
		case failed <- err:
		default:
		}
		if err == nil {
			return
		}
		slog.Error("Steered agent run returned an error", "component", "thread", "session_id", sessionID, "error", err)
		// Same fallback as the queued dispatch above, and for the same
		// reason — but only for a message that actually became a run of
		// its own. A folded or cancelled dispatch never owned runID (the
		// coordinator drops a folded call's RunID, and a cancelled one
		// publishes its own terminal event), so synthesizing a completion
		// for it would either match nothing or, worse, tear down a
		// workspace whose turn is still running.
		if ranOwn.Load() {
			l.handleRunComplete(bgCtx, id, RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
		}
	})

	// applyDecision is the reaction to a dispatch outcome, shared by the
	// two branches below: a decision and a dispatch error can become
	// ready in the same instant, and Go picks between ready cases at
	// random. Reached through the error branch, that would have thrown
	// away a decision that had in fact been made.
	applyDecision := func(outcome DispatchOutcome) (SendDisposition, error) {
		switch outcome {
		case DispatchFolded:
			// Folded into the turn in flight: that turn's completion is
			// still the one this entity ends on, so leave rt.runID alone.
			return SendDisposition{Steered: true}, nil
		case DispatchCancelled:
			// A cancel got here first. Nothing ran, nothing folded, and
			// nothing is owed an owner — which is exactly what reserving
			// acceptance buys: this is a definite answer, not a run that
			// may or may not still appear.
			//
			// The caller moved the delegation to running before dispatching
			// (it had no way to know this would happen), and since no run
			// exists, no completion will ever move it back. Rest it here
			// instead: a live workspace with nothing in flight is idle.
			l.restIdleAfterPersonTurn(bgCtx, id, RunComplete{SessionID: sessionID})
			return SendDisposition{}, nil
		}
		// It became the active turn, so it owns the workspace from here.
		// Still under opMu, so the run it displaced (if any) has not been
		// reacted to yet, and once it is its RunID will no longer match.
		// Marked as the person's: it ends by resting the delegation at
		// idle with its workspace intact, not by settling and merging it
		// — see handleRunComplete.
		c.mu.Lock()
		rt.runID = runID
		rt.person = true
		rt.awaitingDelegations = false
		c.mu.Unlock()
		return SendDisposition{}, nil
	}

	select {
	case outcome := <-decided:
		return applyDecision(outcome)
	case err := <-failed:
		// A decision may have landed in the same instant the dispatch
		// returned; it is the more specific answer, so take it first.
		select {
		case outcome := <-decided:
			return applyDecision(outcome)
		default:
		}
		// The dispatch failed before reaching a decision (a coordinator
		// that never admitted the call). Nothing was queued and no run
		// started, so there is nothing to own and nothing to report but
		// the error.
		if err != nil {
			// The caller moved the delegation to running before
			// dispatching. No run exists to move it back, so without this
			// the thread sat at running for the rest of the process — the
			// same reason the cancelled branch rests it.
			l.restIdleAfterPersonTurn(bgCtx, id, RunComplete{SessionID: sessionID})
			return SendDisposition{}, err
		}
		// Returned cleanly without ever reporting a decision. Not a state
		// any coordinator in this system produces; report what was read
		// before the dispatch rather than inventing an outcome.
		return disp, nil
	}
}

// forwardInto pumps one of a delegation workspace's event sources onto the
// parent App's fan-in, republishing each event unchanged so a subscriber
// cannot tell it apart from one the parent raised itself - which is the
// point: the TUI's permission handling should not need to know that the
// asking workspace is somewhere else.
//
// A free function rather than a method because Go has no generic methods.
func forwardInto[T any](ctx context.Context, l *lifecycle, subscribe func(context.Context) <-chan pubsub.Event[T]) {
	sub := subscribe(ctx)
	l.goWorker(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				l.parentApp.SendEvent(ev)
			}
		}
	})
}

// withDelegation tags ctx with id's identity so every permission request
// raised by its run is attributed to it rather than to the user's own
// visible turn. Applied to every dispatch path (see startRun and send), so
// no path can forget: the parent needs the tag both to label a forwarded
// prompt and to route the answer back to the right permission service.
func (l *lifecycle) withDelegation(ctx context.Context, id string) context.Context {
	st, err := l.store.Get(ctx, id)
	if err != nil {
		// Tagging is best-effort: losing the label degrades a prompt,
		// refusing the dispatch would lose the work. A caller that
		// already had the values may have stamped them itself before
		// calling in (TaskManager.Create does) — returning ctx untouched
		// leaves that stamp in place, which is the point of it.
		slog.Warn("Failed to tag run with its delegation", "component", "thread", "thread", id, "error", err)
		return ctx
	}
	return permission.WithDelegation(ctx, permission.DelegationRef{
		ID:   st.ID,
		Name: st.Name,
		Kind: string(st.Kind),
	})
}

// send dispatches message into id's session, respawning its workspace
// first if it is not currently live. It is the generic body behind
// [Manager.Send] and [TaskManager.Send]: neither the "queue into a live
// runtime" branch nor the "respawn, then dispatch" branch has any
// git-specific or task-specific logic in it — only spawnPath (the
// worktree path for a thread, "" for a task, since threadspawn's
// ParentAppSpawner ignores it) and spawner differ between the two callers.
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
//
// from decides both how the message is persisted and whether it may fold
// into a turn already running — see [Sender], and the steering branch
// below for what folding costs and buys.
//
// The returned [SendDisposition] describes which of the branches the
// message took and, when it was queued behind a turn already in flight,
// how many prompts were waiting ahead of it. Only Steered is decided by
// the coordinator itself; the rest is reporting only — no dispatch
// decision is made from it — and the queue depth is read before the
// dispatch, so it describes the queue the message is joining rather than
// the one it has already joined.
func (l *lifecycle) send(ctx, bgCtx context.Context, id string, spawner Spawner, spawnPath, sessionID, msg string, from Sender, attachments []Attachment) (SendDisposition, error) {
	// Tag bgCtx so coordinator.run persists this dispatch's user message
	// with Origin agent.OriginAgent instead of the default
	// message.OriginPerson — a thread_send/task_send follow-up was not
	// typed by the person, even though it is (and must remain) an
	// ordinary message.User turn like any other. Both branches below
	// (queue into the live runtime, and respawn-then-dispatch) read
	// bgCtx, so tagging it once here covers both.
	//
	// A [SenderPerson] send is exactly the case the tag does not describe:
	// the person typed it themselves, into the TUI's view of this
	// session, so it is persisted as their own words like any prompt they
	// type into their own session.
	if from == SenderAgent {
		bgCtx = WithAgentDispatch(bgCtx)
	}
	// Same reasoning for the delegation tag: both branches below dispatch
	// a run, and a prompt raised by either has to be attributable.
	bgCtx = l.withDelegation(bgCtx, id)

	c := l.control(id)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	removed := c.removed
	c.mu.Unlock()
	if removed {
		return SendDisposition{}, fmt.Errorf("thread: %q has been removed", id)
	}

	if rt != nil {
		// The workspace is live: either a run is in flight, or the entity
		// is idle (created without a goal, or reactivated). An agent's
		// send takes the same path for both — dispatch the message as its
		// own RunID-bearing turn (the dispatcher gives every RunID-bearing
		// queued prompt its own turn and terminal RunComplete) and hand
		// workspace ownership to it: rt.runID is advanced under c.mu, so
		// any in-flight run's completion no longer matches in
		// handleRunComplete and cannot release the workspace out from
		// under the queued turn. For an idle entity rt.runID was empty, so
		// there is no earlier run to displace. The person's own send does
		// not: see steer.
		//
		// Which of those two it is, is exactly what the sender needs told
		// back: read it now, before the dispatch below adds this message
		// to the queue it is describing.
		var disp SendDisposition
		if coord := rt.handle.Workspace().Coordinator(); coord != nil {
			if busy, ahead := coord.SessionQueue(sessionID); busy {
				disp = SendDisposition{Queued: true, Ahead: ahead}
			}
		}
		if _, err := l.setStatus(ctx, id, StatusRunning, "", "", 0); err != nil {
			return SendDisposition{}, err
		}
		runID := uuid.NewString()
		if from == SenderPerson {
			return l.steer(bgCtx, c, rt, id, sessionID, msg, runID, attachments, disp)
		}
		c.mu.Lock()
		rt.runID = runID
		rt.awaitingDelegations = false
		c.mu.Unlock()
		// Reserved before dispatch, exactly as startRun and steer do: a
		// follow-up sent to a delegation can be cancelled the moment after
		// it is sent, and the reservation is what keeps that cancel from
		// landing in the gap between this goroutine being scheduled and
		// the coordinator admitting the call.
		coord := rt.handle.Workspace().Coordinator()
		if coord == nil {
			// See startRun: nil is a documented possibility, and a panic
			// in a worker goroutine takes the process with it.
			slog.Error("Queued agent run has no coordinator to dispatch to", "component", "thread", "session_id", sessionID)
			return SendDisposition{}, errors.New("thread: workspace has no agent coordinator")
		}
		accept := coord.BeginAccepted(sessionID)
		l.goWorker(func() {
			if err := coord.RunAccepted(WithRunID(bgCtx, runID), accept, sessionID, msg, nil); err != nil {
				closeAccepted(accept)
				slog.Error("Queued agent run returned an error", "component", "thread", "session_id", sessionID, "error", err)
				// Mirror startRun's fallback for pre-execution failures so
				// the workspace is not stranded on a run that never
				// published its own RunComplete.
				l.handleRunComplete(bgCtx, id, RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
			}
		})
		return disp, nil
	}

	handle, err := spawner.Spawn(bgCtx, spawnPath)
	if err != nil {
		return SendDisposition{}, fmt.Errorf("thread: respawn workspace: %w", err)
	}
	if err := bgCtx.Err(); err != nil {
		_ = spawner.Release(context.Background(), handle.ID())
		return SendDisposition{}, err
	}
	// This call owns the freshly spawned handle until startRun installs it
	// as the shared runtime; release it on every earlier exit.
	owned := true
	defer func() {
		if owned {
			// detachForTerminalWork, not ctx directly: this defer can fire
			// because setStatus below failed precisely because ctx was
			// already cancelled, and a Release built on that same dead ctx
			// would fail too, leaking the freshly spawned handle (and its
			// App/DB connection) for the life of the process.
			releaseCtx, cancel := detachForTerminalWork(ctx)
			defer cancel()
			_ = spawner.Release(releaseCtx, handle.ID())
		}
	}()

	if _, err := l.setStatus(ctx, id, StatusRunning, "", "", 0); err != nil {
		return SendDisposition{}, err
	}
	l.startRun(bgCtx, handle, spawner, id, sessionID, msg)
	owned = false // Ownership transferred to the shared runtime state.
	// A workspace that had to be respawned has no turn in flight, so this
	// message is the one that runs, never a queued follow-up.
	return SendDisposition{Resumed: true}, nil
}

// cancel is the generic body behind [TaskManager.Cancel] and
// [Manager.Cancel]: it stops st's in-flight run and rests it at
// [StatusCancelled] with reason recorded as its Error — a real status
// transition, not merely releasing the runtime and leaving whatever
// status happened to be last recorded. reason defaults to "cancelled"
// when empty.
//
// If st has no live runtime (already finished, or never started), this is
// a no-op: an already-terminal entity cannot be "more cancelled", and
// overwriting a real outcome (completed, failed, merged, ...) with
// "cancelled" would destroy it.
//
// Cancelling reaches only st's own session (AgentCoordinator.Cancel(st.
// SessionID)), never the whole coordinator. This is load-bearing for a
// task, whose App is its parent's — anything broader would reach the
// user's own foreground turn too — and merely redundant-but-harmless for
// a thread, whose isolated App has nothing else running in it regardless.
//
// Callers must have already resolved and kind-checked st via their own
// Get/resolve (TaskManager.Get rejects a thread's id and vice versa); any
// kind-specific refusal (Manager.Cancel's mid-merge check) is the
// caller's job, decided before reaching here — this method itself has no
// notion of Kind.
func (l *lifecycle) cancel(ctx context.Context, st Thread, reason string) error {
	// Same detach handleRunComplete does, for the same reason: everything
	// below is terminal bookkeeping, and the contexts a cancel arrives on
	// are exactly the ones a cancel kills — the run's own, or a shutting
	// down app's. On a dead one the finalizing transaction never opens
	// ("cancel finalization: beginning transaction: context canceled"),
	// so the row stays at running, the parent is never told its
	// delegation ended, and the workspace release is skipped too. The
	// deadline keeps a wedged store from holding shutdown open.
	ctx, releaseBookkeeping := detachForTerminalWork(ctx)
	defer releaseBookkeeping()

	c := l.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	c.runtime = nil
	c.mu.Unlock()

	if rt == nil {
		return nil
	}

	rt.watchCancel()
	if rt.runCancel != nil {
		rt.runCancel()
	}
	if a := rt.handle.Workspace(); a != nil && a.Coordinator() != nil {
		a.Coordinator().Cancel(st.SessionID)
	}
	if err := rt.spawner.Release(ctx, rt.handle.ID()); err != nil {
		slog.Error("Failed to release cancelled workspace", "component", "thread", "id", st.ID, "kind", st.Kind, "error", err)
	}

	if reason == "" {
		reason = "cancelled"
	}
	var final Thread
	var err error
	if st.Kind == KindTask {
		c.mu.Lock()
		depth := c.depth
		c.mu.Unlock()
		final, err = l.finalizeTask(ctx, st, StatusCancelled, reason, "", 0, depth)
	} else {
		final, err = l.setStatus(ctx, st.ID, StatusCancelled, reason, "", 0)
	}
	if err != nil {
		return fmt.Errorf("cancel finalization: %w", err)
	}
	if final.ID != "" {
		slog.Info("Delegation reached terminal status", "id", st.ID, "kind", st.Kind, "status", StatusCancelled)
	}
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
func (l *lifecycle) handleRunComplete(ctx context.Context, id string, rc RunComplete) {
	// Everything below is terminal bookkeeping — releasing the workspace,
	// recording the final status, telling the parent — and none of it may
	// run on a context the very event being handled has already canceled.
	// The contexts reaching here are the run's own and the manager's
	// long-lived one, and a cancellation is exactly what cancels those:
	// the run a person interrupted arrives here with err ==
	// context.Canceled and a dead ctx, so the store.Get failed, the status
	// was "left stale" at StatusRunning, and deliverCompletion was never
	// reached — the parent was never told its delegation had ended. Detach
	// so the bookkeeping still lands, with a deadline so a wedged store
	// cannot hold shutdown (which joins these workers) forever.
	//
	// followUpCtx keeps the caller's own lifetime for the one branch that
	// outlives this call: onRunSuccess hands an auto-merge thread to a
	// worker goroutine that captures the context it is given, so handing
	// it the bounded one below would cancel the merge the moment this
	// function returns.
	followUpCtx := ctx
	ctx, cancel := detachForTerminalWork(ctx)
	defer cancel()

	c := l.existingControl(id)
	if c == nil {
		return
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	if rt == nil {
		c.mu.Unlock()
		return
	}
	// A parked delegation (see the park branch below) has no run id to
	// match: what resumes it is an auto-woken continuation, which carries
	// none. Its session is the identity that still holds, and rc is
	// checked against it below, once the row is loaded.
	if !rt.awaitingDelegations && (rc.RunID == "" || rc.RunID != rt.runID) {
		c.mu.Unlock()
		return
	}
	if rt.awaitingDelegations && rc.SessionID != rt.parkedSession {
		c.mu.Unlock()
		return
	}
	if rt.person {
		// A turn the person drove by hand ends where it started: the
		// delegation goes back to idle with its workspace still live,
		// because the person is sitting in it and will very likely type
		// again. None of the terminal machinery applies — releasing the
		// workspace would pull it out from under them, and merging (for
		// an auto-policy thread) would fold and discard the worktree they
		// are still working in. A thread revived by hand is merged when
		// they say so, not when they stop typing.
		rt.runID = ""
		rt.person = false
		c.mu.Unlock()
		l.restIdleAfterPersonTurn(ctx, id, rc)
		return
	}
	c.mu.Unlock()

	// A delegation that ended its turn to wait for delegations of its own
	// is not finished — that is how an agent waits, and its own
	// completion inbox will wake it to carry on. Finalizing here told the
	// parent its delegation had answered, and a pipeline that starts a
	// reviewer the moment the developer "finishes" then ran the review
	// against work that was still being written.
	//
	// So park instead: the workspace stays live, the row stays running
	// (which is what task_result reports), and nothing is delivered. The
	// continuation that resumes the session ends in this same function,
	// and finalizes there — or parks again, for as many rounds as the
	// delegation needs.
	//
	// Only for a run that ended cleanly: an error or a cancel is this
	// delegation's own outcome, and no child's result changes it.
	if !rc.Cancelled && rc.Error == "" && l.awaitsOwnDelegations(ctx, rc.SessionID) {
		c.mu.Lock()
		if c.runtime == rt {
			rt.runID = ""
			rt.awaitingDelegations = true
			rt.parkedSession = rc.SessionID
		}
		c.mu.Unlock()
		slog.Info("Delegation is waiting on delegations of its own; not finalizing yet",
			"component", "thread", "id", id, "session_id", rc.SessionID)
		return
	}

	c.mu.Lock()
	c.runtime = nil
	depth := c.depth
	c.mu.Unlock()
	rt.watchCancel()
	if rt.runCancel != nil {
		rt.runCancel()
	}
	if err := rt.spawner.Release(ctx, rt.handle.ID()); err != nil {
		slog.Error("Failed to release completed workspace", "component", "thread", "thread", id, "error", err)
	}
	st, err := l.store.Get(ctx, id)
	if err != nil {
		// The runtime is already torn down at this point, so nothing else
		// will retry this terminal transition — a store hiccup here would
		// otherwise strand the row in StatusRunning until a restart. Give
		// a couple of short retries before giving up loudly.
		for attempt := 0; attempt < 2 && err != nil; attempt++ {
			time.Sleep(50 * time.Millisecond)
			st, err = l.store.Get(ctx, id)
		}
		if err != nil {
			slog.Error("Failed to load thread after run completion; status left stale", "component", "thread", "thread", id, "error", err)
			return
		}
	}
	// Only react to the session this entity currently owns, and only
	// while a run is actually in flight: Remove or a completed merge can
	// race a straggling RunComplete from a run that no longer matters.
	if rc.SessionID != st.SessionID || st.Status != StatusRunning {
		return
	}

	status, errText, result, completedAt := StatusCompleted, "", rc.Text, time.Now().Unix()
	switch {
	case rc.Cancelled:
		status, result, completedAt = StatusInterrupted, "", 0
	case rc.Error != "":
		status, errText, result, completedAt = StatusFailed, rc.Error, "", 0
	default:
		if l.onRunSuccess != nil && l.onRunSuccess(followUpCtx, c, st, rc.Text) {
			return
		}
	}

	var finalSt Thread
	if st.Kind == KindTask {
		finalSt, err = l.finalizeTask(ctx, st, status, errText, result, completedAt, depth)
	} else {
		finalSt, err = l.setStatus(ctx, id, status, errText, result, completedAt)
	}
	if err != nil {
		slog.Error("Failed to finalize delegation", "component", "thread", "id", id, "error", err)
		return
	}
	if finalSt.ID == "" {
		return
	}

	slog.Info("Delegation reached terminal status", "id", id, "kind", finalSt.Kind, "status", finalSt.Status)
	l.deliverCompletion(ctx, rt.handle, finalSt, depth)
}

// restIdleAfterPersonTurn records the end of a hand-driven turn: back to
// [StatusIdle], carrying whatever that turn itself produced as the
// delegation's Error (empty when it succeeded, which is what finally
// clears a stale failure the person has just worked past — the state the
// row describes is now the one they are looking at).
//
// ResultSummary and CompletedAt are preserved rather than rewritten: they
// describe the delegation's own goal run, and a person's turn is not a new
// answer to that goal. Nothing is delivered to the parent either — the
// parent asked for the goal's outcome and has long since been told it.
func (l *lifecycle) restIdleAfterPersonTurn(ctx context.Context, id string, rc RunComplete) {
	st, err := l.store.Get(ctx, id)
	if err != nil {
		return
	}
	// A completion for a session this entity no longer owns says nothing
	// about it — the same guard the dispatched path applies.
	if rc.SessionID != st.SessionID {
		return
	}
	if _, err := l.setStatus(ctx, id, StatusIdle, rc.Error, st.ResultSummary, st.CompletedAt); err != nil {
		slog.Error("Failed to record the end of a hand-driven turn", "component", "thread", "thread", id, "error", err)
	}
}

// deliverCompletion pushes st (a delegation that just reached a terminal
// status) into its parent session's completion inbox, so the parent's
// next step sees the outcome ahead of any steering message queued at the
// same time — see agent.TaskCompletion and runTurn.prepareStep.
//
// Where to deliver is entirely resolveDelivery's call — see
// Manager.resolveDeliveryTarget for how a task (which shares its
// parent's own App via threadspawn's ParentAppSpawner) and a thread
// (which spawns a wholly separate one via LocalSpawner) resolve
// differently. ok=false
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
func (l *lifecycle) finalizeTask(ctx context.Context, st Thread, status Status, errText, result string, completedAt int64, depth int) (Thread, error) {
	store, ok := l.store.(TaskFinalizationStore)
	if !ok {
		return l.setStatus(ctx, st.ID, status, errText, result, completedAt)
	}
	terminalAt := time.Now().UnixNano()
	final, won, err := store.FinalizeTask(ctx, st.ID, FinalizeTaskParams{
		Status: status, Error: errText, ResultSummary: result, CompletedAt: completedAt,
		CompletionDepth: depth, TerminalAt: terminalAt,
	})
	if err != nil {
		return Thread{}, err
	}
	if !won {
		return Thread{}, nil
	}
	l.publish(EventStatusChanged, final)
	return final, nil
}

// awaitsOwnDelegations reports whether sessionID has delegations of its
// own that are not yet accounted for: still running, or terminal with
// their result not yet handed to this session (the durable outbox bit —
// a child that finished a moment ago is a wake this session has coming,
// not an answer it already has).
//
// A store that cannot answer is treated as "nothing outstanding": the
// caller then finalizes exactly as it always did, which is the behaviour
// a failed read must not be allowed to change.
func (l *lifecycle) awaitsOwnDelegations(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	all, err := l.store.ListAll(ctx)
	if err != nil {
		slog.Warn("Could not check a delegation's own delegations before finalizing it",
			"component", "thread", "session_id", sessionID, "error", err)
		return false
	}
	for _, child := range all {
		if child.ParentSessionID != sessionID {
			continue
		}
		if child.Status == StatusRunning || child.CompletionPending {
			return true
		}
	}
	return false
}

func (l *lifecycle) deliverCompletion(ctx context.Context, handle Handle, st Thread, depth int) {
	l.deliverStoredCompletion(ctx, handle, st, depth)
}

func (l *lifecycle) deliverStoredCompletion(ctx context.Context, handle Handle, st Thread, depth int) {
	if l.resolveDelivery == nil {
		return
	}
	target, parentSessionID, ok := l.resolveDelivery(ctx, handle, st)
	if !ok {
		slog.Info("Delivery skipped: no resolvable parent", "id", st.ID, "kind", st.Kind)
		return
	}
	if target == nil || target.Coordinator() == nil {
		slog.Info("Delivery skipped: no delivery target", "id", st.ID, "kind", st.Kind)
		return
	}
	terminalAt := time.Now()
	if st.TerminalAt != 0 {
		terminalAt = time.Unix(0, st.TerminalAt)
	}
	target.Coordinator().DeliverTaskCompletion(ctx, parentSessionID, TaskCompletion{
		DelegationID:   st.ID,
		Kind:           string(st.Kind),
		Name:           st.Name,
		Goal:           st.Goal,
		Status:         string(st.Status),
		ChildSessionID: st.SessionID,
		ResultText:     st.ResultSummary,
		Error:          st.Error,
		Depth:          depth,
		// Stamped once, here — the single place every delivery path
		// (a run finishing, or a thread's auto-merge outcome) builds a
		// TaskCompletion — so prepareStep's own "Completion delivered"
		// log can report how long the completion sat before reaching
		// the model without a second clock reading anywhere else.
		TerminalAt: terminalAt,
		Acknowledge: func(ackCtx context.Context) error {
			if st.Kind != KindTask || !st.CompletionPending {
				return nil
			}
			store, ok := l.store.(TaskFinalizationStore)
			if !ok {
				return nil
			}
			return store.MarkTaskCompletionDelivered(ackCtx, st.ID)
		},
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
	if store, ok := l.store.(TaskFinalizationStore); ok {
		pending, err := store.ListPendingTaskCompletions(ctx)
		if err != nil {
			return err
		}
		for _, st := range pending {
			l.deliverStoredCompletion(ctx, nil, st, st.CompletionDepth)
		}
	}
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
			if st.Kind == KindTask {
				final, err := l.finalizeTask(ctx, st, StatusInterrupted, "", "", 0, st.CompletionDepth)
				if err != nil {
					return err
				}
				if final.ID != "" {
					l.deliverStoredCompletion(ctx, nil, final, final.CompletionDepth)
				}
				continue
			}
			if _, err := l.setStatus(ctx, st.ID, StatusInterrupted, "", "", 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// setPermissionsSkip applies the parent workspace's permission-bypass
// ("yolo") state to every delegation workspace that is live right now.
//
// A thread runs in an isolated app.App with its own permission service, so
// it only learns the parent's bypass state when it is spawned. Without
// this, toggling yolo in the main window leaves a running thread on
// whatever the state was when it started: turned on, the thread still
// blocks on a prompt nobody is positioned to answer; turned off, the
// thread keeps skipping prompts the user has just decided they want back.
// The second direction is the one with consequences, which is why this
// propagates in both.
//
// A task's handle wraps the parent's own App, so setting the flag through
// it is the same idempotent write the caller already made - harmless, and
// cheaper to allow than to special-case.
func (l *lifecycle) setPermissionsSkip(skip bool) {
	for _, c := range l.snapshotControls() {
		c.mu.Lock()
		rt := c.runtime
		c.mu.Unlock()
		if rt == nil {
			continue
		}
		rt.handle.Workspace().Permissions().SetSkipRequests(skip)
	}
}

// closeAccepted releases an acceptance reservation the coordinator never
// consumed. RunAccepted takes ownership of the handle once it admits the
// call, and Close is idempotent, so this is safe on every error path —
// and necessary on them: a dispatch that failed before admission left the
// reservation counted forever, which pins the session's dispatch state in
// memory and makes it look permanently mid-dispatch.
func closeAccepted(accept any) {
	if closer, ok := accept.(interface{ Close() }); ok {
		closer.Close()
	}
}
