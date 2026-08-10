package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// threadsTestWorkspace is a minimal workspace.Workspace stub for exercising
// the thread list cache, following the testWorkspace pattern above (embed
// the full interface, override only what's exercised).
type threadsTestWorkspace struct {
	workspace.Workspace
	threads   []proto.Thread
	err       error
	supported bool
	calls     int

	// AttachThread-specific: kept separate from err/threads so tests that
	// exercise ListThreads don't need to care about them.
	attachWS    workspace.Workspace
	attachErr   error
	detachCalls int
}

func (w *threadsTestWorkspace) SupportsThreads() bool { return w.supported }

func (w *threadsTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	w.calls++
	return w.threads, w.err
}

// The following ThreadController methods round out threadsTestWorkspace for
// root_test.go, which drives the router through attach/merge/remove/create
// rather than just ListThreads.

func (w *threadsTestWorkspace) AttachThread(context.Context, string) (workspace.Workspace, func(), error) {
	return w.attachWS, func() { w.detachCalls++ }, w.attachErr
}

func (w *threadsTestWorkspace) CreateThread(context.Context, proto.CreateThreadRequest) (proto.Thread, error) {
	return proto.Thread{}, w.err
}

func (w *threadsTestWorkspace) MergeThread(context.Context, string) (proto.Thread, error) {
	return proto.Thread{}, w.err
}

func (w *threadsTestWorkspace) RemoveThread(context.Context, string, proto.RemoveThreadOptions) error {
	return w.err
}

func TestDispatchThreadsRefreshNoopWhenInFlight(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadsCacheState{}

	require.NotNil(t, c.dispatchThreadsRefresh(com))
	require.True(t, c.inFlight)
	require.Nil(t, c.dispatchThreadsRefresh(com))
	require.Equal(t, 0, ws.calls, "the cmd wasn't run yet, so the workspace shouldn't have been probed")
}

func TestDispatchThreadsRefreshNoopWhenUnsupported(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: false}
	com := &common.Common{Workspace: ws}
	c := &threadsCacheState{}

	require.Nil(t, c.dispatchThreadsRefresh(com))
	require.False(t, c.inFlight)
}

func TestApplyThreadsLoadedWritesThrough(t *testing.T) {
	t.Parallel()

	want := []proto.Thread{{ID: "s1", Name: "one"}, {ID: "s2", Name: "two"}}
	ws := &threadsTestWorkspace{supported: true, threads: want}
	com := &common.Common{Workspace: ws}
	c := &threadsCacheState{inFlight: true}

	cmds := c.applyThreadsLoaded(com, threadsLoadedMsg{gen: c.gen, threads: want})
	require.Nil(t, cmds)
	require.False(t, c.inFlight)
	require.Equal(t, want, c.threads)
	require.WithinDuration(t, time.Now(), c.checkedAt, time.Second)
}

func TestApplyThreadsLoadedDiscardsStaleGenerationAndRedispatches(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true, threads: []proto.Thread{{ID: "fresh"}}}
	com := &common.Common{Workspace: ws}
	c := &threadsCacheState{inFlight: true, gen: 5}

	// A result carrying an older generation (e.g. an invalidation raced the
	// in-flight fetch) must be discarded, not written through, and a fresh
	// fetch re-dispatched instead.
	cmds := c.applyThreadsLoaded(com, threadsLoadedMsg{gen: 4, threads: []proto.Thread{{ID: "stale"}}})
	require.Len(t, cmds, 1)
	require.Nil(t, c.threads, "stale result must not be written through")
	require.True(t, c.inFlight, "the re-dispatch should mark in-flight again")

	msg := cmds[0]() // run the re-dispatched cmd synchronously
	loaded, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, c.gen, loaded.gen)
	require.Equal(t, ws.threads, loaded.threads)
}

func TestThreadsCacheFreshness(t *testing.T) {
	t.Parallel()

	c := &threadsCacheState{}
	require.False(t, c.fresh(threadsCacheTTL), "zero-value checkedAt is never fresh")

	c.checkedAt = time.Now()
	require.True(t, c.fresh(threadsCacheTTL))

	c.checkedAt = time.Now().Add(-2 * threadsCacheTTL)
	require.False(t, c.fresh(threadsCacheTTL))
}

func TestInvalidateThreadsBumpsGenAndDiscardsInFlightResult(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadsCacheState{}
	c.checkedAt = time.Now()

	cmd := c.dispatchThreadsRefresh(com) // captures gen 0
	require.NotNil(t, cmd)

	c.invalidateThreads() // a concurrent event bumps gen to 1, clears checkedAt
	require.False(t, c.fresh(threadsCacheTTL))

	msg := cmd() // the in-flight fetch (still gen 0) lands
	result, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, uint64(0), result.gen)

	cmds := c.applyThreadsLoaded(com, result)
	require.Len(t, cmds, 1, "stale-gen result should be discarded and re-dispatched")
	require.Zero(t, c.checkedAt, "the discarded result must not mark the cache fresh again")
}

func TestApplyThreadEventUpsertsCreatedAndUpdated(t *testing.T) {
	t.Parallel()

	c := &threadsCacheState{threads: []proto.Thread{{ID: "s1", Status: "running"}}}
	c.checkedAt = time.Now()

	// Created: no existing row with this ID, so it's appended.
	c.applyThreadEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Thread{ID: "s2", Status: "running"},
	})
	require.Len(t, c.threads, 2)
	require.False(t, c.fresh(threadsCacheTTL), "the event should invalidate the TTL")

	c.checkedAt = time.Now()
	// Updated: existing row with this ID is replaced in place.
	c.applyThreadEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.UpdatedEvent,
		Payload: proto.Thread{ID: "s1", Status: "merged"},
	})
	require.Len(t, c.threads, 2)
	idx := -1
	for i, s := range c.threads {
		if s.ID == "s1" {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	require.Equal(t, "merged", c.threads[idx].Status)
	require.False(t, c.fresh(threadsCacheTTL))
}

func TestApplyThreadEventRemovesOnDeleted(t *testing.T) {
	t.Parallel()

	c := &threadsCacheState{threads: []proto.Thread{{ID: "s1"}, {ID: "s2"}}}
	c.checkedAt = time.Now()

	c.applyThreadEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.DeletedEvent,
		Payload: proto.Thread{ID: "s1"},
	})
	require.Equal(t, []proto.Thread{{ID: "s2"}}, c.threads)
	require.False(t, c.fresh(threadsCacheTTL))
}

func TestStaleThreadsRefreshCmdOnlyWhenActiveAndStale(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadsCacheState{}

	require.Nil(t, c.staleThreadsRefreshCmd(com, false), "inactive dashboard should never refresh")

	require.NotNil(t, c.staleThreadsRefreshCmd(com, true), "stale cache while active should refresh")

	c.inFlight = false
	c.checkedAt = time.Now()
	require.Nil(t, c.staleThreadsRefreshCmd(com, true), "fresh cache should not refresh")
}

func TestDispatchThreadsRefreshPropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	ws := &threadsTestWorkspace{supported: true, err: wantErr}
	com := &common.Common{Workspace: ws}
	c := &threadsCacheState{}

	cmd := c.dispatchThreadsRefresh(com)
	require.NotNil(t, cmd)
	msg := cmd()
	loaded, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Nil(t, loaded.threads, "best-effort: zero value returned alongside the logged error")
}
