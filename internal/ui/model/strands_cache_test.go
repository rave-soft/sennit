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

// strandsTestWorkspace is a minimal workspace.Workspace stub for exercising
// the strand list cache, following the testWorkspace pattern above (embed
// the full interface, override only what's exercised).
type strandsTestWorkspace struct {
	workspace.Workspace
	strands   []proto.Strand
	err       error
	supported bool
	calls     int

	// AttachStrand-specific: kept separate from err/strands so tests that
	// exercise ListStrands don't need to care about them.
	attachWS    workspace.Workspace
	attachErr   error
	detachCalls int
}

func (w *strandsTestWorkspace) SupportsStrands() bool { return w.supported }

func (w *strandsTestWorkspace) ListStrands(context.Context) ([]proto.Strand, error) {
	w.calls++
	return w.strands, w.err
}

// The following StrandController methods round out strandsTestWorkspace for
// root_test.go, which drives the router through attach/merge/remove/create
// rather than just ListStrands.

func (w *strandsTestWorkspace) AttachStrand(context.Context, string) (workspace.Workspace, func(), error) {
	return w.attachWS, func() { w.detachCalls++ }, w.attachErr
}

func (w *strandsTestWorkspace) CreateStrand(context.Context, proto.CreateStrandRequest) (proto.Strand, error) {
	return proto.Strand{}, w.err
}

func (w *strandsTestWorkspace) MergeStrand(context.Context, string) (proto.Strand, error) {
	return proto.Strand{}, w.err
}

func (w *strandsTestWorkspace) RemoveStrand(context.Context, string, proto.RemoveStrandOptions) error {
	return w.err
}

func TestDispatchStrandsRefreshNoopWhenInFlight(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &strandsCacheState{}

	require.NotNil(t, c.dispatchStrandsRefresh(com))
	require.True(t, c.inFlight)
	require.Nil(t, c.dispatchStrandsRefresh(com))
	require.Equal(t, 0, ws.calls, "the cmd wasn't run yet, so the workspace shouldn't have been probed")
}

func TestDispatchStrandsRefreshNoopWhenUnsupported(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: false}
	com := &common.Common{Workspace: ws}
	c := &strandsCacheState{}

	require.Nil(t, c.dispatchStrandsRefresh(com))
	require.False(t, c.inFlight)
}

func TestApplyStrandsLoadedWritesThrough(t *testing.T) {
	t.Parallel()

	want := []proto.Strand{{ID: "s1", Name: "one"}, {ID: "s2", Name: "two"}}
	ws := &strandsTestWorkspace{supported: true, strands: want}
	com := &common.Common{Workspace: ws}
	c := &strandsCacheState{inFlight: true}

	cmds := c.applyStrandsLoaded(com, strandsLoadedMsg{gen: c.gen, strands: want})
	require.Nil(t, cmds)
	require.False(t, c.inFlight)
	require.Equal(t, want, c.strands)
	require.WithinDuration(t, time.Now(), c.checkedAt, time.Second)
}

func TestApplyStrandsLoadedDiscardsStaleGenerationAndRedispatches(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true, strands: []proto.Strand{{ID: "fresh"}}}
	com := &common.Common{Workspace: ws}
	c := &strandsCacheState{inFlight: true, gen: 5}

	// A result carrying an older generation (e.g. an invalidation raced the
	// in-flight fetch) must be discarded, not written through, and a fresh
	// fetch re-dispatched instead.
	cmds := c.applyStrandsLoaded(com, strandsLoadedMsg{gen: 4, strands: []proto.Strand{{ID: "stale"}}})
	require.Len(t, cmds, 1)
	require.Nil(t, c.strands, "stale result must not be written through")
	require.True(t, c.inFlight, "the re-dispatch should mark in-flight again")

	msg := cmds[0]() // run the re-dispatched cmd synchronously
	loaded, ok := msg.(strandsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, c.gen, loaded.gen)
	require.Equal(t, ws.strands, loaded.strands)
}

func TestStrandsCacheFreshness(t *testing.T) {
	t.Parallel()

	c := &strandsCacheState{}
	require.False(t, c.fresh(strandsCacheTTL), "zero-value checkedAt is never fresh")

	c.checkedAt = time.Now()
	require.True(t, c.fresh(strandsCacheTTL))

	c.checkedAt = time.Now().Add(-2 * strandsCacheTTL)
	require.False(t, c.fresh(strandsCacheTTL))
}

func TestInvalidateStrandsBumpsGenAndDiscardsInFlightResult(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &strandsCacheState{}
	c.checkedAt = time.Now()

	cmd := c.dispatchStrandsRefresh(com) // captures gen 0
	require.NotNil(t, cmd)

	c.invalidateStrands() // a concurrent event bumps gen to 1, clears checkedAt
	require.False(t, c.fresh(strandsCacheTTL))

	msg := cmd() // the in-flight fetch (still gen 0) lands
	result, ok := msg.(strandsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, uint64(0), result.gen)

	cmds := c.applyStrandsLoaded(com, result)
	require.Len(t, cmds, 1, "stale-gen result should be discarded and re-dispatched")
	require.Zero(t, c.checkedAt, "the discarded result must not mark the cache fresh again")
}

func TestApplyStrandEventUpsertsCreatedAndUpdated(t *testing.T) {
	t.Parallel()

	c := &strandsCacheState{strands: []proto.Strand{{ID: "s1", Status: "running"}}}
	c.checkedAt = time.Now()

	// Created: no existing row with this ID, so it's appended.
	c.applyStrandEvent(pubsub.Event[proto.Strand]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Strand{ID: "s2", Status: "running"},
	})
	require.Len(t, c.strands, 2)
	require.False(t, c.fresh(strandsCacheTTL), "the event should invalidate the TTL")

	c.checkedAt = time.Now()
	// Updated: existing row with this ID is replaced in place.
	c.applyStrandEvent(pubsub.Event[proto.Strand]{
		Type:    pubsub.UpdatedEvent,
		Payload: proto.Strand{ID: "s1", Status: "merged"},
	})
	require.Len(t, c.strands, 2)
	idx := -1
	for i, s := range c.strands {
		if s.ID == "s1" {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	require.Equal(t, "merged", c.strands[idx].Status)
	require.False(t, c.fresh(strandsCacheTTL))
}

func TestApplyStrandEventRemovesOnDeleted(t *testing.T) {
	t.Parallel()

	c := &strandsCacheState{strands: []proto.Strand{{ID: "s1"}, {ID: "s2"}}}
	c.checkedAt = time.Now()

	c.applyStrandEvent(pubsub.Event[proto.Strand]{
		Type:    pubsub.DeletedEvent,
		Payload: proto.Strand{ID: "s1"},
	})
	require.Equal(t, []proto.Strand{{ID: "s2"}}, c.strands)
	require.False(t, c.fresh(strandsCacheTTL))
}

func TestStaleStrandsRefreshCmdOnlyWhenActiveAndStale(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws}
	c := &strandsCacheState{}

	require.Nil(t, c.staleStrandsRefreshCmd(com, false), "inactive dashboard should never refresh")

	require.NotNil(t, c.staleStrandsRefreshCmd(com, true), "stale cache while active should refresh")

	c.inFlight = false
	c.checkedAt = time.Now()
	require.Nil(t, c.staleStrandsRefreshCmd(com, true), "fresh cache should not refresh")
}

func TestDispatchStrandsRefreshPropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	ws := &strandsTestWorkspace{supported: true, err: wantErr}
	com := &common.Common{Workspace: ws}
	c := &strandsCacheState{}

	cmd := c.dispatchStrandsRefresh(com)
	require.NotNil(t, cmd)
	msg := cmd()
	loaded, ok := msg.(strandsLoadedMsg)
	require.True(t, ok)
	require.Nil(t, loaded.strands, "best-effort: zero value returned alongside the logged error")
}
