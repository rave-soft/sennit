package thread

import (
	"context"
	"log/slog"
	"time"
)

// A task ends when its run ends. That is the only way this package ever
// learns a delegation is finished: handleRunComplete reacts to the
// coordinator's terminal event, and everything downstream of it — the
// terminal status, the released workspace, the completion the parent
// agent is waiting on — hangs off that one notification.
//
// Which means a run that never terminates is not a slow task, it is a
// permanently lost one. The row stays at StatusRunning, so it keeps
// holding a slot against maxActiveTasksPerParentTurn and
// maxActiveTasksPerWorkspace, and the parent is never told anything: it
// does not fail, it does not answer, it just stops. Recovery only
// reconciles this on the next process start (see lifecycle.recover), so
// in a long-lived session the slot is gone for the rest of the day.
//
// The stall watchdog in internal/agent covers the case that produced
// this — a provider stream that goes silent — at the point where it can
// be diagnosed precisely. This is the backstop underneath it, for the
// wedges nothing more specific catches: a tool that never returns, a
// coordinator that loses a run, a bug not yet written. It asks one crude
// question, "has this delegation's session been written to at all
// recently", and answers it the only way a backstop honestly can, by
// ending the delegation and telling the parent.
const (
	// taskIdleTimeout is how long a running task's session may go without
	// a single message write before the watchdog treats the run as lost.
	//
	// Every streaming delta writes to the session's history, so during
	// normal work this is refreshed constantly, seconds apart. The gaps
	// that are legitimately long are the ones between writes: a single
	// tool call that runs for a long time (a full test suite, a long
	// build) writes nothing while it runs. So the budget is set well past
	// any such call rather than close to it — the cost of firing late is
	// a slot held a while longer, and the cost of firing early is killing
	// work that was fine, which is much worse.
	taskIdleTimeout = 45 * time.Minute
	// taskWatchdogInterval is how often the sweep runs. It bounds how
	// late a trip can be (idle timeout + one interval) and is deliberately
	// coarse: each pass is one cheap indexed query per running task, and
	// nothing about this problem needs sub-minute resolution.
	taskWatchdogInterval = 5 * time.Minute
)

// startIdleWatchdog runs the idle sweep for as long as t's context lives.
// It is started from [NewTaskManager] so no caller can forget it, and it
// registers as an admitted operation, which is what makes Manager.Shutdown
// join it: shutdown cancels the shared context first and then waits, so
// the goroutine is already on its way out by the time the wait begins.
//
// A closed lifecycle (a manager already shutting down when the task
// manager is built) simply gets no watchdog, which is correct — there is
// nothing left for it to watch.
func (t *TaskManager) startIdleWatchdog() {
	done, err := t.lc.beginOp()
	if err != nil {
		return
	}
	go func() {
		defer done()
		ticker := time.NewTicker(taskWatchdogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-ticker.C:
				t.sweepIdleTasks(t.ctx, time.Now())
			}
		}
	}()
}

// sweepIdleTasks ends every running task that has produced nothing for
// longer than [taskIdleTimeout]. now is passed in rather than read here so
// tests can drive the clock.
func (t *TaskManager) sweepIdleTasks(ctx context.Context, now time.Time) {
	tasks, err := t.List(ctx)
	if err != nil {
		slog.Warn("Could not list tasks for the idle sweep", "component", "thread", "error", err)
		return
	}
	for _, st := range tasks {
		if st.Status != StatusRunning {
			continue
		}
		idleFor, ok := t.idleDuration(ctx, st, now)
		if !ok || idleFor < taskIdleTimeout {
			continue
		}
		t.endIdleTask(ctx, st, idleFor)
	}
}

// idleDuration reports how long st has been silent, and whether it is a
// delegation the watchdog may judge at all. ok=false means "not mine to
// end", for one of the reasons below — each of which is a state where
// silence is either expected or not this process's business:
//
//   - no live runtime: the row says running but nothing here is driving
//     it. That is a task left behind by a previous process (recover
//     reconciles those at startup) or one already being torn down.
//   - a person's own turn: the human is sitting in the session and may
//     take as long as they like between messages.
//   - parked awaiting its own delegations: the delegation is not working,
//     it is waiting, exactly as designed (see handleRunComplete's park
//     branch). Its children are producing, and each of them is watched
//     here in its own right — so a wedge below still gets caught, and the
//     parent is woken by the completion that follows.
//   - waiting on a permission prompt: blocked on a person, with no upper
//     bound by design. Killing these would mean killing precisely the
//     delegations the user was about to approve.
func (t *TaskManager) idleDuration(ctx context.Context, st Thread, now time.Time) (time.Duration, bool) {
	c := t.lc.existingControl(st.ID)
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	rt := c.runtime
	var person, parked bool
	if rt != nil {
		person, parked = rt.person, rt.awaitingDelegations
	}
	c.mu.Unlock()
	if rt == nil || person || parked {
		return 0, false
	}

	if ws := rt.handle.Workspace(); ws != nil {
		if perms := ws.Permissions(); perms != nil && perms.AwaitingAnswer(st.ID) {
			return 0, false
		}
	}

	last, err := t.messages.LastActivity(ctx, st.SessionID)
	if err != nil {
		// Unknown is not idle. A store that cannot answer must never be
		// the reason a healthy delegation is killed; the next sweep asks
		// again.
		slog.Warn("Could not read a task's last activity for the idle sweep",
			"component", "thread", "task", st.ID, "error", err)
		return 0, false
	}
	// A session with nothing in it yet is measured from the task's own
	// creation: the prompt is dispatched at Create, so a task that has
	// been running for the whole budget without writing a single message
	// has produced nothing at all.
	since := last
	if since.IsZero() {
		since = time.Unix(st.CreatedAt, 0)
	}
	return now.Sub(since), true
}

// endIdleTask finalizes st as failed and tells its parent, then cancels
// whatever the run is still doing.
//
// The order is deliberate. Cancelling first would let the coordinator's
// own terminal event win the race and record the delegation as
// interrupted — the status that means "someone stopped this" — when what
// actually happened is that it stopped answering. Finalizing first fixes
// the outcome as failed with a reason that says so; the cancel that
// follows still stops the wasted work, and the real terminal event, if
// one ever arrives, finds no runtime left to act on and is ignored.
//
// A run wedged in a syscall may not react to the cancel at all. That is
// survivable and was always the alternative: the slot and the parent's
// answer are recovered either way, and only the wasted work continues.
func (t *TaskManager) endIdleTask(ctx context.Context, st Thread, idleFor time.Duration) {
	c := t.lc.existingControl(st.ID)
	if c == nil {
		return
	}
	c.mu.Lock()
	rt := c.runtime
	var runID string
	if rt != nil {
		runID = rt.runID
	}
	c.mu.Unlock()
	if rt == nil || runID == "" {
		return
	}

	slog.Warn("Task produced nothing within the idle budget; ending it",
		"component", "thread", "task", st.ID, "session_id", st.SessionID,
		"idle", idleFor.Round(time.Second).String(),
		"budget", taskIdleTimeout.String())

	t.lc.handleRunComplete(ctx, st.ID, RunComplete{
		SessionID: st.SessionID,
		RunID:     runID,
		Error:     "task produced nothing for " + idleFor.Round(time.Minute).String() + "; ended by the idle watchdog",
	})

	// Cancel only what this call actually ended. Nothing here holds the
	// entity's opMu — handleRunComplete takes it itself — so between the
	// runtime read above and that call, the run may have completed on its
	// own and the session may already be driving a *different* run (a
	// continuation, a TaskManager.Send). Cancelling by session id would
	// then kill healthy work in the name of a delegation that was already
	// finished. A cleared runtime is the proof that the synthetic
	// completion above is the one that landed; anything else means
	// somebody else is in charge and this must keep its hands off.
	c.mu.Lock()
	ended := c.runtime == nil
	c.mu.Unlock()
	if !ended {
		return
	}
	if ws := rt.handle.Workspace(); ws != nil && ws.Coordinator() != nil {
		ws.Coordinator().Cancel(st.SessionID)
	}
}
