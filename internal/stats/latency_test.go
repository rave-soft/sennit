package stats_test

import (
	"testing"

	"github.com/rave-soft/sennit/internal/stats"
	"github.com/stretchr/testify/require"
)

// The whole point of reporting percentiles instead of a mean is that a
// handoff which is normally instant and occasionally stalls must not
// average out to "fine". Ninety-nine 10ms waits and one 5s wait have a
// mean near 60ms — a number that describes neither the normal case nor
// the problem.
func TestComputeLatency_TailDoesNotHideInTheMiddle(t *testing.T) {
	t.Parallel()

	events := make([]stats.LatencyEvent, 0, 100)
	for range 99 {
		events = append(events, stats.LatencyEvent{Kind: "steering_fold", WaitedMS: 10})
	}
	events = append(events, stats.LatencyEvent{Kind: "steering_fold", WaitedMS: 5000})

	rows := stats.ComputeLatency(events)
	require.Len(t, rows, 1)
	require.Equal(t, int64(100), rows[0].Events)
	require.Equal(t, int64(10), rows[0].P50MS, "the normal case stays visible")
	require.Equal(t, int64(5000), rows[0].MaxMS, "the stall stays visible")
}

// Nearest rank, not interpolation: every number in this table must be a
// wait that actually happened, so a reader chasing a P95 can go find the
// turn that produced it.
func TestComputeLatency_PercentilesAreObservedValues(t *testing.T) {
	t.Parallel()

	var events []stats.LatencyEvent
	for _, ms := range []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10} {
		events = append(events, stats.LatencyEvent{Kind: "steering_fold", WaitedMS: ms})
	}

	rows := stats.ComputeLatency(events)
	require.Len(t, rows, 1)
	// Rank ceil(0.50*10) = 5 and ceil(0.95*10) = 10, both real samples.
	require.Equal(t, int64(5), rows[0].P50MS)
	require.Equal(t, int64(10), rows[0].P95MS)
}

// A single sample is the degenerate case the rank arithmetic has to
// survive: every percentile is that one value, and nothing indexes off
// the end of the slice.
func TestComputeLatency_SingleSample(t *testing.T) {
	t.Parallel()

	rows := stats.ComputeLatency([]stats.LatencyEvent{{Kind: "completion_delivery", WaitedMS: 42}})
	require.Len(t, rows, 1)
	require.Equal(t, stats.Latency{Kind: "completion_delivery", Events: 1, P50MS: 42, P95MS: 42, MaxMS: 42}, rows[0])
}

// Kinds are separate distributions and are never pooled: steering and
// delegation delivery wait on different things, and one being slow says
// nothing about the other.
func TestComputeLatency_KindsStaySeparateAndOrdered(t *testing.T) {
	t.Parallel()

	rows := stats.ComputeLatency([]stats.LatencyEvent{
		{Kind: "steering_fold", WaitedMS: 100},
		{Kind: "completion_delivery", WaitedMS: 1},
		{Kind: "steering_fold", WaitedMS: 200},
	})

	require.Len(t, rows, 2)
	require.Equal(t, "completion_delivery", rows[0].Kind, "rows are ordered by kind so two runs compare line for line")
	require.Equal(t, int64(1), rows[0].Events)
	require.Equal(t, "steering_fold", rows[1].Kind)
	require.Equal(t, int64(2), rows[1].Events)
	require.Equal(t, int64(200), rows[1].MaxMS)
}

func TestComputeLatency_NoEvents(t *testing.T) {
	t.Parallel()

	require.Empty(t, stats.ComputeLatency(nil))
}
