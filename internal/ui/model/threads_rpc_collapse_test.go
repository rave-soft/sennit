package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/threads"
	"github.com/stretchr/testify/require"
)

// TestThreadEventDispatchesOneListThreadsCall is the point of the
// threads_cache.go/threads_dock.go/thread_indicator.go collapse: before it,
// a single pubsub.Event[proto.Thread] could trigger three independent
// ListThreads round trips — one each from the header badge's indicator
// cache, the session panel's dock cache, and (while open) the threads
// dashboard's own cache. They now all read threads.ListCache, so the event
// costs exactly one.
//
// The scenario is deliberately the worst case for the old code: the
// dashboard is open (so its old cache would have redispatched on the
// event) at the same time as the main screen is on uiChat (so the old
// dock/indicator caches would each have redispatched too).
func TestThreadEventDispatchesOneListThreadsCall(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	ws := r.com.Workspace.(*rootTestWorkspace)

	r.main.state = uiChat
	r.dashboard = threads.New(r.com, &r.main.threadList)
	// Becoming active dispatches its own refresh when the cache is not
	// fresh. Drain it and reset the counter, so what is measured below is
	// the event's cost and not the dashboard's arrival — dropping the
	// command instead would leave the cache marked in flight and the
	// event would dispatch nothing at all.
	drainThreadRefresh(t, r, r.dashboard.SetActive(true))
	ws.listThreadsCalls = 0
	r.active = screenDashboard

	_, cmd := r.Update(pubsub.Event[proto.Thread]{
		Type:    pubsub.UpdatedEvent,
		Payload: proto.Thread{ID: "t1", Status: "running"},
	})
	drainThreadRefresh(t, r, cmd)

	require.Equal(t, 1, ws.listThreadsCalls,
		"one thread event must cost exactly one ListThreads round trip, not one per consumer")
}

// drainThreadRefresh runs cmd the way the Bubble Tea runtime would
// (unwrapping tea.BatchMsg), feeding threads.LoadedMsg results back into
// r.Update so a stale-generation re-dispatch (if any) also runs. Other leaf
// messages are executed for their side effects and dropped.
func drainThreadRefresh(t *testing.T, r *Root, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := safeRunCmd(cmd)
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			drainThreadRefresh(t, r, c)
		}
	case threads.LoadedMsg:
		_, next := r.Update(msg)
		drainThreadRefresh(t, r, next)
	}
}
