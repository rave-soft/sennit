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

// terminalBookkeepingTimeout bounds the detached context terminal
// bookkeeping runs on. It's a backstop against a wedged store, not a
// budget — shutdown joins the worker goroutine running it, so it must
// eventually return.
const terminalBookkeepingTimeout = 15 * time.Second

// detachForTerminalWork strips ctx of its cancellation and bounds it by
// terminalBookkeepingTimeout, for work that must land regardless of what
// happened to the context that asked for it — recording a terminal
// status, telling the parent, releasing a workspace. Values (the
// delegation tag, session id) are kept; only the cancellation is dropped.
func detachForTerminalWork(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalBookkeepingTimeout)
}

// runtimeState tracks the in-memory bookkeeping for an entity whose
// workspace is currently spawned: the handle to release, the Spawner
// that produced it (kept per-runtime, not per-lifecycle, since threads
// and tasks share one lifecycle but use different Spawners — releasing
// via the wrong one can leak a workspace or tear down a task's parent
// App), and the cancel func for its RunComplete watcher goroutine.
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
	// while delegations of its own were still in flight — not finished,
	// waiting; its own completion inbox wakes it to carry on (see
	// handleRunComplete's park branch). While set there is no run id to
	// match against, so handleRunComplete matches on parkedSession instead.
	awaitingDelegations bool
	// parkedSession is the identity a parked entity is matched on instead
	// of a run id. The ordinary session check runs only after the
	// workspace has been released, which is too late here: a parked
	// entity must ignore every other session's completions without
	// tearing itself down.
	parkedSession string
}

// threadControl is permanent while an entity is known to the lifecycle. opMu
// serializes lifecycle operations for one entity without serializing
// unrelated entities or holding the lifecycle map lock across I/O.
type threadControl struct {
	opMu    sync.Mutex
	mu      sync.Mutex
	runtime *runtimeState
	removed bool
	// depth is the background-delegation cascade depth stamped at Create
	// (see TaskManager.Create/TaskCreateArgs.Depth); handleRunComplete reads
	// it back to compute an auto-woken continuation's depth.
	depth int
	// parentSessionID is an in-memory admission-check cache, stamped once
	// at Create and never touched again — used by
	// TaskManager.checkActiveCaps for a cheap in-memory read under createMu.
	// It need not survive a restart (Recover sweeps every active task to
	// interrupted). The durable source of truth is Delegation.ParentSessionID,
	// read directly by resolveDeliveryTarget.
	parentSessionID string
	// reports counts the terminal completions already delivered for this
	// delegation. task_send can reactivate a finished task, so the same
	// delegation may report more than once; PriorReports on the event lets
	// the reader tell a repeat from the first.
	//
	// In memory only: it describes one conversation as it's happening. A
	// report delivered after a restart says "first" again, which is correct
	// for a process whose context holds none of the earlier ones either.
	reports int
}

// ErrManagerClosed is returned by mutating manager operations once shutdown
// has started.
var ErrManagerClosed = errors.New("thread: manager is closed")

// ErrNotFound is returned when no delegation matches the id or name a
// caller asked for — including one that already merged and was deleted
// along with its worktree and branch, not only a name that never existed.
var ErrNotFound = errors.New("thread: no such thread")

// runCompleteHook lets an overlay intervene when a run finishes
// successfully, before the generic lifecycle rests the entity at
// StatusCompleted. Called with the entity's opMu held; a hook that needs
// more work hands off to its own goroutine and re-acquires opMu there
// (see [Manager.onAutoMerge]). Returning true means the hook took over
// the terminal transition and the generic StatusCompleted write must not
// also run; false falls through to it.
type runCompleteHook func(ctx context.Context, c *threadControl, st Thread, resultText string) (handled bool)

// recoverHook lets an overlay reclassify an entity during recover before
// the generic active-status sweep runs. Returning handled=true means the
// hook has already decided st's fate (recording a terminal status of its
// own if needed) and the generic path should leave it alone.
type recoverHook func(ctx context.Context, st Thread) (handled bool, err error)

// deliveryResolver finds where a delegation's terminal completion
// should be delivered: the App owning the parent session's completion
// inbox, and the parent session id within it. ok=false (no overlay
// claims st.Kind, or no resolvable parent) is a clean no-op — st's own
// terminal status is already recorded regardless of whether anything is
// listening.
//
// handle is passed through for a kind (a task) whose delivery target is
// reachable through it (handle.Workspace() *is* the parent workspace via
// threadspawn's ParentAppSpawner); a thread resolves independently of
// handle via its own LocalSpawner and may receive nil — see
// Manager.resolveDeliveryTarget.
type deliveryResolver func(ctx context.Context, handle Handle, st Thread) (target Workspace, parentSessionID string, ok bool)

// Logging note: every slog call in this package (and in the completion/
// continuation path it feeds in internal/agent) is restricted to ids,
// kinds, statuses, and counts. Never log a delegation's Goal or its
// ResultSummary/Error text — that content is the user's own work, and logs
// outlive the session it was generated in.

// lifecycle is the generic delegation-lifecycle machinery shared by
// every kind of background delegation this package drives: admission
// control, per-entity serialization, worker tracking, run dispatch,
// workspace release, and event plumbing. It has no notion of git
// worktrees or merge policy — those live in the onRunSuccess/onRecover
// hooks an overlay such as [Manager] supplies. Each entity carries its
// own Spawner in runtimeState rather than the lifecycle holding one,
// since [Manager]'s threads and [TaskManager]'s tasks are spawned
// differently despite sharing this lifecycle.
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

	// parentApp is the workspace this lifecycle's delegations belong to.
	// A thread runs in an isolated App whose events nobody outside it sees
	// unless the user has drilled in, so permission and question traffic
	// raised there is relayed here (see forwardPermissions and
	// forwardQuestions). Nil in tests without one; forwarding is then
	// skipped.
	parentApp Workspace
}

// newLifecycle constructs a ready-to-use lifecycle backed by store.
// onRunSuccess and onRecover are optional (nil means no overlay).
// resolveDelivery is also optional but [Manager] always supplies one
// covering both kinds — see [Manager.resolveDeliveryTarget]. Share one
// lifecycle between every delegation kind driven over the same store
// (see [Manager] and [TaskManager]) rather than one per kind, or
// admission, recovery, and shutdown will each only see one kind.
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

// beginControlledCreate is the shared opening move of Manager.Create
// and TaskManager.Create, once store.Create has made id resolvable: it
// locks c.opMu (so a concurrent Remove either ran first or waits for
// this create's runtime) and stashes the parent/depth link.
//
// c.opMu is returned locked — the caller must defer c.opMu.Unlock().
// removed reports whether a racing Remove already tore the entity down;
// the caller must then fail fast rather than build a worktree or spawn a
// workspace nothing will ever reach.
func (l *lifecycle) beginControlledCreate(id, parentSessionID string, depth int) (c *threadControl, removed bool) {
	c = l.control(id)
	c.opMu.Lock()
	c.mu.Lock()
	removed = c.removed
	c.depth = depth
	c.parentSessionID = parentSessionID
	c.mu.Unlock()
	return c, removed
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

func (l *lifecycle) wait() {
	l.workers.Wait()
}

// snapshotControls copies known controls so callers can iterate and
// tear entities down without holding the map lock across per-entity I/O.
func (l *lifecycle) snapshotControls() map[string]*threadControl {
	l.mu.Lock()
	defer l.mu.Unlock()
	controls := make(map[string]*threadControl, len(l.controls))
	for id, c := range l.controls {
		controls[id] = c
	}
	return controls
}

// startRun registers handle as the running workspace for id, starts its
// RunComplete watcher, then dispatches prompt against sessionID as a
// fire-and-forget run whose completion handleRunComplete picks up. The
// watcher subscription is established before startRun returns, since
// callers ([Manager.Create], [Manager.Send], [TaskManager.Create])
// dispatch right after — a RunComplete published before the subscription
// exists would otherwise be missed.
//
// ctx is the long-lived background context (Manager's/TaskManager's own
// ctx, not a caller's request context) so a shared shutdown can reach
// this run; spawner is recorded on the runtime so it, not any other
// kind's Spawner, releases handle.
//
// Callers must hold the entity's opMu: installing c.runtime without it
// risks a concurrent teardown observing "no runtime" and deleting the
// entity out from under this runtime.
func (l *lifecycle) startRun(ctx context.Context, handle Handle, spawner Spawner, id, sessionID, prompt string) {
	ctx = l.withDelegation(ctx, id)
	rt := l.installRuntime(ctx, handle, spawner, id)
	c := l.control(id)
	runID := uuid.NewString()
	c.mu.Lock()
	rt.runID = runID
	rt.awaitingDelegations = false
	c.mu.Unlock()

	// Reserve acceptance before dispatch so a cancellation between
	// scheduling and coordinator admission cannot leave the run
	// unaccounted for. Coordinator() may be nil (no agent configured);
	// dispatching into it would panic in a worker goroutine, so this
	// routes through handleRunComplete like any other pre-execution
	// failure — via goWorker, since startRun's caller still holds c.opMu
	// and handleRunComplete is not reentrant on it.
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
// starts its RunComplete watcher, without dispatching a run — the state
// an idle entity rests in, and the first half of what startRun does. The
// returned runtime's runID is empty, so a stray completion cannot match
// it. Callers must hold the entity's opMu, as startRun documents.
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
	l.forwardQuestions(watchCtx, handle)
	return rt
}

// steer dispatches the person's own message into a live delegation's
// session as a steering follow-up: if a turn is already in flight the
// message folds into its next step; if idle it starts its own run under
// runID, exactly as an agent's send does.
//
// Only the person gets this: a delegation's turn can sit inside a
// sub-agent call for minutes, so a course correction read only after
// that has corrected nothing — but the same interruption from another
// agent is one agent derailing another's work, which is why
// [SenderAgent] keeps queueing.
//
// The two outcomes need different bookkeeping and cannot be probed for
// afterwards without racing the turn itself, so the decision is taken
// inside the coordinator's own dispatch mutex and reported back through
// the steering hook — see steerAwait for how the wait stays safe against
// handleRunComplete.
func (l *lifecycle) steer(bgCtx context.Context, c *threadControl, rt *runtimeState, coord Coordinator, id, sessionID, msg, runID string, attachments []Attachment, disp SendDisposition) (SendDisposition, error) {
	decided, failed := l.steerDispatch(bgCtx, coord, id, sessionID, msg, runID, attachments)
	return l.steerAwait(bgCtx, c, rt, id, sessionID, runID, disp, decided, failed)
}

// steerDispatch reserves acceptance and dispatches the steering call on
// its own goroutine, reporting the coordinator's dispatch decision on
// decided and any RunAccepted error on failed (both buffered by one so
// the goroutine never blocks). coord is passed in already known
// non-nil, since BeginAccepted would panic on a nil re-read here.
func (l *lifecycle) steerDispatch(bgCtx context.Context, coord Coordinator, id, sessionID, msg, runID string, attachments []Attachment) (decided chan DispatchOutcome, failed chan error) {
	decided = make(chan DispatchOutcome, 1)
	failed = make(chan error, 1)
	var ranOwn atomic.Bool
	steerCtx := WithSteering(WithRunID(bgCtx, runID), func(outcome DispatchOutcome) {
		ranOwn.Store(outcome == DispatchRan)
		select {
		case decided <- outcome:
		default:
		}
	})
	// Reserved before dispatch, same reason as startRun: a cancel arriving
	// before coordinator admission must resolve to a definite answer,
	// which is what makes DispatchCancelled reachable at all.
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
		// Same fallback as the queued dispatch above, but only for a
		// message that became a run of its own: a folded or cancelled
		// dispatch never owned runID, so synthesizing a completion for it
		// would match nothing or tear down a workspace whose turn is
		// still running.
		if ranOwn.Load() {
			l.handleRunComplete(bgCtx, id, RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
		}
	})
	return decided, failed
}

// steerApplyDecision reacts to the coordinator's dispatch outcome, shared
// by both branches steerAwait may reach it from — a decision and a
// dispatch error can become ready in the same instant, and Go picks at
// random between ready cases, so both funnel through here.
func (l *lifecycle) steerApplyDecision(bgCtx context.Context, c *threadControl, rt *runtimeState, id, sessionID, runID string, outcome DispatchOutcome) (SendDisposition, error) {
	switch outcome {
	case DispatchFolded:
		// Folded into the turn in flight: that turn's completion is
		// still the one this entity ends on, so leave rt.runID alone.
		return SendDisposition{Steered: true}, nil
	case DispatchCancelled:
		// A cancel got here first: nothing ran or folded, so nothing is
		// owed an owner. The caller already moved the delegation to
		// running before dispatching, and no run exists to move it back,
		// so rest it here instead.
		l.restIdleAfterPersonTurn(bgCtx, c, rt, id, RunComplete{SessionID: sessionID})
		return SendDisposition{}, nil
	}
	// It became the active turn and owns the workspace from here. Still
	// under opMu, so the run it displaced (if any) hasn't been reacted to
	// yet. Marked as the person's: it ends by resting at idle with its
	// workspace intact, not by merging — see handleRunComplete.
	c.mu.Lock()
	rt.runID = runID
	rt.person = true
	rt.awaitingDelegations = false
	c.mu.Unlock()
	return SendDisposition{}, nil
}

// steerAwait waits for steerDispatch's outcome — or its RunAccepted
// error — and reacts via steerApplyDecision, all while still holding
// opMu (send holds it across this call), which is what keeps
// handleRunComplete from acting on the older run's completion meanwhile.
func (l *lifecycle) steerAwait(bgCtx context.Context, c *threadControl, rt *runtimeState, id, sessionID, runID string, disp SendDisposition, decided chan DispatchOutcome, failed chan error) (SendDisposition, error) {
	select {
	case outcome := <-decided:
		return l.steerApplyDecision(bgCtx, c, rt, id, sessionID, runID, outcome)
	case err := <-failed:
		// A decision may have landed in the same instant the dispatch
		// returned; it is the more specific answer, so take it first.
		select {
		case outcome := <-decided:
			return l.steerApplyDecision(bgCtx, c, rt, id, sessionID, runID, outcome)
		default:
		}
		// The dispatch failed before reaching a decision: nothing was
		// queued and no run started, so there is nothing to own but the
		// error.
		if err != nil {
			// No run exists to move the delegation back to idle, so
			// without this it sits at running for the rest of the
			// process — same reason the cancelled branch rests it.
			l.restIdleAfterPersonTurn(bgCtx, c, rt, id, RunComplete{SessionID: sessionID})
			return SendDisposition{}, err
		}
		// Returned cleanly without reporting a decision — not a state any
		// coordinator here produces; report what was read before dispatch.
		return disp, nil
	}
}

// withDelegation tags ctx with id's identity so permission requests raised
// by its run are attributed to it rather than the user's visible turn.
// Applied to every dispatch path (startRun, send) so the parent can label
// a forwarded prompt and route the answer back.
func (l *lifecycle) withDelegation(ctx context.Context, id string) context.Context {
	st, err := l.store.Get(ctx, id)
	if err != nil {
		// Best-effort: losing the label degrades a prompt, refusing the
		// dispatch would lose the work. A caller that already stamped ctx
		// itself (TaskManager.Create does) is left untouched.
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
// first if not live. It is the generic body behind [Manager.Send] and
// [TaskManager.Send]; only spawnPath and spawner differ between callers.
//
// ctx is the caller's own request context (status writes, releasing an
// early-abandoned respawn); bgCtx is the long-lived background context
// the dispatched run and its watcher are bound to — mirroring
// [lifecycle.startRun]'s split.
//
// Callers must hold no locks: send acquires id's own opMu for its
// duration.
//
// from decides how the message is persisted and whether it may fold into
// a turn already running — see [Sender] and the steering branch below.
//
// The returned [SendDisposition] reports which branch was taken; only
// Steered is decided by the coordinator, and queue depth is read before
// dispatch, describing the queue being joined rather than already joined.
//
// beforeDispatch, if non-nil, runs right before the run is dispatched — in
// both branches: on the live handle when the workspace is already up, and
// on the freshly spawned one after a respawn. Placed before dispatch, not
// after send returns, so whatever it installs (Manager.Send uses it to
// re-register the parent link and its auto-approval grant — see
// registerThreadParent) is in place before the dispatched turn can raise
// its first permission request. TaskManager.Send uses it to re-register
// its own parent link for the same reason; the auto-approval half does
// not apply there, since a task shares its parent's App and so its
// permission service outlives any runtime release.
func (l *lifecycle) send(ctx, bgCtx context.Context, id string, spawner Spawner, spawnPath, sessionID, msg string, from Sender, attachments []Attachment, beforeDispatch func(handle Handle)) (SendDisposition, error) {
	// Tag bgCtx so coordinator.run persists this dispatch's message with
	// Origin agent.OriginAgent, not the default message.OriginPerson — a
	// thread_send/task_send follow-up wasn't typed by the person, though
	// it remains an ordinary message.User turn. Both branches below read
	// bgCtx. A [SenderPerson] send is the case this doesn't apply to: the
	// person typed it into their own session.
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
		// is idle. An agent's send dispatches the message as its own
		// RunID-bearing turn and hands workspace ownership to it —
		// rt.runID advances under c.mu so an in-flight run's completion no
		// longer matches in handleRunComplete. The person's own send does
		// not: see steer.
		//
		// Read the coordinator once, up front: it's documented as possibly
		// nil, and resolving it before the status is set to StatusRunning
		// avoids discovering a nil coordinator with the entity already
		// marked running and no run left to clear it.
		coord := rt.handle.Workspace().Coordinator()
		if coord == nil {
			// See startRun: nil is a documented possibility, and a panic
			// in a worker goroutine takes the process with it.
			slog.Error("Queued agent run has no coordinator to dispatch to", "component", "thread", "session_id", sessionID)
			return SendDisposition{}, errors.New("thread: workspace has no agent coordinator")
		}
		if beforeDispatch != nil {
			beforeDispatch(rt.handle)
		}
		var disp SendDisposition
		if busy, ahead := coord.SessionQueue(sessionID); busy {
			disp = SendDisposition{Queued: true, Ahead: ahead}
		}
		if _, err := l.setStatus(ctx, id, StatusRunning, "", "", 0); err != nil {
			return SendDisposition{}, err
		}
		runID := uuid.NewString()
		if from == SenderPerson {
			return l.steer(bgCtx, c, rt, coord, id, sessionID, msg, runID, attachments, disp)
		}
		c.mu.Lock()
		rt.runID = runID
		rt.awaitingDelegations = false
		c.mu.Unlock()
		// Reserved before dispatch, same reason as startRun and steer: a
		// follow-up can be cancelled the moment after it's sent.
		accept := coord.BeginAccepted(sessionID)
		l.goWorker(func() {
			if err := coord.RunAccepted(WithRunID(bgCtx, runID), accept, sessionID, msg, nil); err != nil {
				closeAccepted(accept)
				slog.Error("Queued agent run returned an error", "component", "thread", "session_id", sessionID, "error", err)
				// Mirrors startRun's fallback so the workspace isn't
				// stranded on a run that never published its own RunComplete.
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
		_ = spawner.Release(context.Background(), handle.ID()) // ok: detached - bgCtx is already done; this is the cleanup for that
		return SendDisposition{}, err
	}
	// rb unwinds the freshly spawned handle if this returns before startRun
	// installs it as the shared runtime — see [unwinder].
	var rb unwinder
	defer rb.unwind()
	rb.push(func() {
		// detachForTerminalWork, not ctx: setStatus below failed because
		// ctx was already cancelled, and a Release built on that same
		// dead ctx would fail too, leaking the handle for the life of the
		// process.
		releaseCtx, cancel := detachForTerminalWork(ctx)
		defer cancel()
		_ = spawner.Release(releaseCtx, handle.ID())
	})

	if _, err := l.setStatus(ctx, id, StatusRunning, "", "", 0); err != nil {
		return SendDisposition{}, err
	}
	if beforeDispatch != nil {
		beforeDispatch(handle)
	}
	l.startRun(bgCtx, handle, spawner, id, sessionID, msg)
	rb.commit()
	// A workspace that had to be respawned has no turn in flight, so this
	// message is the one that runs, never a queued follow-up.
	return SendDisposition{Resumed: true}, nil
}

// releaseRuntime is the teardown sequence shared by every path that
// retires a live runtime: stop the watch loop, cancel the dispatched run
// (if any), optionally cancel the session on its own coordinator, and
// release the handle through its own spawner. rt.runCancel is nil for
// every startRun-started runtime (only startFactoryRun sets it), so
// calling it here is a no-op for those callers.
//
// Callers differ in ctx (own vs. detached), whether cancelSession is true
// (Manager.Remove only on force; handleRunComplete's own call never
// cancels), and how errors are logged — this owns only the sequence.
func releaseRuntime(ctx context.Context, rt *runtimeState, sessionID string, cancelSession bool) error {
	rt.watchCancel()
	if rt.runCancel != nil {
		rt.runCancel()
	}
	if cancelSession {
		if a := rt.handle.Workspace(); a != nil && a.Coordinator() != nil {
			a.Coordinator().Cancel(sessionID)
		}
	}
	return rt.spawner.Release(ctx, rt.handle.ID())
}

// cancel is the generic body behind [TaskManager.Cancel] and
// [Manager.Cancel]: it stops st's in-flight run and rests it at
// [StatusCancelled] with reason recorded as its Error. reason defaults to
// "cancelled" when empty.
//
// A no-op if st has no live runtime: an already-terminal entity cannot be
// "more cancelled", and overwriting a real outcome would destroy it.
//
// Cancelling reaches only st's own session, never the whole coordinator —
// load-bearing for a task, whose App is its parent's (anything broader
// would reach the user's own foreground turn too).
//
// Callers must have already resolved and kind-checked st; any
// kind-specific refusal is the caller's job, decided before reaching here.
func (l *lifecycle) cancel(ctx context.Context, st Thread, reason string) error {
	// Same detach as handleRunComplete, for the same reason: a cancel
	// arrives on contexts a cancel itself kills, and on a dead one the
	// finalizing transaction never opens, leaving the row at running and
	// the parent never told.
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

	if err := releaseRuntime(ctx, rt, st.SessionID, true); err != nil {
		slog.Error("Failed to release cancelled workspace", "component", "thread", "id", st.ID, "kind", st.Kind, "error", err)
	}

	if reason == "" {
		reason = "cancelled"
	}
	var final Thread
	var err error
	c.mu.Lock()
	depth := c.depth
	c.mu.Unlock()
	if st.Kind == KindTask {
		final, err = l.finalizeTask(ctx, st, StatusCancelled, reason, "", 0, depth)
	} else {
		final, err = l.setStatus(ctx, st.ID, StatusCancelled, reason, "", 0)
	}
	if err != nil {
		return fmt.Errorf("cancel finalization: %w", err)
	}
	if final.ID == "" {
		return nil
	}
	slog.Info("Delegation reached terminal status", "id", st.ID, "kind", st.Kind, "status", StatusCancelled)
	// Told to the session waiting on it, exactly as every other terminal
	// status (see finalizeRunComplete): otherwise the parent waits on a
	// report that never comes, and the canceller isn't necessarily the
	// parent.
	//
	// rt.handle rather than nil: a task with no ParentApp resolves its
	// target through the handle (see Manager.resolveDeliveryTarget),
	// still in hand here though c.runtime was already cleared above.
	l.deliverStoredCompletion(ctx, rt.handle, final, depth)
	return nil
}

// handleRunComplete is the generic reaction to a RunComplete notification
// for entity id: match it to the run currently owning the workspace,
// release the workspace, and record the terminal outcome — interrupted,
// failed, or completed, unless onRunSuccess takes over.
//
// Once a terminal status is recorded, this delivers to the entity's
// parent session (see deliverStoredCompletion), except a thread whose
// successful run onRunSuccess (Manager's auto-merge overlay) takes over:
// that returns before the delivery call, since an auto-merge thread's
// useful terminal event is the merge outcome — see Manager.onAutoMerge
// and Manager.deliverMergeOutcome.
func (l *lifecycle) handleRunComplete(ctx context.Context, id string, rc RunComplete) {
	// See detachForTerminalWork: the contexts reaching here are exactly
	// the ones a cancellation kills, so an interrupted run's own ctx
	// would otherwise fail store.Get below, leaving the status stale and
	// the parent never told.
	//
	// followUpCtx keeps the caller's own lifetime for the one branch that
	// outlives this call: onRunSuccess hands an auto-merge thread to a
	// worker goroutine that captures its context, so the bounded ctx
	// below would cancel the merge the moment this function returns.
	followUpCtx := ctx
	ctx, cancel := detachForTerminalWork(ctx)
	defer cancel()

	c := l.existingControl(id)
	if c == nil {
		return
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	rt, matched := l.matchRunComplete(ctx, c, id, rc)
	if !matched {
		return
	}

	if l.parkIfAwaitingDelegations(ctx, c, rt, id, rc) {
		return
	}

	l.finalizeRunComplete(ctx, followUpCtx, c, rt, id, rc)
}

// matchRunComplete is handleRunComplete's first step: reads c's current
// runtime, matches rc against the run it owns, and — for a hand-driven
// person turn ending — rests the entity back at idle itself. matched=false
// (no runtime, a stale run/session id, or an already-settled person turn)
// means handleRunComplete must return immediately.
//
// Called with c.opMu already held.
func (l *lifecycle) matchRunComplete(ctx context.Context, c *threadControl, id string, rc RunComplete) (rt *runtimeState, matched bool) {
	c.mu.Lock()
	rt = c.runtime
	if rt == nil {
		c.mu.Unlock()
		return nil, false
	}
	// A parked delegation (see the park branch below) has no run id to
	// match; its session is the identity checked below instead.
	if !rt.awaitingDelegations && (rc.RunID == "" || rc.RunID != rt.runID) {
		c.mu.Unlock()
		return nil, false
	}
	if rt.awaitingDelegations && rc.SessionID != rt.parkedSession {
		c.mu.Unlock()
		return nil, false
	}
	if rt.person {
		// A hand-driven turn ends where it started: back to idle with
		// the workspace still live, since the person is likely to type
		// again. Releasing or merging here would pull the workspace out
		// from under them; a thread revived by hand merges when they say
		// so, not when they stop typing.
		rt.runID = ""
		rt.person = false
		c.mu.Unlock()
		l.restIdleAfterPersonTurn(ctx, c, rt, id, rc)
		return nil, false
	}
	c.mu.Unlock()
	return rt, true
}

// parkIfAwaitingDelegations is handleRunComplete's second step: a
// delegation that ended its turn to wait on delegations of its own is not
// finished — finalizing here would tell the parent it answered before its
// own children have. Park instead: the workspace stays live, the row
// stays running, nothing is delivered, and the continuation that resumes
// the session re-enters this same function.
//
// Only for a run that ended cleanly — an error or cancel is this
// delegation's own outcome. Returns true when it parked, in which case
// handleRunComplete must return without finalizing.
func (l *lifecycle) parkIfAwaitingDelegations(ctx context.Context, c *threadControl, rt *runtimeState, id string, rc RunComplete) bool {
	if rc.Cancelled || rc.Error != "" || !l.awaitsOwnDelegations(ctx, rc.SessionID) {
		return false
	}
	c.mu.Lock()
	if c.runtime == rt {
		rt.runID = ""
		rt.awaitingDelegations = true
		rt.parkedSession = rc.SessionID
	}
	c.mu.Unlock()
	slog.Info("Delegation is waiting on delegations of its own; not finalizing yet",
		"component", "thread", "id", id, "session_id", rc.SessionID)
	return true
}

// finalizeRunComplete is handleRunComplete's last step: release rt's
// workspace, load the entity's current row, and — if the completion still
// applies to the session this entity currently owns — record its
// terminal status and deliver it to the parent. followUpCtx is passed
// through only to onRunSuccess (see handleRunComplete's doc for why).
// Called with c.opMu already held.
func (l *lifecycle) finalizeRunComplete(ctx, followUpCtx context.Context, c *threadControl, rt *runtimeState, id string, rc RunComplete) {
	c.mu.Lock()
	c.runtime = nil
	depth := c.depth
	c.mu.Unlock()
	if err := releaseRuntime(ctx, rt, "", false); err != nil {
		slog.Error("Failed to release completed workspace", "component", "thread", "thread", id, "error", err)
	}
	st, err := l.store.Get(ctx, id)
	if err != nil {
		// The runtime is already torn down, so nothing else retries this
		// transition; give a couple of short retries before giving up
		// loudly.
		for attempt := 0; attempt < 2 && err != nil; attempt++ {
			time.Sleep(50 * time.Millisecond)
			st, err = l.store.Get(ctx, id)
		}
		if err != nil {
			slog.Error("Failed to load thread after run completion; status left stale", "component", "thread", "thread", id, "error", err)
			return
		}
	}
	// Only react to the session this entity currently owns while a run is
	// in flight: Remove or a completed merge can race a straggling
	// RunComplete from a run that no longer matters.
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
	l.deliverStoredCompletion(ctx, rt.handle, finalSt, depth)
}

// restIdleAfterPersonTurn records the end of a hand-driven turn: back to
// [StatusIdle], with that turn's own outcome as Error (empty on success,
// clearing a stale failure). ResultSummary and CompletedAt are preserved
// — they describe the delegation's own goal run, not this turn. Nothing
// is delivered to the parent; it already has the goal's outcome.
//
// A parked entity (rt.awaitingDelegations) is left alone: its row is
// deliberately still StatusRunning while its own delegations are
// outstanding, and moving it to idle here would strand it there once its
// own completion finally arrives — finalizeRunComplete only reacts while
// the row still reads StatusRunning.
func (l *lifecycle) restIdleAfterPersonTurn(ctx context.Context, c *threadControl, rt *runtimeState, id string, rc RunComplete) {
	c.mu.Lock()
	parked := rt.awaitingDelegations
	c.mu.Unlock()
	if parked {
		return
	}
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
// own not yet accounted for: still running, or terminal with a result
// not yet handed to this session (CompletionPending is the durable
// outbox bit for the latter). A store that cannot answer is treated as
// "nothing outstanding", so a failed read cannot change finalization
// behavior.
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

// deliverStoredCompletion pushes st (a delegation that just reached a
// terminal status) into its parent session's completion inbox, ahead of
// any steering message queued at the same time — see agent.TaskCompletion
// and runTurn.prepareStep.
//
// Where to deliver is resolveDelivery's call — see
// Manager.resolveDeliveryTarget for how a task (parent's own App) and a
// thread (its own separate App) resolve differently. ok=false is a clean
// no-op: st's own terminal status is already recorded regardless.
//
// depth is the cascade depth stamped on this entity at Create, carried
// onto the event so an auto-woken continuation can run one level deeper
// and refuse to cascade past the hard limit (see
// agent.TaskCompletion.Depth). Always 0 for a thread today.
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
	c := l.control(st.ID)
	c.mu.Lock()
	priorReports := c.reports
	c.reports++
	c.mu.Unlock()
	target.Coordinator().DeliverTaskCompletion(ctx, parentSessionID, TaskCompletion{
		DelegationID:   st.ID,
		PriorReports:   priorReports,
		Kind:           string(st.Kind),
		Name:           st.Name,
		Goal:           st.Goal,
		Status:         string(st.Status),
		ChildSessionID: st.SessionID,
		ResultText:     st.ResultSummary,
		Error:          st.Error,
		Depth:          depth,
		// Stamped once here, the single place every delivery path builds
		// a TaskCompletion, so prepareStep's log can report delivery
		// latency without a second clock reading elsewhere.
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
// entities left in an active status become interrupted, since their
// goroutines are gone with the old process. onRecover (if set) can
// reclassify st first — Manager's overlay fails entities whose worktree
// has vanished from disk.
//
// Sweeps every delegation kind (ListAll), not just threads, or another
// kind's interrupted runs would go unreconciled after a restart.
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
// ("yolo") state to every delegation workspace live right now.
//
// A thread runs in an isolated App with its own permission service, so it
// only learns the parent's bypass state when spawned; without this,
// toggling yolo in the main window leaves a running thread on whatever
// state it started with. Turning it off matters most — the thread would
// keep skipping prompts the user just asked to see again.
//
// A task's handle wraps the parent's own App, so setting the flag through
// it is the same idempotent write already made — harmless to allow rather
// than special-case.
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
func closeAccepted(accept *AcceptedRun) {
	if accept != nil {
		accept.Close()
	}
}
