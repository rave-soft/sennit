package gather_test

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/stats/gather"
	"github.com/stretchr/testify/require"
)

// Latency is opt-in: the default `sennit stat` view must not pay for a
// query it does not render.
func TestGather_LatencyIsOptIn(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		latency: []db.ListLatencyEventsSinceRow{{Kind: "steering_fold", WaitedMs: 12}},
	}

	off, err := gather.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeProject, ProjectPath: "/p"})
	require.NoError(t, err)
	require.Empty(t, off.Latency)

	on, err := gather.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeProject, ProjectPath: "/p", WithLatency: true})
	require.NoError(t, err)
	require.Len(t, on.Latency, 1)
	require.Equal(t, int64(12), on.Latency[0].P50MS)
}

// The global scope reads the unscoped query, not the project-scoped one:
// asking for every project and getting one project's numbers back would
// be wrong in the direction that looks plausible.
func TestGather_LatencyGlobalScopeReadsAllProjects(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		latency:    []db.ListLatencyEventsSinceRow{{Kind: "steering_fold", WaitedMs: 12}},
		allLatency: []db.ListAllLatencyEventsSinceRow{{Kind: "steering_fold", WaitedMs: 34}, {Kind: "completion_delivery", WaitedMs: 56}},
	}

	snap, err := gather.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeGlobal, WithLatency: true})
	require.NoError(t, err)
	require.Len(t, snap.Latency, 2)
	require.Equal(t, int64(56), snap.Latency[0].MaxMS)
	require.Equal(t, int64(34), snap.Latency[1].MaxMS)
}

// A session's own handful of samples answers no question a reader can
// act on, so the session scope skips the query entirely even when asked.
func TestGather_LatencySkippedForSessionScope(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		tree:       []db.ListSessionTreeSinceRow{{ID: "root", Title: "root"}},
		allLatency: []db.ListAllLatencyEventsSinceRow{{Kind: "steering_fold", WaitedMs: 34}},
	}

	snap, err := gather.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeSession, SessionID: "root", WithLatency: true})
	require.NoError(t, err)
	require.Empty(t, snap.Latency)
}
