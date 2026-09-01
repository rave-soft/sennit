package threads

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/listcache"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestActiveDockThreadsFiltersAndSorts(t *testing.T) {
	t.Parallel()

	threads := []proto.Thread{
		{ID: "merged", Status: "merged", CreatedAt: 1},
		{ID: "b-running", Status: "running", CreatedAt: 30},
		{ID: "a-pending", Status: "pending", CreatedAt: 10},
		{ID: "failed", Status: "failed", CreatedAt: 5},
		{ID: "c-merging", Status: "merging", CreatedAt: 20},
	}

	active := ActiveDockThreads(threads)
	require.Len(t, active, 3)
	require.Equal(t, []string{"a-pending", "c-merging", "b-running"}, dockThreadIDs(active))
}

// TestActiveDockThreadsIncludesIdle proves "idle must not read as
// finished" (see StatusIdle's doc comment) at the filtering layer: an idle
// delegation's workspace is still live and belongs in the dock's live-work
// list even though Status.Active() alone excludes it. Covers both a
// Kind=thread and a Kind=task idle row.
func TestActiveDockThreadsIncludesIdle(t *testing.T) {
	t.Parallel()

	threads := []proto.Thread{
		{ID: "idle-thread", Kind: "thread", Status: "idle", CreatedAt: 1},
		{ID: "idle-task", Kind: "task", Status: "idle", CreatedAt: 2},
		{ID: "done", Kind: "thread", Status: "completed", CreatedAt: 3},
	}

	active := ActiveDockThreads(threads)
	require.ElementsMatch(t, []string{"idle-thread", "idle-task"}, dockThreadIDs(active))
}

// TestThreadDockStatusWordIdleIsExplicit proves threadDockStatusWord gives
// idle its own word rather than falling through to the raw-status default
// (undifferentiated from any other unhandled status) or reusing a terminal
// word.
func TestThreadDockStatusWordIdleIsExplicit(t *testing.T) {
	t.Parallel()

	word := threadDockStatusWord(proto.ThreadStatusIdle)
	require.NotEmpty(t, word)
	require.NotEqual(t, threadDockStatusWord(proto.ThreadStatusCompleted), word)
	require.NotEqual(t, threadDockStatusWord(proto.ThreadStatusRunning), word)
	require.NotEqual(t, threadDockStatusWord(proto.ThreadStatusFailed), word, "must not fall through to an unhandled-status default indistinguishable from idle")
}

// TestThreadDockGoalHeadline covers the one line a delegation shows
// beside its name. A plain goal keeps its first line; a structured
// prompt - the shape a pipeline skill hands its agents - is read the way
// the chat's own delegation block reads it, so the row says what the job
// is instead of repeating the agent's name back at it.
func TestThreadDockGoalHeadline(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", DockGoalHeadline("dev", ""))
	require.Equal(t, "fix the bug", DockGoalHeadline("dev", "  fix the bug  "))
	require.Equal(t, "first line", DockGoalHeadline("dev", "first line\nsecond line\nthird line"))

	// The case this exists for: the leading label repeats the name, and
	// the line that carries the work is labeled too.
	require.Equal(t, "keep LSP restarts isolated",
		DockGoalHeadline("middle-developer", "ROLE: middle-developer\nTASK: keep LSP restarts isolated\nORIGINAL USER REQUEST:\n..."))

	// Prose with a colon is not scaffolding and stays as written.
	require.Equal(t, "Fix this: the parser drops newlines",
		DockGoalHeadline("dev", "Fix this: the parser drops newlines"))
}

func TestThreadDockStatusLine(t *testing.T) {
	t.Parallel()

	elapsed := 4*time.Minute + 3*time.Second

	// The step count and the in-progress todo are both shown; the todo
	// wins over the last tool call as the activity segment.
	line := DockStatusLine(proto.ThreadStatusRunning, DockActivity{
		InProgressTodo: "writing tests",
		LastTool:       "bash go test ./...",
		MessageCount:   7,
	}, elapsed)
	require.Equal(t, "step 7 · → writing tests · 4m03s", line)

	// Without a todo, the last tool call fills the activity segment.
	line = DockStatusLine(proto.ThreadStatusRunning, DockActivity{
		LastTool:     "Read internal/ui/model/ui.go",
		MessageCount: 7,
	}, elapsed)
	require.Equal(t, "step 7 · → Read internal/ui/model/ui.go · 4m03s", line)

	// Just a step count when there's neither todo nor tool activity.
	line = DockStatusLine(proto.ThreadStatusRunning, DockActivity{
		MessageCount: 7,
	}, elapsed)
	require.Equal(t, "step 7 · 4m03s", line)

	// Falls back to the thread's own status word when there's no activity
	// at all.
	line = DockStatusLine(proto.ThreadStatusRunning, DockActivity{}, elapsed)
	require.Equal(t, "running… · 4m03s", line)

	line = DockStatusLine(proto.ThreadStatusPending, DockActivity{}, elapsed)
	require.Equal(t, "pending · 4m03s", line)

	line = DockStatusLine(proto.ThreadStatusMerging, DockActivity{}, elapsed)
	require.Equal(t, "merging… · 4m03s", line)

	// The elapsed suffix is always present, regardless of branch.
	require.Contains(t, DockStatusLine(proto.ThreadStatusRunning, DockActivity{}, 45*time.Second), "45s")
}

// TestDropActivityDiscardsCachedSnapshot proves the Deleted-event cleanup
// UI.updateThreads performs (dropActivity), now that the shared thread list
// (threads_cache.go) owns removing the row itself: the dock must still
// forget any live activity snapshot for a thread that's gone, or a stale
// snapshot could linger keyed by an ID nothing will ever look up again.
func TestDropActivityDiscardsCachedSnapshot(t *testing.T) {
	t.Parallel()

	c := &DockState{activity: map[string]listcache.TTLCache[DockActivity]{
		"s1": {Value: DockActivity{MessageCount: 3}},
	}}
	c.DropActivity("s1")
	_, ok := c.activity["s1"]
	require.False(t, ok)
}

func TestStaleThreadActivityRefreshCmds(t *testing.T) {
	t.Parallel()

	sess := session.Session{MessageCount: 3}
	ws := &threadsDockTestWorkspace{supported: true, attachWS: &threadsDockTestWorkspace{sess: sess}}
	com := &common.Common{Workspace: ws}

	visible := []proto.Thread{
		{ID: "no-session"},                       // skipped: no session yet
		{ID: "fresh", SessionID: "sess-fresh"},   // skipped: cached and fresh
		{ID: "stale", SessionID: "sess-stale"},   // dispatched: cached but stale
		{ID: "unfetched", SessionID: "sess-new"}, // dispatched: never fetched
	}

	c := &DockState{
		activity: map[string]listcache.TTLCache[DockActivity]{
			"fresh": {Timestamp: time.Now()},
			"stale": {Timestamp: time.Now().Add(-2 * threadsDockActivityTTL)},
		},
	}

	cmds := c.StaleActivityRefreshCmds(com, visible)
	require.Len(t, cmds, 2)
	require.True(t, c.activity["stale"].InFlight)
	require.True(t, c.activity["unfetched"].InFlight)
	require.False(t, c.activity["fresh"].InFlight)
	require.False(t, c.activity["no-session"].InFlight)
}

func TestDispatchThreadActivityRefreshAndApply(t *testing.T) {
	t.Parallel()

	sess := session.Session{
		MessageCount: 5,
		Todos: []session.Todo{
			{Content: "task one", Status: session.TodoStatusCompleted},
			{Content: "task two", Status: session.TodoStatusInProgress, ActiveForm: "doing task two"},
		},
	}
	attached := &threadsDockTestWorkspace{sess: sess, msgs: []message.Message{
		{Parts: []message.ContentPart{
			message.ToolCall{ID: "tc1", Name: "view", Input: `{"file_path":"internal/ui/model/ui.go"}`},
		}},
	}}
	ws := &threadsDockTestWorkspace{supported: true, attachWS: attached}
	com := &common.Common{Workspace: ws}

	c := &DockState{}
	cmd := c.dispatchThreadActivityRefresh(com, "t1", "sess-1")
	require.NotNil(t, cmd)

	msg := cmd()
	loaded, ok := msg.(DockActivityLoadedMsg)
	require.True(t, ok)
	require.Equal(t, "t1", loaded.threadID)
	require.NoError(t, loaded.err)
	require.Equal(t, "doing task two", loaded.activity.InProgressTodo)
	require.Equal(t, int64(5), loaded.activity.MessageCount)
	require.Equal(t, "view internal/ui/model/ui.go", loaded.activity.LastTool)
	require.Equal(t, 1, ws.detachCalls)

	c.activity = map[string]listcache.TTLCache[DockActivity]{"t1": {InFlight: true}}
	c.ApplyActivityLoaded(loaded)
	require.False(t, c.activity["t1"].InFlight)
	require.Equal(t, "doing task two", c.activity["t1"].Value.InProgressTodo)
}

func TestApplyThreadActivityLoadedDiscardsStaleGen(t *testing.T) {
	t.Parallel()

	c := &DockState{activityGen: 2, activity: map[string]listcache.TTLCache[DockActivity]{"t1": {InFlight: true}}}
	c.ApplyActivityLoaded(DockActivityLoadedMsg{
		threadID: "t1",
		gen:      1,
		activity: DockActivity{MessageCount: 9},
	})
	require.False(t, c.activity["t1"].InFlight, "inFlight is always cleared")
	require.Zero(t, c.activity["t1"].Value, "a stale-gen result must not be written through")
}

// dockThreadIDs extracts IDs in order, for asserting ActiveDockThreads'
// filter+sort result concisely.
func dockThreadIDs(threads []proto.Thread) []string {
	ids := make([]string, len(threads))
	for i, t := range threads {
		ids[i] = t.ID
	}
	return ids
}

// threadsDockTestWorkspace is a minimal workspace.Workspace stub for
// exercising the dock's list and activity fetches, following the
// threadsTestWorkspace pattern in threads_cache_test.go.
type threadsDockTestWorkspace struct {
	workspace.Workspace
	threads   []proto.Thread
	err       error
	supported bool

	attachWS    workspace.Workspace
	attachErr   error
	detachCalls int

	sess    session.Session
	sessErr error

	msgs    []message.Message
	msgsErr error

	taskSupported bool
	tasks         []proto.Thread
	taskErr       error
}

func (w *threadsDockTestWorkspace) SupportsThreads() bool { return w.supported }

func (w *threadsDockTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	return w.threads, w.err
}

func (w *threadsDockTestWorkspace) SupportsTasks() bool { return w.taskSupported }

func (w *threadsDockTestWorkspace) ListTasks(context.Context) ([]proto.Thread, error) {
	return w.tasks, w.taskErr
}

func (w *threadsDockTestWorkspace) AttachThread(context.Context, string) (workspace.Workspace, func(), error) {
	return w.attachWS, func() { w.detachCalls++ }, w.attachErr
}

func (w *threadsDockTestWorkspace) GetSession(context.Context, string) (session.Session, error) {
	return w.sess, w.sessErr
}

func (w *threadsDockTestWorkspace) SetCurrentSessionGeneration(context.Context, string, uint64) error {
	return nil
}

func (w *threadsDockTestWorkspace) ListMessages(context.Context, string) ([]message.Message, error) {
	return w.msgs, w.msgsErr
}

func (w *threadsDockTestWorkspace) ListMessagesBySessionIDs(context.Context, string, uint64, []string) (map[string][]message.Message, error) {
	result := make(map[string][]message.Message)
	if w.msgsErr == nil {
		for _, m := range w.msgs {
			result[m.SessionID] = append(result[m.SessionID], m)
		}
	}
	return result, w.msgsErr
}
