package db_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/stretchr/testify/require"
)

// The benchmarks in this file measure what [db.Connect]'s
// SetMaxOpenConns(1) actually costs now that one database is shared by
// every project a process serves, rather than being one database per
// project. The single connection is deliberate — letting the pool
// interleave writes and WAL checkpoints corrupted the file (see the
// comment on the call itself) — so the question these answer is not
// "should we raise it" but "at what concurrency does serializing behind
// it start to hurt".
//
// They are benchmarks, not tests, so they stay out of the normal suite.
// Run them with:
//
//	go test ./internal/db/ -run '^$' -bench . -benchtime 2000x
//
// A fixed iteration count rather than a duration keeps the numbers
// comparable between runs: these measure queueing, and letting Go pick a
// different iteration count per concurrency level would vary the amount
// of contention along with the thing being measured.

// benchDB opens a fresh migrated database, out of any other benchmark's
// way, and returns its query set.
func benchDB(b *testing.B) *db.Queries {
	b.Helper()
	dataDir := b.TempDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, db.Release(dataDir)) })
	return db.New(conn)
}

// writeOnce performs one session insert, the write every workspace does
// most often and the one the concurrent-projects scenario multiplies.
func writeOnce(ctx context.Context, q *db.Queries, id, projectPath string) error {
	_, err := q.CreateSession(ctx, db.CreateSessionParams{
		ID: id, Title: "bench", ProjectPath: projectPath,
	})
	return err
}

// BenchmarkSharedConnWrites measures per-write cost as more projects
// write at once through the one shared connection. Perfect
// serialization would show ns/op flat in the number of writers (each
// write still takes one write's worth of time, it just waits its turn);
// a rising curve is queueing overhead on top of that, which is the cost
// the shared database introduced.
func BenchmarkSharedConnWrites(b *testing.B) {
	for _, writers := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("projects=%d", writers), func(b *testing.B) {
			q := benchDB(b)
			ctx := context.Background()
			var seq atomic.Int64

			b.ResetTimer()
			var wg sync.WaitGroup
			perWriter := b.N / writers
			if perWriter == 0 {
				perWriter = 1
			}
			for w := range writers {
				wg.Add(1)
				go func(project int) {
					defer wg.Done()
					path := fmt.Sprintf("/project-%d", project)
					for range perWriter {
						id := fmt.Sprintf("s-%d", seq.Add(1))
						if err := writeOnce(ctx, q, id, path); err != nil {
							b.Error(err)
							return
						}
					}
				}(w)
			}
			wg.Wait()
		})
	}
}

// BenchmarkSharedConnWriteLatency reports the tail, not the average. A
// serialized queue's mean stays respectable while individual writes wait
// behind everyone else's, and a user notices the wait, not the mean.
// p50/p95/max are reported as custom metrics in milliseconds.
func BenchmarkSharedConnWriteLatency(b *testing.B) {
	for _, writers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("projects=%d", writers), func(b *testing.B) {
			q := benchDB(b)
			ctx := context.Background()
			var seq atomic.Int64

			perWriter := b.N / writers
			if perWriter == 0 {
				perWriter = 1
			}
			samples := make([][]time.Duration, writers)

			b.ResetTimer()
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func(project int) {
					defer wg.Done()
					path := fmt.Sprintf("/project-%d", project)
					mine := make([]time.Duration, 0, perWriter)
					for range perWriter {
						id := fmt.Sprintf("s-%d", seq.Add(1))
						start := time.Now()
						if err := writeOnce(ctx, q, id, path); err != nil {
							b.Error(err)
							return
						}
						mine = append(mine, time.Since(start))
					}
					samples[project] = mine
				}(w)
			}
			wg.Wait()
			b.StopTimer()

			reportLatency(b, samples)
		})
	}
}

// BenchmarkSharedConnReadUnderWriteLoad measures what a read costs while
// other projects are writing. This is the case the single connection is
// worst at: WAL would let a reader run alongside a writer, but one Go
// pool connection makes the reader wait for the writer to finish. It is
// also the case a user feels directly — a session list that stalls
// because some other workspace is busy.
func BenchmarkSharedConnReadUnderWriteLoad(b *testing.B) {
	for _, writers := range []int{0, 1, 4, 16} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			q := benchDB(b)
			ctx := context.Background()

			// Give the reader something to find, and give every writer
			// a project of its own so nothing contends on rows.
			require.NoError(b, writeOnce(ctx, q, "seed", "/reader"))

			stop := make(chan struct{})
			var writerWG sync.WaitGroup
			var seq atomic.Int64
			for w := range writers {
				writerWG.Add(1)
				go func(project int) {
					defer writerWG.Done()
					path := fmt.Sprintf("/writer-%d", project)
					for {
						select {
						case <-stop:
							return
						default:
						}
						id := fmt.Sprintf("w-%d", seq.Add(1))
						if err := writeOnce(ctx, q, id, path); err != nil {
							return
						}
					}
				}(w)
			}

			samples := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for range b.N {
				start := time.Now()
				if _, err := q.GetSessionByID(ctx, "seed"); err != nil {
					b.Fatal(err)
				}
				samples = append(samples, time.Since(start))
			}
			b.StopTimer()

			close(stop)
			writerWG.Wait()
			reportLatency(b, [][]time.Duration{samples})
		})
	}
}

// reportLatency flattens per-goroutine samples and reports the
// distribution in milliseconds. Nearest rank, matching how
// internal/stats reports the handoff latencies, so numbers from the two
// places mean the same thing.
func reportLatency(b *testing.B, samples [][]time.Duration) {
	b.Helper()
	var all []time.Duration
	for _, s := range samples {
		all = append(all, s...)
	}
	if len(all) == 0 {
		return
	}
	slices.Sort(all)
	at := func(p float64) float64 {
		rank := min(max(int(p*float64(len(all))), 1), len(all))
		return float64(all[rank-1].Microseconds()) / 1000
	}
	b.ReportMetric(at(0.50), "p50ms")
	b.ReportMetric(at(0.95), "p95ms")
	b.ReportMetric(at(1.00), "maxms")
}
