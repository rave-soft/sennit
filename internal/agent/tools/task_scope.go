package tools

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// Background tasks form a tree, and every task_* tool answers from one
// vantage point in it: the session the tool call came from. A task's own
// child session (TaskInfo.SessionID) is the TaskInfo.ParentSessionID of
// every task that task goes on to start, so the flat list the manager
// returns is a parent-pointer forest, and the caller's place in it is
// what decides which tasks that caller is allowed to see and act on.
//
// The rule is one line: a caller reaches its own subtree and nothing
// else. Everything the rule buys follows from the shape rather than from
// a list of special cases — a delegation cannot cancel itself (it is not
// below itself), cannot cancel the delegation it hangs under, cannot
// reach across to a sibling's work, and the session a person is typing
// into still sees every task it started, however many levels down.
//
// It exists because the unscoped version cost a night's work. A
// delegated agent meant to cancel one of the two tasks it had started,
// passed its own id to task_cancel instead — its own row was in the
// listing, and nothing checked whose it was — and killed itself
// mid-turn. Its report to the session that was waiting on it died with
// it, its own child kept editing the repository for nine more minutes
// with nobody left above it, and the pipeline stopped where it stood.
type taskScope struct {
	// callerSessionID is the session the tool call arrived on: a
	// delegation's own child session, or the session a person is typing
	// into.
	callerSessionID string
	// tasks is the manager's listing, order preserved: every scoped
	// listing is a filter over it, so callers keep the store's order.
	tasks []TaskInfo
	// bySession maps a task's own child session to the task running in
	// it, for walking up from a caller to the delegation it *is*.
	bySession map[string]TaskInfo
	// descendants holds the ids of every task below callerSessionID,
	// however many levels down.
	descendants map[string]struct{}
}

// newTaskScope resolves callerSessionID's place in tasks. A caller with
// no task of its own (a person's session) is not an error: it simply
// sits above the forest rather than inside it, and its subtree is
// everything it started.
func newTaskScope(callerSessionID string, tasks []TaskInfo) taskScope {
	s := taskScope{
		callerSessionID: callerSessionID,
		tasks:           tasks,
		bySession:       make(map[string]TaskInfo, len(tasks)),
		descendants:     make(map[string]struct{}, len(tasks)),
	}
	byParent := make(map[string][]TaskInfo, len(tasks))
	for _, t := range tasks {
		if t.SessionID != "" {
			s.bySession[t.SessionID] = t
		}
		byParent[t.ParentSessionID] = append(byParent[t.ParentSessionID], t)
	}
	// Breadth-first down the parent pointers, guarded by the visited set
	// rather than by a depth limit: a store that somehow described a
	// cycle would otherwise hang the tool call that read it.
	frontier := []string{callerSessionID}
	for len(frontier) > 0 {
		session := frontier[0]
		frontier = frontier[1:]
		for _, child := range byParent[session] {
			if _, seen := s.descendants[child.ID]; seen {
				continue
			}
			s.descendants[child.ID] = struct{}{}
			if child.SessionID != "" {
				frontier = append(frontier, child.SessionID)
			}
		}
	}
	return s
}

// subtree returns the tasks the caller may see, in the manager's own
// order.
func (s taskScope) subtree() []TaskInfo {
	out := make([]TaskInfo, 0, len(s.descendants))
	for _, t := range s.tasks {
		if _, ok := s.descendants[t.ID]; ok {
			out = append(out, t)
		}
	}
	return out
}

// self returns the task the caller is running as, if the caller is a
// delegation rather than a person's session.
func (s taskScope) self() (TaskInfo, bool) {
	t, ok := s.bySession[s.callerSessionID]
	return t, ok
}

// refuse reports why the caller may not act on id, or ok=false when it
// may. The wording is aimed at the model that made the call: it has to
// say what to do instead, since the interesting refusals are all cases
// where the model wanted something real and reached for the wrong id.
func (s taskScope) refuse(id string, verb string) (fantasy.ToolResponse, bool) {
	if _, ok := s.descendants[id]; ok {
		return fantasy.ToolResponse{}, false
	}
	if self, ok := s.self(); ok && self.ID == id {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Task %s is the task you are running as; you cannot %s yourself. "+
				"If you meant to stop one of the tasks you started, list them first — "+
				"task_list shows only your own. If you meant to stop working, end "+
				"your turn with your report instead.", id, verb)), true
	}
	// Up the parent pointers from the caller's own task. The seen set is
	// the loop guard, for the same reason it is in newTaskScope.
	seen := make(map[string]struct{}, len(s.bySession))
	for session := s.callerSessionID; ; {
		task, ok := s.bySession[session]
		if !ok {
			break
		}
		if _, looped := seen[task.ID]; looped {
			break
		}
		seen[task.ID] = struct{}{}
		if task.ID == id {
			return fantasy.NewTextErrorResponse(fmt.Sprintf(
				"Task %s is a delegation you are running under; you cannot %s it. "+
					"Say what is wrong in your report and let it decide.", id, verb)), true
		}
		session = task.ParentSessionID
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf(
		"No task %s among the tasks you started, so there is nothing here to %s: "+
			"task_list shows the ones that are yours, and a task someone else "+
			"started is not one of them. If that is a thread's id and not a "+
			"task's, the thread_* tools are the ones that take it.", id, verb)), true
}

// scopeTasks is the preamble every task_* tool shares: resolve the
// calling session, list, and place the caller in the tree. The listing
// is workspace-wide by necessity — the tree cannot be walked from a
// pre-filtered slice — and is narrowed here, once, for all of them.
// A non-nil failed is the response the tool must return as-is.
func scopeTasks(ctx context.Context, manager TaskManager, action string) (scope taskScope, failed *fantasy.ToolResponse, err error) {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return taskScope{}, nil, missingSessionID(action)
	}
	tasks, err := manager.List(ctx)
	if err != nil {
		resp := fantasy.NewTextErrorResponse(err.Error())
		return taskScope{}, &resp, nil
	}
	return newTaskScope(sessionID, tasks), nil, nil
}
