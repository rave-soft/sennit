package model

import (
	"errors"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// These are the regression tests for a spin that made the whole TUI look
// frozen: a thread refresh that fails records nothing, so it is never
// "fresh", so the next Update dispatches it again — and the failure's own
// result message is itself an Update. Observed in the wild at ~830
// attempts a second against a read-only workspace whose AttachThread can
// only ever refuse, filling 10MB log files with one repeated line while
// tools, sub-agents and tasks appeared to stop on their own.

// TestThreadActivityRefreshBacksOffAfterFailure is the core case: the
// per-thread activity probe. Without the fix the second
// staleThreadActivityRefreshCmds call re-dispatches immediately, which is
// the spin.
func TestThreadActivityRefreshBacksOffAfterFailure(t *testing.T) {
	t.Parallel()

	ws := &threadsDockTestWorkspace{supported: true, attachWS: &threadsDockTestWorkspace{}}
	com := &common.Common{Workspace: ws}
	visible := []proto.Thread{{ID: "t1", SessionID: "sess-1"}}

	c := &threadsDockState{}
	cmds := c.staleThreadActivityRefreshCmds(com, visible)
	require.Len(t, cmds, 1, "the first probe should be dispatched")

	entry := c.activity["t1"]
	c.applyThreadActivityLoaded(threadDockActivityLoadedMsg{
		threadID: "t1",
		gen:      c.activityGen,
		entryGen: entry.generation,
		err:      errors.New("read-only workspace: AttachThread is not allowed"),
	})
	require.False(t, c.activity["t1"].inFlight, "a failed probe must still clear its in-flight marker")

	require.Empty(t, c.staleThreadActivityRefreshCmds(com, visible),
		"a probe that just failed must not be re-dispatched on the next Update")

	// Once the backoff has elapsed, the probe is tried again: this is a
	// hold-off, not a permanent giveup.
	expired := c.activity["t1"]
	expired.failedAt = time.Now().Add(-2 * listRefreshBackoff)
	c.activity["t1"] = expired
	require.Len(t, c.staleThreadActivityRefreshCmds(com, visible), 1,
		"the probe must resume once the backoff window has passed")
}

// TestThreadActivitySuccessClearsBackoff proves the hold-off is tied to
// failing, not to having ever failed: a workspace that starts refusing and
// later accepts must go back to its normal TTL cadence.
func TestThreadActivitySuccessClearsBackoff(t *testing.T) {
	t.Parallel()

	c := &threadsDockState{activity: map[string]ttlCache[threadDockActivity]{
		"t1": {inFlight: true, failedAt: time.Now()},
	}}
	c.applyThreadActivityLoaded(threadDockActivityLoadedMsg{
		threadID: "t1",
		gen:      c.activityGen,
		entryGen: c.activity["t1"].generation,
		activity: threadDockActivity{MessageCount: 4},
	})

	settled := c.activity["t1"]
	require.False(t, settled.backingOff(listRefreshBackoff),
		"a successful probe must clear the recorded failure")
	require.True(t, settled.fresh(threadsDockActivityTTL))
	require.Equal(t, int64(4), c.activity["t1"].value.MessageCount)
}

// TestThreadActivityStaleGenerationFailureIsNotRecorded: a failure that
// belongs to a superseded request must not hold back the invalidation that
// superseded it.
func TestThreadActivityStaleGenerationFailureIsNotRecorded(t *testing.T) {
	t.Parallel()

	c := &threadsDockState{activity: map[string]ttlCache[threadDockActivity]{
		"t1": {inFlight: true, generation: 3},
	}}
	c.applyThreadActivityLoaded(threadDockActivityLoadedMsg{
		threadID: "t1",
		gen:      c.activityGen,
		entryGen: 1,
		err:      errors.New("boom"),
	})

	require.False(t, c.activity["t1"].inFlight)
	settled := c.activity["t1"]
	require.False(t, settled.backingOff(listRefreshBackoff),
		"a stale-generation failure must not hold back the newer request that replaced it")
}

// The dock's and indicator's own list-refresh backoff tests moved to
// threads_cache_test.go (TestApplyThreadsLoadedErrorBacksOff,
// TestApplyThreadsLoadedStaleGenerationFailureRedispatches): both caches
// (and the dashboard's) collapsed onto the single threadListCache in
// threads_cache.go, so there is exactly one list-refresh backoff path left
// to pin instead of three copies of it.
