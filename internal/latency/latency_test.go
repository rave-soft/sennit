package latency_test

import (
	"context"
	"testing"
	"time"

	sennitdb "github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/stretchr/testify/require"
)

// newTestRecorder opens a fresh migrated database and returns a recorder
// writing to it plus the query set for reading back.
func newTestRecorder(t *testing.T) (latency.Recorder, *sennitdb.Queries) {
	t.Helper()
	dataDir := t.TempDir()
	// Release, not ResetPool: each test gets its own directory, so
	// dropping this one's refcount closes exactly this one's database.
	// ResetPool closes every pooled connection in the process, which
	// would yank the database out from under whichever sibling test is
	// running in parallel.
	t.Cleanup(func() { require.NoError(t, sennitdb.Release(dataDir)) })
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	return latency.NewService(q), q
}

func seedSession(t *testing.T, q *sennitdb.Queries, id, projectPath string) {
	t.Helper()
	_, err := q.CreateSession(t.Context(), sennitdb.CreateSessionParams{
		ID: id, Title: id, ProjectPath: projectPath,
	})
	require.NoError(t, err)
}

// A recorded wait must come back as the same number of milliseconds it
// went in as, under the kind it was written with — the whole table is
// worthless if the units drift.
func TestRecord_RoundTrips(t *testing.T) {
	t.Parallel()

	rec, q := newTestRecorder(t)
	seedSession(t, q, "s1", "/proj")

	rec.Record(t.Context(), latency.KindSteeringFold, "s1", 1500*time.Millisecond)
	rec.Record(t.Context(), latency.KindCompletionDelivery, "s1", 42*time.Millisecond)

	rows, err := q.ListLatencyEventsSince(t.Context(), sennitdb.ListLatencyEventsSinceParams{
		CreatedAt: 0, ProjectPath: "/proj",
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byKind := map[string]int64{}
	for _, r := range rows {
		byKind[r.Kind] = r.WaitedMs
	}
	require.Equal(t, int64(1500), byKind["steering_fold"])
	require.Equal(t, int64(42), byKind["completion_delivery"])
}

// The project-scoped query reads the path off the session, so an event
// recorded by one project's session must not show up in another's
// numbers.
func TestRecord_ScopedByItsSessionsProject(t *testing.T) {
	t.Parallel()

	rec, q := newTestRecorder(t)
	seedSession(t, q, "mine", "/proj")
	seedSession(t, q, "theirs", "/other")

	rec.Record(t.Context(), latency.KindSteeringFold, "mine", 10*time.Millisecond)
	rec.Record(t.Context(), latency.KindSteeringFold, "theirs", 20*time.Millisecond)

	scoped, err := q.ListLatencyEventsSince(t.Context(), sennitdb.ListLatencyEventsSinceParams{
		CreatedAt: 0, ProjectPath: "/proj",
	})
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	require.Equal(t, int64(10), scoped[0].WaitedMs)

	all, err := q.ListAllLatencyEventsSince(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, all, 2, "the global query still sees both")
}

// Measurement must never be the reason anything fails. An event for a
// session that no longer exists violates the foreign key; Record has to
// swallow that rather than surface it to a turn that is otherwise fine.
func TestRecord_UnknownSessionIsSwallowed(t *testing.T) {
	t.Parallel()

	rec, q := newTestRecorder(t)

	require.NotPanics(t, func() {
		rec.Record(t.Context(), latency.KindSteeringFold, "never-existed", time.Second)
	})

	rows, err := q.ListAllLatencyEventsSince(t.Context(), 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// A negative wait means the two clocks it was derived from disagree.
// Storing it as a zero would read as "instant", which is a measurement
// nobody made; dropping it is the honest option.
func TestRecord_DropsNonsenseInputs(t *testing.T) {
	t.Parallel()

	rec, q := newTestRecorder(t)
	seedSession(t, q, "s1", "/proj")

	rec.Record(t.Context(), latency.KindSteeringFold, "s1", -time.Second)
	rec.Record(t.Context(), latency.KindSteeringFold, "", time.Second)

	rows, err := q.ListAllLatencyEventsSince(t.Context(), 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// The callers record at the end of a step, holding the turn's context. A
// cancel landing in that window must not drop the measurement of the
// slow turn that is most worth having.
func TestRecord_SurvivesACanceledContext(t *testing.T) {
	t.Parallel()

	rec, q := newTestRecorder(t)
	seedSession(t, q, "s1", "/proj")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec.Record(ctx, latency.KindCompletionDelivery, "s1", 700*time.Millisecond)

	rows, err := q.ListAllLatencyEventsSince(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(700), rows[0].WaitedMs)
}

// Rows die with their session, which is what keeps this table out of
// `sennit gc`'s business.
func TestRecord_CascadesWithItsSession(t *testing.T) {
	t.Parallel()

	rec, q := newTestRecorder(t)
	seedSession(t, q, "s1", "/proj")
	rec.Record(t.Context(), latency.KindSteeringFold, "s1", time.Second)

	require.NoError(t, q.DeleteSession(t.Context(), "s1"))

	rows, err := q.ListAllLatencyEventsSince(t.Context(), 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}
