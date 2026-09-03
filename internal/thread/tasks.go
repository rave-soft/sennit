package thread

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/permission"
)

// maxActiveTasksPerWorkspace and maxActiveTasksPerParentTurn bound
// concurrent task delegations. They are hard constants, not configuration:
// every task shares its parent App's working directory and
// permission.Service, so beyond a small number extra concurrency just
// queues behind the same gate and contends for the same files rather than
// getting more done. The cascade depth limit (maxTaskCascadeDepth,
// internal/agent) bounds how deep a chain of delegations runs; these bound
// how wide it gets at any one level.
//
// maxActiveTasksPerParentTurn is half of maxActiveTasksPerWorkspace so one
// turn's fan-out can never claim the whole budget. Threads don't count
// toward either: they spawn their own isolated App (threadspawn's
// LocalSpawner) and never touch the resources these limits protect. This
// is enforced simply by scope: the counts below are computed from List,
// which is already Kind-scoped to tasks.
const (
	maxActiveTasksPerWorkspace  = 4
	maxActiveTasksPerParentTurn = 2
)

// TaskCreateArgs holds the inputs to [TaskManager.Create].
type TaskCreateArgs struct {
	// Goal is the task's prompt, dispatched immediately — unlike a thread, a
	// task has no idle worktree to wait in, so there is no goal-less path
	// the way [CreateArgs.Goal] has for [Manager.Create].
	Goal string
	// ParentSessionID is the session the task's own child session nests
	// under, the same relationship [Manager.Create] establishes for a
	// thread via Sessions.CreateTaskSession. Required: a task with no
	// parent is not a task, just an orphaned session.
	ParentSessionID string
	// Depth is the background-delegation cascade depth of the turn that
	// created this task (0 for a real user turn), carried in-memory only
	// (threadControl.depth) to compute the depth of an auto-woken
	// continuation (agent.TaskCompletion.Depth).
	Depth int
	// SessionTitle and AgentID preserve specialized delegation history.
	SessionTitle string
	AgentID      string
	// SessionID, when set, is used verbatim as the child session's id
	// instead of a generated one, so a delegation from a tool call can
	// reuse the "<messageID>$$<toolCallID>" identity that makes it openable
	// from the parent's chat.
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
// spawning an isolated one (threadspawn's ParentAppSpawner), which is why
// it starts cheaply where a thread cannot.
//
// TaskManager is a sibling of [Manager], not a method on it: Manager's
// public surface is stated in terms of threads, and bolting a
// worktree-less kind onto it would strain that surface or force every
// thread-only method to guard against a task ID. What they share is the
// [lifecycle] underneath both — see [NewTaskManager].
type TaskManager struct {
	store    Store
	spawner  Spawner
	messages MessageService
	ctx      context.Context
	lc       *lifecycle

	// createMu serializes Create end to end, so the concurrency-cap check
	// and the create it gates run as one atomic step. Task creation isn't a
	// hot path, so serializing all of it trades away nothing that matters
	// for a correctness guarantee that does.
	createMu sync.Mutex
}

// NewTaskManager constructs a TaskManager. lc and ctx must be an existing
// [Manager]'s own m.lc and m.ctx, not freshly constructed ones: a separate
// lifecycle would split the controls map, worker group, and recovery sweep
// this package exists to share, and a separate ctx would leave
// Manager.Shutdown unable to reach a task's in-flight run. spawner should
// be a threadspawn ParentAppSpawner over the workspace's own App, and
// messages that same App's message store.
func NewTaskManager(store Store, spawner Spawner, messages MessageService, lc *lifecycle, ctx context.Context) *TaskManager {
	t := &TaskManager{store: store, spawner: spawner, messages: messages, ctx: ctx, lc: lc}
	// Started here, not left to the caller, so no construction site can
	// silently omit the idle sweep. See watchdog.go.
	t.startIdleWatchdog()
	return t
}

// Create records a task, gives it a child session under
// args.ParentSessionID, and dispatches args.Goal immediately in the task's
// bound runtime. It returns once the task is running, not once it
// finishes — subscribe via a [Manager] sharing this lifecycle
// ([Manager.Wait]) for that.
//
// The task's Name is generated (task-<uuid>), not caller-supplied: unlike
// a thread, a task has no user-chosen name, and several nameless tasks
// would otherwise collide on the store's UNIQUE(project_path, name).
//
// Create refuses over maxActiveTasksPerWorkspace or
// maxActiveTasksPerParentTurn active tasks (see checkActiveCaps) rather
// than queuing: a caller told "started" when the work was actually
// deferred would go on to reason about a delegation that doesn't exist yet.
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

	// Held for the rest of this call — see createMu's doc comment.
	t.createMu.Lock()
	defer t.createMu.Unlock()
	if err := t.checkActiveCaps(ctx, args.ParentSessionID, args.Goal); err != nil {
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

	// deliverTaskCompletion reads the cascade depth back through this same
	// control once the task finishes; checkActiveCaps reads the parent
	// link back to count this task against its parent turn's budget.
	c, removed := t.lc.beginControlledCreate(st.ID, args.ParentSessionID, args.Depth)
	defer c.opMu.Unlock()
	if removed {
		return Thread{}, fmt.Errorf("thread: task %q was removed during creation", st.ID)
	}

	handle, err := t.spawner.Spawn(t.ctx, "")
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}
	// rb unwinds the spawned handle if Create returns before startRun
	// installs it as the shared runtime — see [unwinder].
	var rb unwinder
	defer rb.unwind()
	// ParentAppSpawner.Release is a no-op, but a future Spawner for this
	// kind might not be.
	rb.push(func() {
		_ = t.spawner.Release(ctx, handle.ID())
	})

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

	// A task's child session inherits auto-approval from args.ParentSessionID,
	// and only when the parent already carries it: the parent already
	// approves everything a turn asks for, so the child is granted nothing
	// new. A task under an ordinary session still prompts. This is the one
	// place every delegation kind (the agent tool, a named agent, agentic
	// fetch, and any nested delegation reached the same way) creates its
	// child session, so propagating here covers all of them, including a
	// chain more than one level deep — the second-level child's own parent
	// is the first-level child, whose auto-approval was granted right here.
	if perms := handle.Workspace().Permissions(); perms != nil && perms.IsAutoApproveSession(args.ParentSessionID) {
		perms.AutoApproveSession(sess.ID)
	}

	newSt, err := t.store.SetSession(ctx, st.ID, sess.ID)
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}
	st = newSt

	// Into runningSt, not st: setStatus returns the zero Thread on error,
	// and failCreate below needs st's real ID.
	runningSt, err := t.lc.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}
	st = runningSt
	// Registered only once nothing left in this function can fail: a task
	// shares its parent's App/Coordinator (see DelegationParent's doc
	// comment), and registering earlier would leave a stale registration
	// pointing at a session that never runs if a later step fails.
	coord := handle.Workspace().Coordinator()
	registerParent(coord, coord, st, args.Depth)
	// A task's tool calls hit the parent App's permission.Service, the same
	// one the visible turn uses, so without a delegation tag a task's
	// prompt would be indistinguishable from the user's own turn.
	// startRun stamps the same tag from the store (lifecycle.withDelegation)
	// and ordinarily just replaces this one; it's not redundant because
	// that stamp is best-effort and returns the context untagged if its
	// store lookup fails. The derived context, not t.ctx itself, is
	// stamped so the identity stays scoped to this run — t.ctx is shared
	// with every other task and thread.
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
	rb.commit()

	// ids and depth only — args.Goal is the user's prompt and never
	// belongs in a log line.
	slog.Info("Task dispatched", "task", st.ID, "session", st.SessionID, "parent_session", args.ParentSessionID, "depth", args.Depth)

	return st, nil
}

// checkActiveCaps refuses Create with a clear, model-visible error once
// either concurrency cap (maxActiveTasksPerWorkspace,
// maxActiveTasksPerParentTurn) is already at its limit, or when the same
// parent already has this very task running. Callers must hold t.createMu
// across this call and whatever create it gates — see that field's doc
// comment.
//
// The duplicate check is narrow by design: same parent, same goal
// (whitespace- and case-folded), and the twin still active. A goal that
// merely resembles a live one is left alone — refusing real work is worse
// than the rare duplicate — and a goal repeated after its twin finished is
// an ordinary retry, handled where the report is delivered
// (runTurn.foldCompletions), not here.
//
// "Active" is Status.Active(): pending or running, including a task
// blocked on a permission prompt mid-run — it still holds its slot.
//
// parentSessionID is read back from each active task's own in-memory
// control rather than resolved through Sessions, since this runs under
// createMu on every dispatch and a DB round trip per active task would be
// a needless cost. The persisted column is the fallback for a task resumed
// after a restart, which has no in-memory control to read from.
func (t *TaskManager) checkActiveCaps(ctx context.Context, parentSessionID, goal string) error {
	tasks, err := t.List(ctx)
	if err != nil {
		return fmt.Errorf("thread: check active task count: %w", err)
	}

	wanted := normalizeGoal(goal)
	var total, forParent int
	for _, tk := range tasks {
		if !tk.Status.Active() {
			continue
		}
		total++
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
			if normalizeGoal(tk.Goal) == wanted {
				return fmt.Errorf(
					"thread: this turn already has %s running the same task (%s); wait for it to finish, or read its answer with task_result once it does",
					tk.ID, tk.Status,
				)
			}
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

// normalizeGoal folds whitespace and case so checkActiveCaps's duplicate
// check treats a goal re-sent with different indentation or casing as the
// same goal. Matching is exact after folding — near-misses are left alone.
func normalizeGoal(goal string) string {
	return strings.ToLower(strings.Join(strings.Fields(goal), " "))
}

// failCreate records cause as the task's terminal failure and returns it
// to Create's caller.
func (t *TaskManager) failCreate(ctx context.Context, st Thread, cause error) error {
	// detachForTerminalWork, not ctx directly: cause is often ctx having
	// been cancelled, and a status write on that same dead ctx would fail
	// too. See [Manager.failCreate], which has the same problem.
	writeCtx, cancel := detachForTerminalWork(ctx)
	defer cancel()
	if _, err := t.lc.setStatus(writeCtx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("Failed to record task create failure", "component", "thread", "task", st.ID, "error", err)
	}
	return cause
}

// List returns every known task (kind = KindTask), across the whole
// workspace. Store.List is kind = 'thread'-scoped, so this goes through
// ListAll and filters in Go instead of adding a second SQL query.
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
// resolve-by-name fallback: a task's Name is generated, not user-chosen.
//
// Rejects a thread's id with a clear error rather than returning it — the
// same guard [Manager.Merge]/[Manager.Remove] apply in the other direction.
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
// [StatusCancelled] with reason recorded as its Error. See [lifecycle.cancel]
// for the mechanics shared with [Manager.Cancel].
//
// The cancel reaches id's whole subtree, not id alone: a cancelled task
// will never read its delegations' answers, so left running they'd go on
// holding concurrency slots, prompting for permissions, and editing the
// workspace with nothing above them to look at the result.
//
// Only id's failure is returned. A descendant that will not cancel is
// logged and the sweep continues, rather than abandoning the rest of the
// subtree.
func (t *TaskManager) Cancel(ctx context.Context, id, reason string) error {
	// Detached before the read below too: a cancel on a dead context
	// couldn't even load the task to cancel it.
	ctx, done := detachForTerminalWork(ctx)
	defer done()
	// Held across the descendants below too, as one operation, so a cancel
	// can't start after Shutdown has closed admission and begun tearing
	// down state those workers still use.
	admitted, err := t.lc.beginOp()
	if err != nil {
		return err
	}
	defer admitted()
	st, err := t.Get(ctx, id)
	if err != nil {
		return err
	}
	// Read before st is cancelled: a listing taken after would miss
	// nothing, while cancelling first and then failing to list would leave
	// the whole subtree running.
	descendants := t.descendantsOf(ctx, st)
	cancelErr := t.lc.cancel(ctx, st, reason)
	for _, child := range descendants {
		childReason := fmt.Sprintf("the task this was delegated by (%s) was cancelled", st.ID)
		if reason != "" {
			childReason += ": " + reason
		}
		if err := t.lc.cancel(ctx, child, childReason); err != nil {
			slog.Error("Failed to cancel a delegation of a cancelled task",
				"component", "thread", "task", child.ID, "parent_task", st.ID, "error", err)
		}
	}
	return cancelErr
}

// descendantsOf returns every task below st, nearest first, by walking the
// parent-pointer forest breadth-first: a task's child session
// (Thread.SessionID) is the ParentSessionID of everything it delegates.
//
// A listing that cannot be read yields nothing rather than an error: the
// cancel the caller asked for must still happen.
func (t *TaskManager) descendantsOf(ctx context.Context, st Thread) []Thread {
	if st.SessionID == "" {
		return nil
	}
	all, err := t.List(ctx)
	if err != nil {
		slog.Warn("Could not list the delegations of a task being cancelled",
			"component", "thread", "task", st.ID, "error", err)
		return nil
	}
	byParent := make(map[string][]Thread, len(all))
	for _, child := range all {
		byParent[child.ParentSessionID] = append(byParent[child.ParentSessionID], child)
	}
	var out []Thread
	// seen guards the walk rather than a depth limit, so a store
	// describing a cycle can't hang the cancel.
	seen := map[string]struct{}{st.ID: {}}
	for frontier := []string{st.SessionID}; len(frontier) > 0; {
		session := frontier[0]
		frontier = frontier[1:]
		for _, child := range byParent[session] {
			if _, ok := seen[child.ID]; ok {
				continue
			}
			seen[child.ID] = struct{}{}
			out = append(out, child)
			if child.SessionID != "" {
				frontier = append(frontier, child.SessionID)
			}
		}
	}
	return out
}

// Send dispatches message into id's session, reactivating it first if its
// runtime isn't currently live — the same "queue if live, otherwise
// respawn and dispatch" behavior [Manager.Send] gives a thread, shared via
// lifecycle.send. For a task, "respawn" only means rebinding to the parent
// App; nothing is actually rebuilt.
//
// A task [TaskManager.Cancel] stopped is the one exception: cancelling was
// a decision, not a pause, so Send refuses to resume it. Anything else not
// live — completed, failed, or incidentally interrupted — is reactivated
// like a thread would be, since a task has no merge-flow-equivalent state
// that would make reactivating it meaningless.
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
	disp, err := t.lc.send(ctx, t.ctx, st.ID, t.spawner, "", st.SessionID, message, SenderAgent, nil, nil)
	if err != nil {
		return SendDisposition{}, err
	}
	// The dispatcher's DelegationParent registry is per coordinator
	// instance and empty on a fresh process, so it's re-registered here —
	// see Manager.Send's identical re-registration.
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
// terminal status some other way — see [StatusCancelled]'s doc comment.
func wasCancelled(st Thread) bool {
	return st.Status == StatusCancelled
}

// defaultOutputLimit and maxOutputLimit bound how many of a task's child
// session messages [TaskManager.Output] returns: the transcript goes
// straight into the parent's own context when read, so even an explicit
// request cannot ask for the whole history in one call.
const (
	defaultOutputLimit = 20
	maxOutputLimit     = 100
)

// TaskOutputMessage is one message from a task's child session, reduced to
// what [TaskManager.Output] surfaces: the role and its text content.
type TaskOutputMessage struct {
	Role string
	Text string
}

// TaskOutput is the result of [TaskManager.Output]: a tail of a task's
// child session transcript.
type TaskOutput struct {
	Messages []TaskOutputMessage
	// Total is how many user/assistant text messages exist in the session,
	// so a caller can tell a truncated tail from the whole transcript.
	Total int
}

// Output returns a tail of id's child session transcript, reduced to user
// and assistant text only: tool calls, tool results, and reasoning are
// left out so a caller checking on a task doesn't flood its own context
// with raw tool traffic.
//
// limit caps how many of the most recent such messages come back; <= 0
// uses defaultOutputLimit, and any larger value is clamped to
// maxOutputLimit.
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
