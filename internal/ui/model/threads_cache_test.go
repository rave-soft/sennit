package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
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

	// Task-specific, kept separate from threads/err so tests that only
	// exercise ListThreads don't need to care about them.
	taskSupported bool
	tasks         []proto.Thread
	taskErr       error
	taskCalls     int

	// cancelTaskCalls records every id CancelTask was invoked with, so
	// tests can assert both "called exactly once" and "called with the
	// right id, not some other delegation's".
	cancelTaskCalls []string
	cancelTaskErr   error

	// cancelThreadCalls is CancelTask's sibling for CancelThread, kept
	// separate so a test can assert the router picked the *right* one of
	// the two for a given delegation's kind.
	cancelThreadCalls []string
	cancelThreadErr   error
}

func (w *threadsTestWorkspace) SupportsThreads() bool { return w.supported }

func (w *threadsTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	w.calls++
	return w.threads, w.err
}

func (w *threadsTestWorkspace) SupportsTasks() bool { return w.taskSupported }

func (w *threadsTestWorkspace) ListTasks(context.Context) ([]proto.Thread, error) {
	w.taskCalls++
	return w.tasks, w.taskErr
}

func (w *threadsTestWorkspace) CancelTask(_ context.Context, id, _ string) error {
	w.cancelTaskCalls = append(w.cancelTaskCalls, id)
	return w.cancelTaskErr
}

func (w *threadsTestWorkspace) CancelThread(_ context.Context, id, _ string) error {
	w.cancelThreadCalls = append(w.cancelThreadCalls, id)
	return w.cancelThreadErr
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
	c := &threadListCache{}

	require.NotNil(t, c.dispatchRefresh(com))
	require.True(t, c.cache.inFlight)
	require.Nil(t, c.dispatchRefresh(com))
	require.Equal(t, 0, ws.calls, "the cmd wasn't run yet, so the workspace shouldn't have been probed")
}

func TestDispatchThreadsRefreshNoopWhenUnsupported(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: false}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	require.Nil(t, c.dispatchRefresh(com))
	require.False(t, c.cache.inFlight)
}

func TestApplyThreadsLoadedWritesThrough(t *testing.T) {
	t.Parallel()

	want := []proto.Thread{{ID: "s1", Name: "one"}, {ID: "s2", Name: "two"}}
	ws := &threadsTestWorkspace{supported: true, threads: want}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{cache: ttlCache[[]proto.Thread]{inFlight: true}}

	cmds, applied := c.applyLoaded(com, threadsLoadedMsg{gen: c.cache.generation, threads: want})
	require.Nil(t, cmds)
	require.True(t, applied)
	require.False(t, c.cache.inFlight)
	require.Equal(t, want, c.cache.value)
	require.WithinDuration(t, time.Now(), c.cache.timestamp, time.Second)
}

func TestApplyThreadsLoadedDiscardsStaleGenerationAndRedispatches(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true, threads: []proto.Thread{{ID: "fresh"}}}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{cache: ttlCache[[]proto.Thread]{inFlight: true, generation: 5}}

	// A result carrying an older generation (e.g. an invalidation raced the
	// in-flight fetch) must be discarded, not written through, and a fresh
	// fetch re-dispatched instead.
	cmds, applied := c.applyLoaded(com, threadsLoadedMsg{gen: 4, threads: []proto.Thread{{ID: "stale"}}})
	require.Len(t, cmds, 1)
	require.False(t, applied)
	require.Nil(t, c.cache.value, "stale result must not be written through")
	require.True(t, c.cache.inFlight, "the re-dispatch should mark in-flight again")

	msg := cmds[0]() // run the re-dispatched cmd synchronously
	loaded, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, c.cache.generation, loaded.gen)
	require.Equal(t, ws.threads, loaded.threads)
}

func TestThreadsCacheFreshness(t *testing.T) {
	t.Parallel()

	c := &threadListCache{}
	require.False(t, c.cache.fresh(threadsCacheTTL), "zero-value checkedAt is never fresh")

	c.cache.timestamp = time.Now()
	require.True(t, c.cache.fresh(threadsCacheTTL))

	c.cache.timestamp = time.Now().Add(-2 * threadsCacheTTL)
	require.False(t, c.cache.fresh(threadsCacheTTL))
}

func TestInvalidateThreadsBumpsGenAndDiscardsInFlightResult(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}
	c.cache.timestamp = time.Now()

	cmd := c.dispatchRefresh(com) // captures gen 0
	require.NotNil(t, cmd)

	c.invalidate() // a concurrent event bumps gen to 1, clears checkedAt
	require.False(t, c.cache.fresh(threadsCacheTTL))

	msg := cmd() // the in-flight fetch (still gen 0) lands
	result, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, uint64(0), result.gen)

	cmds, applied := c.applyLoaded(com, result)
	require.Len(t, cmds, 1, "stale-gen result should be discarded and re-dispatched")
	require.False(t, applied)
	require.Zero(t, c.cache.timestamp, "the discarded result must not mark the cache fresh again")
}

func TestApplyThreadEventUpsertsCreatedAndUpdated(t *testing.T) {
	t.Parallel()

	c := &threadListCache{cache: ttlCache[[]proto.Thread]{value: []proto.Thread{{ID: "s1", Status: "running"}}}}
	c.cache.timestamp = time.Now()

	// Created: no existing row with this ID, so it's appended.
	c.applyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Thread{ID: "s2", Status: "running"},
	})
	require.Len(t, c.cache.value, 2)
	require.False(t, c.cache.fresh(threadsCacheTTL), "the event should invalidate the TTL")

	c.cache.timestamp = time.Now()
	// Updated: existing row with this ID is replaced in place.
	c.applyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.UpdatedEvent,
		Payload: proto.Thread{ID: "s1", Status: "merged"},
	})
	require.Len(t, c.cache.value, 2)
	idx := -1
	for i, s := range c.cache.value {
		if s.ID == "s1" {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	require.Equal(t, "merged", c.cache.value[idx].Status)
	require.False(t, c.cache.fresh(threadsCacheTTL))
}

func TestApplyThreadEventRemovesOnDeleted(t *testing.T) {
	t.Parallel()

	c := &threadListCache{cache: ttlCache[[]proto.Thread]{value: []proto.Thread{{ID: "s1"}, {ID: "s2"}}}}
	c.cache.timestamp = time.Now()

	c.applyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.DeletedEvent,
		Payload: proto.Thread{ID: "s1"},
	})
	require.Equal(t, []proto.Thread{{ID: "s2"}}, c.cache.value)
	require.False(t, c.cache.fresh(threadsCacheTTL))
}

func TestStaleThreadsRefreshCmdOnlyWhenActiveAndStale(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	require.Nil(t, c.staleRefreshCmd(com, false), "inactive dashboard should never refresh")

	require.NotNil(t, c.staleRefreshCmd(com, true), "stale cache while active should refresh")

	c.cache.inFlight = false
	c.cache.timestamp = time.Now()
	require.Nil(t, c.staleRefreshCmd(com, true), "fresh cache should not refresh")
}

func TestApplyThreadsLoadedErrorPreservesCachedValue(t *testing.T) {
	t.Parallel()

	want := []proto.Thread{{ID: "known-good"}}
	c := &threadListCache{cache: ttlCache[[]proto.Thread]{value: want, timestamp: time.Now(), inFlight: true}}

	cmds, applied := c.applyLoaded(nil, threadsLoadedMsg{err: errors.New("boom")})
	require.Empty(t, cmds)
	require.False(t, applied)
	require.Equal(t, want, c.cache.value)
	require.False(t, c.cache.inFlight)
}

// TestApplyThreadsLoadedErrorBacksOff proves the fix for the divergence
// this cache used to have from its dock/indicator siblings (see
// threads_cache.go's package doc comment): an error must record a failure
// so staleRefreshCmd backs off, instead of leaving no record at all and
// re-dispatching on every Update.
func TestApplyThreadsLoadedErrorBacksOff(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true, err: errors.New("boom")}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	cmd := c.staleRefreshCmd(com, true)
	require.NotNil(t, cmd)
	loaded, ok := cmd().(threadsLoadedMsg)
	require.True(t, ok)
	require.Error(t, loaded.err)

	cmds, applied := c.applyLoaded(com, loaded)
	require.Empty(t, cmds, "a failed refresh must not immediately re-dispatch itself")
	require.False(t, applied)

	require.Nil(t, c.staleRefreshCmd(com, true),
		"a refresh that just failed must not be re-dispatched on the next Update")
}

// TestApplyThreadsLoadedStaleGenerationFailureRedispatches keeps the
// backoff fix from swallowing a superseded refresh's redispatch: a failure
// whose generation was already invalidated must still be retried
// immediately, not held back by a backoff that belongs to a stale request.
func TestApplyThreadsLoadedStaleGenerationFailureRedispatches(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true, err: errors.New("boom")}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	cmd := c.dispatchRefresh(com) // captures generation 0
	require.NotNil(t, cmd)
	loaded := cmd().(threadsLoadedMsg)

	c.invalidate() // bumps the generation while the fetch is out

	cmds, applied := c.applyLoaded(com, loaded)
	require.NotEmpty(t, cmds, "a superseded failure must still re-dispatch the authoritative refresh")
	require.False(t, applied)
	require.False(t, c.cache.backingOff(threadsRefreshBackoff),
		"and it must not record a backoff that would stall that re-dispatch")
}

// TestStaleRefreshCmdStopsPollingWhenEmpty proves the other half of the
// divergence fix: once a refresh lands empty, staleRefreshCmd must not
// re-poll forever for a project with no threads at all — only an
// invalidation (a thread event, or an explicit call) re-arms it.
func TestStaleRefreshCmdStopsPollingWhenEmpty(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}
	c.cache.set(nil)

	require.Nil(t, c.staleRefreshCmd(com, true), "an empty, fetched list must not be re-polled")

	c.invalidate()
	require.NotNil(t, c.staleRefreshCmd(com, true), "an invalidation re-arms it")
}

// The threads list is threads only. A task is the `agent` tool's own
// delegation: it renders inline in the chat that started it, it is never
// merged, and nothing removed a finished one from the shared table — so
// merging them here only buried the threads this screen is about.
func TestDispatchThreadsRefreshExcludesTasks(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{
		supported:     true,
		threads:       []proto.Thread{{ID: "thr-1", Name: "a-thread", Kind: "thread"}},
		taskSupported: true,
		tasks:         []proto.Thread{{ID: "task-1", Name: "a-task", Kind: "task"}},
	}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	cmd := c.dispatchRefresh(com)
	require.NotNil(t, cmd)
	msg := cmd()
	loaded, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Zero(t, ws.taskCalls, "the threads list must not pay for a task round trip it does not use")
	require.Equal(t, []proto.Thread{
		{ID: "thr-1", Name: "a-thread", Kind: "thread"},
	}, loaded.threads)
}

func TestDispatchThreadsRefreshIgnoresTaskSupport(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{
		supported:     true,
		threads:       []proto.Thread{{ID: "thr-1"}},
		taskSupported: false,
	}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	msg := c.dispatchRefresh(com)()
	loaded, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Zero(t, ws.taskCalls, "ListTasks is not part of this fetch at all")
	require.Equal(t, []proto.Thread{{ID: "thr-1"}}, loaded.threads)
}

func TestDispatchThreadsRefreshTaskFailureKeepsThreads(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{
		supported:     true,
		threads:       []proto.Thread{{ID: "thr-1"}},
		taskSupported: true,
		taskErr:       errors.New("boom"),
	}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	msg := c.dispatchRefresh(com)()
	loaded, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.NoError(t, loaded.err, "a ListTasks failure alone must not fail the whole refresh")
	require.Equal(t, []proto.Thread{{ID: "thr-1"}}, loaded.threads)
}

func TestDispatchThreadsRefreshPropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	ws := &threadsTestWorkspace{supported: true, err: wantErr}
	com := &common.Common{Workspace: ws}
	c := &threadListCache{}

	cmd := c.dispatchRefresh(com)
	require.NotNil(t, cmd)
	msg := cmd()
	loaded, ok := msg.(threadsLoadedMsg)
	require.True(t, ok)
	require.Nil(t, loaded.threads, "best-effort: zero value returned alongside the logged error")
}

// A task's lifecycle event must not write a row into the threads cache.
//
// Tasks share the delegations table and the lifecycle that publishes these
// events, so filtering the fetch was not enough: a task's own create or
// status event went straight into the cache, around the kind-scoped query,
// and stayed there until a full refresh replaced the slice.
func TestApplyThreadEventIgnoresTasks(t *testing.T) {
	t.Parallel()

	c := &threadListCache{}
	c.cache.set([]proto.Thread{{ID: "thr-1", Kind: "thread"}})

	c.applyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Thread{ID: "task-1", Kind: "task", Status: "running"},
	})

	require.Equal(t, []proto.Thread{{ID: "thr-1", Kind: "thread"}}, c.cache.value,
		"a task must not appear in a list scoped to threads")
}

// A thread's own event still writes through, which is what makes the list
// update before the next refresh lands.
func TestApplyThreadEventUpsertsThreads(t *testing.T) {
	t.Parallel()

	c := &threadListCache{}
	c.cache.set([]proto.Thread{{ID: "thr-1", Kind: "thread", Status: "running"}})

	c.applyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.UpdatedEvent,
		Payload: proto.Thread{ID: "thr-1", Kind: "thread", Status: "completed"},
	})
	c.applyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Thread{ID: "thr-2", Kind: "thread"},
	})

	require.Equal(t, []proto.Thread{
		{ID: "thr-1", Kind: "thread", Status: "completed"},
		{ID: "thr-2", Kind: "thread"},
	}, c.cache.value)
}

// A payload from before Kind existed describes a thread: the field is
// additive, so an empty value must not be read as "not a thread".
func TestApplyThreadEventTreatsEmptyKindAsThread(t *testing.T) {
	t.Parallel()

	c := &threadListCache{}
	c.applyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Thread{ID: "thr-1"},
	})

	require.Equal(t, []proto.Thread{{ID: "thr-1"}}, c.cache.value)
}
