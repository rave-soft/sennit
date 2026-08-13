package model

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/workspace"
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

	active := activeDockThreads(threads)
	require.Len(t, active, 3)
	require.Equal(t, []string{"a-pending", "c-merging", "b-running"}, dockThreadIDs(active))
}

func TestVisibleDockThreadsCapsAndReportsMore(t *testing.T) {
	t.Parallel()

	mk := func(n int) []proto.Thread {
		threads := make([]proto.Thread, n)
		for i := range threads {
			threads[i] = proto.Thread{ID: string(rune('a' + i))}
		}
		return threads
	}

	cases := []struct {
		n             int
		wantVisible   int
		wantMoreCount int
	}{
		{0, 0, 0},
		{3, 3, 0},
		{4, 3, 1},
		{10, 3, 7},
	}
	for _, tc := range cases {
		visible, more := visibleDockThreads(mk(tc.n))
		require.Lenf(t, visible, tc.wantVisible, "n=%d", tc.n)
		require.Equalf(t, tc.wantMoreCount, more, "n=%d", tc.n)
	}
}

func TestThreadDockGoalFirstLine(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", threadDockGoalFirstLine(""))
	require.Equal(t, "fix the bug", threadDockGoalFirstLine("  fix the bug  "))
	require.Equal(t, "first line", threadDockGoalFirstLine("first line\nsecond line\nthird line"))
}

func TestThreadDockStatusLine(t *testing.T) {
	t.Parallel()

	elapsed := 4*time.Minute + 3*time.Second

	// The step count and the in-progress todo are both shown; the todo
	// wins over the last tool call as the activity segment.
	line := threadDockStatusLine(thread.StatusRunning, threadDockActivity{
		InProgressTodo: "writing tests",
		LastTool:       "bash go test ./...",
		MessageCount:   7,
	}, elapsed)
	require.Equal(t, "step 7 · → writing tests · 4m03s", line)

	// Without a todo, the last tool call fills the activity segment.
	line = threadDockStatusLine(thread.StatusRunning, threadDockActivity{
		LastTool:     "Read internal/ui/model/ui.go",
		MessageCount: 7,
	}, elapsed)
	require.Equal(t, "step 7 · → Read internal/ui/model/ui.go · 4m03s", line)

	// Just a step count when there's neither todo nor tool activity.
	line = threadDockStatusLine(thread.StatusRunning, threadDockActivity{
		MessageCount: 7,
	}, elapsed)
	require.Equal(t, "step 7 · 4m03s", line)

	// Falls back to the thread's own status word when there's no activity
	// at all.
	line = threadDockStatusLine(thread.StatusRunning, threadDockActivity{}, elapsed)
	require.Equal(t, "running… · 4m03s", line)

	line = threadDockStatusLine(thread.StatusPending, threadDockActivity{}, elapsed)
	require.Equal(t, "pending · 4m03s", line)

	line = threadDockStatusLine(thread.StatusMerging, threadDockActivity{}, elapsed)
	require.Equal(t, "merging… · 4m03s", line)

	// The elapsed suffix is always present, regardless of branch.
	require.Contains(t, threadDockStatusLine(thread.StatusRunning, threadDockActivity{}, 45*time.Second), "45s")
}

func TestApplyThreadEventInvalidatesThreadsDock(t *testing.T) {
	t.Parallel()

	c := &threadsDockState{}
	c.checkedAt = time.Now()
	gen := c.gen

	c.applyThreadEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.UpdatedEvent,
		Payload: proto.Thread{ID: "s1"},
	})

	require.False(t, c.fresh(threadsDockTTL))
	require.Greater(t, c.gen, gen)
}

func TestInvalidateThreadsDockDiscardsInFlightResult(t *testing.T) {
	t.Parallel()

	ws := &threadsDockTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadsDockState{}
	c.checkedAt = time.Now()

	cmd := c.dispatchThreadsDockRefresh(com) // captures gen 0
	require.NotNil(t, cmd)

	c.invalidateThreadsDock() // a concurrent event bumps gen, clears checkedAt

	msg := cmd() // the in-flight fetch (still gen 0) lands
	loaded, ok := msg.(threadsDockLoadedMsg)
	require.True(t, ok)
	require.Equal(t, uint64(0), loaded.gen)

	redispatch := c.applyThreadsDockLoaded(com, loaded)
	require.NotNil(t, redispatch, "stale-gen result should be discarded and re-dispatched")
	require.Zero(t, c.checkedAt, "the discarded result must not mark the cache fresh again")
}

func TestApplyThreadsDockLoadedBumpsActivityGen(t *testing.T) {
	t.Parallel()

	ws := &threadsDockTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadsDockState{}
	activityGen := c.activityGen

	cmd := c.applyThreadsDockLoaded(com, threadsDockLoadedMsg{gen: c.gen, threads: []proto.Thread{{ID: "s1"}}})
	require.Nil(t, cmd)
	require.Greater(t, c.activityGen, activityGen, "a new thread list should invalidate stale per-thread activity")
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

	c := &threadsDockState{
		activity: map[string]threadDockActivity{
			"fresh": {FetchedAt: time.Now()},
			"stale": {FetchedAt: time.Now().Add(-2 * threadsDockActivityTTL)},
		},
	}

	cmds := c.staleThreadActivityRefreshCmds(com, visible)
	require.Len(t, cmds, 2)
	require.True(t, c.activityInFlight["stale"])
	require.True(t, c.activityInFlight["unfetched"])
	require.False(t, c.activityInFlight["fresh"])
	require.False(t, c.activityInFlight["no-session"])
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

	c := &threadsDockState{}
	cmd := c.dispatchThreadActivityRefresh(com, "t1", "sess-1")
	require.NotNil(t, cmd)

	msg := cmd()
	loaded, ok := msg.(threadDockActivityLoadedMsg)
	require.True(t, ok)
	require.Equal(t, "t1", loaded.threadID)
	require.NoError(t, loaded.err)
	require.Equal(t, "doing task two", loaded.activity.InProgressTodo)
	require.Equal(t, int64(5), loaded.activity.MessageCount)
	require.Equal(t, "view internal/ui/model/ui.go", loaded.activity.LastTool)
	require.Equal(t, 1, ws.detachCalls)

	c.activityInFlight = map[string]bool{"t1": true}
	c.applyThreadActivityLoaded(loaded)
	require.False(t, c.activityInFlight["t1"])
	require.Equal(t, "doing task two", c.activity["t1"].InProgressTodo)
}

func TestApplyThreadActivityLoadedDiscardsStaleGen(t *testing.T) {
	t.Parallel()

	c := &threadsDockState{activityGen: 2, activityInFlight: map[string]bool{"t1": true}}
	c.applyThreadActivityLoaded(threadDockActivityLoadedMsg{
		threadID: "t1",
		gen:      1,
		activity: threadDockActivity{MessageCount: 9},
	})
	require.False(t, c.activityInFlight["t1"], "inFlight is always cleared")
	require.Nil(t, c.activity, "a stale-gen result must not be written through")
}

// dockThreadIDs extracts IDs in order, for asserting activeDockThreads'
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
}

func (w *threadsDockTestWorkspace) SupportsThreads() bool { return w.supported }

func (w *threadsDockTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	return w.threads, w.err
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
