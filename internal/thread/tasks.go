package thread

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/permission"
)

// maxActiveTasksPerWorkspace and maxActiveTasksPerParentTurn bound
// concurrent task delegations. Both are hard constants, not configuration:
// unlike options.background_agents (an explicit, permanent product
// choice), these are a defensive width limit, and a value someone could
// misconfigure upward would defeat the reason they exist.
//
// Every task runs inside the *same* parent App as the turn that created it
// (see threadspawn's ParentAppSpawner) — the same working directory, and the same
// permission.Service the visible turn itself uses. Permission prompts are
// answered one at a time, so beyond a small number, extra concurrent tasks
// do not get more done in parallel; they just queue behind the same
// permission gate and contend for the same files, while making the
// workspace harder to reason about. The cascade depth limit
// (maxTaskCascadeDepth, internal/agent) already bounds how *deep* a chain
// of delegations may run; these bound how *wide* it may get at any one
// level, which depth does nothing to prevent.
//
// maxActiveTasksPerWorkspace is the total across every parent turn in the
// workspace. maxActiveTasksPerParentTurn is deliberately smaller than that
// total — half — so one turn's fan-out can never claim the entire budget
// and starve a second turn (the user's own foreground turn, or another
// session) of the ability to delegate anything at all.
//
// Threads do not count toward either limit: a thread spawns its own
// isolated App with its own worktree (threadspawn's LocalSpawner), so it
// never touches
// the parent App's working directory or its permission.Service - neither
// of the two resources these limits protect. This is enforced simply by
// scope: the counts below are computed from List, which is already
// Kind-scoped to tasks.
const (
	maxActiveTasksPerWorkspace  = 4
	maxActiveTasksPerParentTurn = 2
)

// TaskCreateArgs holds the inputs to [TaskManager.Create].
type TaskCreateArgs struct {
	// Goal is the task's prompt, dispatched immediately: unlike a thread,
	// a task has no worktree to rest idle in, so there is no goal-less
	// path the way [CreateArgs.Goal] has for [Manager.Create].
	Goal string
	// ParentSessionID is the session the task's own child session nests
	// under, the same relationship [Manager.Create] establishes for a
	// thread's child session via Sessions.CreateTaskSession. Required: a
	// task with no parent is not a task, just an orphaned session.
	ParentSessionID string
	// Depth is the background-delegation cascade depth of the turn that
	// created this task (0 for a real user turn). It is carried
	// in-memory only (see threadControl.depth) — not persisted — since it
	// is only ever needed transiently, between this Create and the one
	// completion event this task ever produces, to compute the depth of
	// an auto-woken continuation (see agent.TaskCompletion.Depth).
	Depth int
	// SessionTitle and AgentID preserve specialized delegation history.
	SessionTitle string
	AgentID      string
	// SessionID, when set, is used verbatim as the child session's id
	// instead of a generated one. A delegation launched from a tool call
	// passes the "<messageID>$$<toolCallID>" identity the transcript
	// derives for that call, which is what makes the delegation openable
	// from the parent's chat; callers with no such identity leave it
	// empty and get a generated id.
	SessionID string
	// Factory, when set, owns preparation and execution instead of the coder
	// dispatcher. It is invoked asynchronously only after Create returns.
	Factory TaskRunFactory
}

// TaskRunResult is the terminal output of a specialized task run.
type TaskRunResult struct {
	Text string
}

// TaskRunFactory performs potentially blocking preparation. cleanup may be
// returned with an error and is called exactly once by the lifecycle.
type TaskRunFactory func(context.Context, string) (run func(context.Context) (TaskRunResult, error), cleanup func(), err error)

// TaskManager drives the task delegation kind: the same admission,
// per-entity serialization, run dispatch, status transitions, recovery,
// and shutdown as [Manager]'s threads, minus the git worktree/merge
// overlay. A task runs inside its parent workspace's own App instead of
// an isolated one it spawns — see threadspawn's ParentAppSpawner — which
// is the
// entire reason this kind exists: it starts cheaply where a thread
// cannot.
//
// TaskManager is deliberately a sibling of [Manager], not a method on it:
// Manager's public surface (List, Wait, Merge, and its CreateArgs'
// git-specific fields) is stated in terms of threads, and bolting a
// worktree-less kind onto it would either strain that surface or require
// every thread-only method to guard against a task ID. What TaskManager
// shares with Manager is the [lifecycle] underneath both — see
// [NewTaskManager].
type TaskManager struct {
	store    Store
	spawner  Spawner
	messages MessageService
	ctx      context.Context
	lc       *lifecycle

	// createMu serializes Create end to end (see Create's own comment on
	// it): the concurrency caps must check the active count and act on it
	// as one atomic step, or two Create calls racing each other could both
	// pass the check before either is counted and together exceed it.
	// Task creation is not a hot path, so serializing all of it - not just
	// the check - trades away nothing that matters for a correctness
	// guarantee that does.
	createMu sync.Mutex
}

// NewTaskManager constructs a TaskManager. lc and ctx must be an existing
// [Manager]'s own m.lc and m.ctx (both unexported, so this can only be
// called from within package thread), not freshly constructed ones: a
// separate lifecycle would give tasks their own controls map, worker
// group, and recovery sweep, splitting exactly the machinery this package
// exists to share, and a separate ctx would mean Manager.Shutdown's
// cancellation never reaches a task's in-flight run. spawner should be a
// threadspawn ParentAppSpawner wrapping the workspace's own App, and
// messages that
// same App's own message store — a task's child session lives in the
// parent's own message store, not a separate one, so [TaskManager.Output]
// reads it directly rather than through any task-specific plumbing.
func NewTaskManager(store Store, spawner Spawner, messages MessageService, lc *lifecycle, ctx context.Context) *TaskManager {
	t := &TaskManager{store: store, spawner: spawner, messages: messages, ctx: ctx, lc: lc}
	// Started here rather than left to the caller: a task manager without
	// its idle sweep silently loses the slot every wedged run costs, and
	// nothing about the construction site makes that omission visible.
	// See watchdog.go for what it watches and why.
	t.startIdleWatchdog()
	return t
}

// Create records a task, gives it a child session under
// args.ParentSessionID, and dispatches args.Goal immediately in the
// task's bound runtime (the parent App). It returns once the task is
// running; it does not wait for the run to finish — subscribe to the
// shared lifecycle's events (via a [Manager] sharing it, [Manager.Wait])
// for that.
//
// The task's Name is generated (task-<uuid>), not caller-supplied: unlike
// a thread, a task has no user-chosen name, and several nameless tasks
// would otherwise collide on the store's UNIQUE(project_path, name).
//
// Create refuses over maxActiveTasksPerWorkspace or
// maxActiveTasksPerParentTurn active tasks (see checkActiveCaps) rather
// than queuing the request: a caller told "started" when the work was
// actually deferred would go on to reason about a delegation that does
// not exist yet.
func (t *TaskManager) Create(ctx context.Context, args TaskCreateArgs) (Thread, error) {
	done, err := t.lc.beginOp()
	if err != nil {
		return Thread{}, err
	}
	defer done()
	if args.Goal == "" {
		return Thread{}, fmt.Errorf("thread: task goal is required")
	}
	if args.ParentSessionID == "" {
		return Thread{}, fmt.Errorf("thread: task requires a parent session")
	}

	// Held for the rest of this call - see createMu's own doc comment for
	// why the check and the create that acts on it must be one atomic
	// step, not two.
	t.createMu.Lock()
	defer t.createMu.Unlock()
	if err := t.checkActiveCaps(ctx, args.ParentSessionID); err != nil {
		return Thread{}, err
	}

	st, err := t.store.Create(ctx, CreateParams{
		Name:            "task-" + uuid.NewString(),
		Goal:            args.Goal,
		Kind:            KindTask,
		ParentSessionID: args.ParentSessionID,
	})
	if err != nil {
		return Thread{}, fmt.Errorf("thread: create task record: %w", err)
	}
	t.lc.publish(EventCreated, st)

	// The task is resolvable from here on. Nothing tears a task down yet
	// (this step does not wire task removal), but startRun's contract
	// requires the caller hold the entity's opMu across installation
	// regardless, so this mirrors Manager.Create's guard for when that
	// changes.
	c := t.lc.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	removed := c.removed
	// Stash the creating turn's cascade depth on the control now, while
	// nothing else can be reading it yet - deliverTaskCompletion reads it
	// back through this same control once the task finishes. parentSessionID
	// is stashed the same way Manager.Create stashes it for a thread (see
	// threadControl.parentSessionID) - checkActiveCaps reads it back to
	// count this task against its own parent turn's budget while it is
	// active.
	c.depth = args.Depth
	c.parentSessionID = args.ParentSessionID
	c.mu.Unlock()
	if removed {
		return Thread{}, fmt.Errorf("thread: task %q was removed during creation", st.ID)
	}

	handle, err := t.spawner.Spawn(t.ctx, "")
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}
	// This call owns handle until startRun installs it as the shared
	// runtime; release it on every earlier exit. ParentAppSpawner.Release
	// is a no-op, but a future Spawner for this kind might not be.
	owned := true
	defer func() {
		if owned {
			_ = t.spawner.Release(ctx, handle.ID())
		}
	}()

	title := args.SessionTitle
	if title == "" {
		title = args.Goal
	}
	childSessionID := args.SessionID
	if childSessionID == "" {
		childSessionID = uuid.NewString()
	}
	var sess Session
	if args.AgentID == "" {
		sess, err = handle.Workspace().Sessions().CreateTaskSession(ctx, childSessionID, args.ParentSessionID, title)
	} else {
		sess, err = handle.Workspace().Sessions().CreateSubAgentSession(ctx, childSessionID, args.ParentSessionID, title, args.AgentID)
	}
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}

	newSt, err := t.store.SetSession(ctx, st.ID, sess.ID)
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}
	st = newSt
	// Register the parent here, right where sess.ID becomes durably
	// associated with the task record: a task shares its parent's own
	// App/Coordinator (see DelegationParent's doc comment), so this is
	// the same coordinator instance startRun will later dispatch the
	// task's turns through. Placing it before setStatus/startRun means
	// no later error path in this function can leave a half-registered
	// parent pointing at a session that never actually runs.
	coord := handle.Workspace().Coordinator()
	registerParent(coord, coord, st, args.Depth)

	// Into runningSt, not st: setStatus returns the zero Thread on error,
	// and failCreate below needs st's real ID (see SetSession's identical
	// newSt pattern above - the same clobber-on-error class of bug).
	runningSt, err := t.lc.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}
	st = runningSt
	// A task's tool calls hit the parent App's permission.Service — the
	// same one the visible turn uses — so without a delegation tag, a
	// prompt raised by the task's run would be indistinguishable from one
	// raised by the user's own turn.
	//
	// startRun stamps the tag too, from the store (lifecycle.withDelegation),
	// and in the ordinary case that stamp simply replaces this one with the
	// same values. This one is not therefore redundant: withDelegation is
	// best-effort and returns the context untagged when its store lookup
	// fails, and the values are already in hand here, where nothing can
	// fail. Stamping a context derived from t.ctx rather than t.ctx itself
	// keeps it scoped to this run — t.ctx is shared with every other task
	// and, transitively, with Manager's threads, and must not pick up one
	// run's identity permanently.
	runCtx := permission.WithDelegation(t.ctx, permission.DelegationRef{
		ID:   st.ID,
		Name: st.Name,
		Kind: string(st.Kind),
	})
	if args.Factory == nil {
		t.lc.startRun(WithAgentDispatch(runCtx), handle, t.spawner, st.ID, st.SessionID, args.Goal)
	} else {
		t.lc.startFactoryRun(runCtx, handle, t.spawner, st.ID, st.SessionID, args.Factory)
	}
	owned = false // Ownership transferred to the shared runtime state.

	// ids and depth only — args.Goal is the user's prompt and never
	// belongs in a log line (see the package-level logging note in
	// lifecycle.go).
	slog.Info("Task dispatched", "task", st.ID, "session", st.SessionID, "parent_session", args.ParentSessionID, "depth", args.Depth)

	return st, nil
}

// checkActiveCaps refuses Create with a clear, model-visible error once
// either concurrency cap (maxActiveTasksPerWorkspace,
// maxActiveTasksPerParentTurn) is already at its limit. Callers must hold
// t.createMu across this call and whatever create it gates - see that
// field's doc comment for why the check alone is not enough.
//
// "Active" is Status.Active(): pending or running. A task blocked on a
// permission prompt mid-run is still StatusRunning - it does not stop
// holding its slot while it waits, by design, the same way it does not
// stop counting as "in flight" anywhere else in this package. A completed,
// failed, or interrupted task holds no slot at all.
//
// parentSessionID is read back from each active task's own control (see
// Create's parentSessionID stash) rather than resolved through Sessions,
// since it is already in memory and this runs under createMu on every
// dispatch - a DB round trip per active task on every Create would be a
// needless cost for something already sitting in the controls map.
func (t *TaskManager) checkActiveCaps(ctx context.Context, parentSessionID string) error {
	tasks, err := t.List(ctx)
	if err != nil {
		return fmt.Errorf("thread: check active task count: %w", err)
	}

	var total, forParent int
	for _, tk := range tasks {
		if !tk.Status.Active() {
			continue
		}
		total++
		// The persisted column is the fallback, and it is what makes the
		// count right after a restart: a task resumed then has no
		// in-memory control to read a parent from, so every one of them
		// was invisible to the per-parent limit and a parent could
		// exceed it without ever being told.
		parent := tk.ParentSessionID
		if c := t.lc.existingControl(tk.ID); c != nil {
			c.mu.Lock()
			if c.parentSessionID != "" {
				parent = c.parentSessionID
			}
			c.mu.Unlock()
		}
		if parent == parentSessionID {
			forParent++
		}
	}

	if total >= maxActiveTasksPerWorkspace {
		return fmt.Errorf(
			"thread: %d background tasks already running in this workspace (limit %d); wait for one to finish",
			total, maxActiveTasksPerWorkspace,
		)
	}
	if forParent >= maxActiveTasksPerParentTurn {
		return fmt.Errorf(
			"thread: this turn already has %d background tasks running (limit %d); wait for one to finish",
			forParent, maxActiveTasksPerParentTurn,
		)
	}
	return nil
}

// failCreate records cause as the task's terminal failure and returns it
// to Create's caller.
func (t *TaskManager) failCreate(ctx context.Context, st Thread, cause error) error {
	if _, err := t.lc.setStatus(ctx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("Failed to record task create failure", "component", "thread", "task", st.ID, "error", err)
	}
	return cause
}

// List returns every known task (kind = KindTask), across the whole
// workspace — the same scope [Manager.List] uses for threads. Store.List
// is kind = 'thread'-scoped, so this goes through ListAll and filters in
// Go instead, rather than adding a second SQL query for one caller.
func (t *TaskManager) List(ctx context.Context) ([]Thread, error) {
	all, err := t.store.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	tasks := make([]Thread, 0, len(all))
	for _, st := range all {
		if st.Kind == KindTask {
			tasks = append(tasks, st)
		}
	}
	return tasks, nil
}

// Get resolves id to a task. Unlike [Manager.Get], there is no
// resolve-by-name fallback: a task's Name is generated, not user-chosen
// (see Create), so nothing ever addresses one by name.
//
// Rejects a thread's id with a clear error rather than returning it: one
// table means every id is reachable through Store.Get regardless of kind,
// and a task-facing caller asking for a task must never be handed a
// thread — the same guard [Manager.Merge]/[Manager.Remove] apply in the
// other direction.
func (t *TaskManager) Get(ctx context.Context, id string) (Thread, error) {
	st, err := t.store.Get(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	if st.Kind != KindTask {
		return Thread{}, fmt.Errorf("thread: %q is not a task", id)
	}
	return st, nil
}

// Cancel stops id's in-flight run and leaves it at the terminal
// [StatusCancelled] with reason recorded as its Error. A task has no
// merge flow or other kind-specific state to refuse this for — every
// task is cancellable in any non-terminal status — so this is a thin
// Get-then-delegate wrapper; see [lifecycle.cancel] for the mechanics
// shared with [Manager.Cancel].
func (t *TaskManager) Cancel(ctx context.Context, id, reason string) error {
	// Detached before the read below, not just around the write: a
	// cancel arriving on a dead context could not even load the task to
	// cancel it. See detachForTerminalWork.
	ctx, done := detachForTerminalWork(ctx)
	defer done()
	// Admitted like every other mutation: without this a cancel could
	// start after Shutdown had closed admission and begun joining
	// workers, and then tear down state those workers were still using.
	admitted, err := t.lc.beginOp()
	if err != nil {
		return err
	}
	defer admitted()
	st, err := t.Get(ctx, id)
	if err != nil {
		return err
	}
	return t.lc.cancel(ctx, st, reason)
}

// Send dispatches message into id's session, reactivating it first if its
// runtime is not currently live — the same "queue if live, otherwise
// respawn and dispatch" behavior [Manager.Send] gives a thread, shared via
// lifecycle.send. For a task, "respawn" only means rebinding to the
// parent App through threadspawn's ParentAppSpawner; nothing is actually
// rebuilt.
//
// A task [TaskManager.Cancel] stopped is the one exception: cancelling was
// a decision, not a pause, so Send refuses to resume it rather than
// silently treating it the same as a task that was merely interrupted
// (e.g. by a process restart). Anything else not live — completed,
// failed, or incidentally interrupted — is reactivated exactly like a
// thread would be, since a task has no merge-flow-equivalent state that
// would make reactivating it meaningless the way [Manager.Activate]
// refuses a thread mid-merge.
func (t *TaskManager) Send(ctx context.Context, id, message string) (SendDisposition, error) {
	done, err := t.lc.beginOp()
	if err != nil {
		return SendDisposition{}, err
	}
	defer done()
	st, err := t.Get(ctx, id)
	if err != nil {
		return SendDisposition{}, err
	}
	if wasCancelled(st) {
		return SendDisposition{}, fmt.Errorf("thread: task %q was cancelled (%s) and cannot be resumed; create a new task instead", id, st.Error)
	}
	disp, err := t.lc.send(ctx, t.ctx, st.ID, t.spawner, "", st.SessionID, message, SenderAgent, nil)
	if err != nil {
		return SendDisposition{}, err
	}
	// The dispatcher's DelegationParent registry lives per coordinator
	// instance and is empty on a freshly-started process — see
	// Manager.Send's identical re-registration. A task's Parent is its own
	// coordinator (it shares its parent's App via threadspawn's
	// ParentAppSpawner, unlike a thread's wholly isolated one), so no
	// ParentApp guard is needed here, only a persisted parent to re-register.
	if c := t.lc.existingControl(st.ID); c != nil {
		c.mu.Lock()
		rt := c.runtime
		depth := c.depth
		c.mu.Unlock()
		if rt != nil {
			coord := rt.handle.Workspace().Coordinator()
			registerParent(coord, coord, st, depth)
		}
	}
	return disp, nil
}

// wasCancelled reports whether st was explicitly stopped via
// [TaskManager.Cancel] or [Manager.Cancel], as opposed to reaching a
// terminal status some other way (completing normally, failing, or an
// incidental interruption — see [StatusCancelled]'s own doc comment for
// the distinction). A plain status check now; it used to infer this from
// StatusInterrupted plus a non-empty Error, which held only because every
// other writer of that status happened to leave Error empty — nothing
// enforced that, so a status of its own replaces the inference rather
// than sitting alongside it.
func wasCancelled(st Thread) bool {
	return st.Status == StatusCancelled
}

// defaultOutputLimit and maxOutputLimit bound how many of a task's child
// session messages [TaskManager.Output] returns: a background task's
// transcript goes straight into the parent's own context when read, so
// the default stays small and even an explicit request cannot ask for
// the whole history in one call.
const (
	defaultOutputLimit = 20
	maxOutputLimit     = 100
)

// TaskOutputMessage is one message from a task's child session, reduced
// to what [TaskManager.Output] surfaces: the role and its text content.
// Tool calls, tool results, and reasoning are omitted — see Output's doc
// comment for why.
type TaskOutputMessage struct {
	Role string
	Text string
}

// TaskOutput is the result of [TaskManager.Output]: a tail of a task's
// child session transcript.
type TaskOutput struct {
	Messages []TaskOutputMessage
	// Total is how many user/assistant text messages exist in the
	// session, so a caller can tell a truncated tail from the whole
	// transcript instead of the omission passing silently.
	Total int
}

// Output returns a tail of id's child session transcript: the same data
// the UI would render for that session, reduced to user and assistant
// text only. Tool calls, tool results, and reasoning are deliberately
// left out — the point of this method is letting the caller check on a
// task without the check itself flooding its own context, and raw tool
// traffic is exactly what would defeat that.
//
// limit caps how many of the most recent such messages come back; <= 0
// uses defaultOutputLimit, and any larger value is clamped to
// maxOutputLimit — the default is a starting point, not a ceiling the
// caller can lift by asking.
func (t *TaskManager) Output(ctx context.Context, id string, limit int) (TaskOutput, error) {
	st, err := t.Get(ctx, id)
	if err != nil {
		return TaskOutput{}, err
	}
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	if limit > maxOutputLimit {
		limit = maxOutputLimit
	}

	all, err := t.messages.List(ctx, st.SessionID)
	if err != nil {
		return TaskOutput{}, fmt.Errorf("thread: list task session messages: %w", err)
	}

	texts := make([]TaskOutputMessage, 0, len(all))
	for _, msg := range all {
		if msg.Role != RoleUser && msg.Role != RoleAssistant {
			continue
		}
		if msg.Text == "" {
			continue
		}
		texts = append(texts, TaskOutputMessage{Role: string(msg.Role), Text: msg.Text})
	}

	total := len(texts)
	if total > limit {
		texts = texts[total-limit:]
	}
	return TaskOutput{Messages: texts, Total: total}, nil
}
