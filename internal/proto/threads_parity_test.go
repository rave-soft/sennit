package proto_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/thread"
)

// TestThreadStatusParityWithDomain holds proto's copy of the thread status
// predicates against the domain's own. The two cannot share a definition —
// proto is the transport boundary and deliberately does not depend on
// internal/thread — so the copy is the design, and this is what keeps it
// honest: a status added on one side and forgotten on the other silently
// changes what the CLI and the dashboard treat as finished.
func TestThreadStatusParityWithDomain(t *testing.T) {
	t.Parallel()

	statuses := []struct {
		proto  proto.ThreadStatus
		domain thread.Status
	}{
		{proto.ThreadStatusPending, thread.StatusPending},
		{proto.ThreadStatusRunning, thread.StatusRunning},
		{proto.ThreadStatusIdle, thread.StatusIdle},
		{proto.ThreadStatusMerging, thread.StatusMerging},
		{proto.ThreadStatusCompleted, thread.StatusCompleted},
		{proto.ThreadStatusMerged, thread.StatusMerged},
		{proto.ThreadStatusFailed, thread.StatusFailed},
		{proto.ThreadStatusInterrupted, thread.StatusInterrupted},
		{proto.ThreadStatusCancelled, thread.StatusCancelled},
		{proto.ThreadStatusConflict, thread.StatusConflict},
		{proto.ThreadStatusMergeBlocked, thread.StatusMergeBlocked},
	}

	for _, s := range statuses {
		require.Equal(t, string(s.domain), string(s.proto), "the wire value must match the domain's")
		require.Equal(t, s.domain.Active(), s.proto.Active(), "Active must agree for %q", s.proto)
		require.Equal(t, s.domain.Terminal(), s.proto.Terminal(), "Terminal must agree for %q", s.proto)
	}

	// An unknown status is neither, on both sides — that is what keeps a
	// newer version's status from being treated as finished by an older
	// binary sharing the same database.
	require.False(t, proto.ThreadStatus("from-the-future").Active())
	require.False(t, proto.ThreadStatus("from-the-future").Terminal())
	require.False(t, thread.Status("from-the-future").Active())
	require.False(t, thread.Status("from-the-future").Terminal())
}
