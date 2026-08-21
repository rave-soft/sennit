package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// concurrencyProbeModel records the maximum number of Stream iterators in
// flight at once. Each stream blocks on release, so a second dispatch that
// wrongly started a concurrent run for the same session is observable as an
// in-flight count above one.
type concurrencyProbeModel struct {
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	entered  chan struct{}
	release  chan struct{}
}

func (m *concurrencyProbeModel) Provider() string { return "fake" }
func (m *concurrencyProbeModel) Model() string    { return "fake-model" }

func (m *concurrencyProbeModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "done"}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *concurrencyProbeModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	return func(yield func(fantasy.StreamPart) bool) {
		cur := m.inFlight.Add(1)
		for {
			mx := m.maxSeen.Load()
			if cur <= mx || m.maxSeen.CompareAndSwap(mx, cur) {
				break
			}
		}
		// Signal that a stream is in flight (non-blocking), then hold here
		// so a racing second dispatch would be caught by maxSeen.
		select {
		case m.entered <- struct{}{}:
		default:
		}
		<-m.release
		m.inFlight.Add(-1)

		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "done"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *concurrencyProbeModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *concurrencyProbeModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestRun_ConcurrentInProcessDispatchStartsOneRun fires a burst of concurrent
// in-process Run calls (the path channel events use) at an idle session. Only
// one may become the active run; the rest must queue behind it. Before the
// dispatch decision was serialized under the per-session mutex, two callers
// could both pass the busy check and start two runs on the same session — this
// test catches that regression (maxSeen would exceed one).
//
// The probe answers the title call out of band (see isTitleCall) so
// GenerateTitle does not race into its inFlight/maxSeen counters. The
// queue count is not asserted because PrepareStep drains queued prompts into
// the active step before the model's Stream is called — by the time "entered"
// fires the queue is already empty by design.
func TestRun_ConcurrentInProcessDispatchStartsOneRun(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &concurrencyProbeModel{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_, _ = sa.Run(t.Context(), SessionAgentCall{
				SessionID: sess.ID,
				Prompt:    "event",
			})
		})
	}

	// Wait until the active run's Stream is in flight (blocked on release).
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		close(model.release)
		wg.Wait()
		t.Fatal("no run became active")
	}

	// Every other dispatch must have either queued (and been folded into the
	// active step by PrepareStep) or never started its own Stream. Either way,
	// at most one Stream may be in flight and the high-water mark must be one.
	require.Equal(t, int32(1), model.inFlight.Load(), "exactly one run may be active")
	require.Equal(t, int32(1), model.maxSeen.Load(), "no two runs may stream concurrently for one session")

	close(model.release)
	wg.Wait()
}

// TestDispatchDecision_AcceptIsAtomicUnderConcurrentRuns pins the
// invariant dispatchDecision's whole design rests on: the busy-check ->
// (enqueue | become-active) decision for one session must execute as a
// single, uninterrupted critical section under that session's mu. If a
// future edit caches a fact about session state (e.g. "is it idle") and
// then drops and reacquires the mutex before acting on it, two
// concurrent dispatches can each capture "idle" and each become the
// active run - this test's job is to make that observable as maxSeen > 1
// instead of a silent, occasional double-stream in production.
//
// The contention here is forced, not left to luck: this takes the
// session's own dispatch mutex directly (via the test-only
// sessionMuForTest, the same *sessionState.mu dispatchDecision uses)
// before starting any Run call, so both goroutines below are genuinely
// parked waiting on it - not staggered by goroutine-startup jitter the
// way an unsynchronized burst would be - by the time it is released.
//
// What this does NOT claim: it cannot guarantee catching a lock drop
// with literally zero work between the drop and the reacquire (e.g. a
// bare `mu.Unlock(); mu.Lock()` with nothing read or computed in
// between). Measured directly against exactly that shape of mutation -
// with real work restored around it so the drop is otherwise identical
// to a genuine stale-read bug - the reacquiring goroutine's own next
// Lock() call consistently wins the race back before the parked
// competitor is even rescheduled: dispatchDecision's critical section
// executes in low-microsecond time, well under typical goroutine wakeup
// latency, so a contended competitor essentially never wins a race whose
// entire window is that short, no matter how much concurrency or how
// many trials are thrown at it (verified up to 64-way concurrency and
// hundreds of trials under -race: zero hits). That is a property of how
// fast the function is, not a gap in this test's contention - and it
// means a lock drop with any realistic amount of interposed work (a
// function call, a channel op, anything slower than a couple of
// instructions) is exactly what this test is built to catch.
func TestDispatchDecision_AcceptIsAtomicUnderConcurrentRuns(t *testing.T) {
	t.Parallel()

	const trials = 10
	const concurrency = 2

	for trial := range trials {
		env := testEnv(t)
		model := &concurrencyProbeModel{
			entered: make(chan struct{}, concurrency),
			release: make(chan struct{}),
		}
		sa := testSessionAgent(env, model, "system").(*sessionAgent)

		sess, err := env.sessions.Create(t.Context(), "session")
		require.NoError(t, err)

		// Hold the session's own dispatch mutex before any Run call can,
		// so every concurrent dispatch below is forced to queue behind
		// it rather than race to acquire it first.
		gate := sa.sessionMuForTest(sess.ID)
		gate.Lock()

		var wg sync.WaitGroup
		for range concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "event"})
			}()
		}

		// Comfortably over the 1ms starvation threshold: by the time this
		// releases the gate, both goroutines above have been queued on
		// the mutex long enough that the runtime's own fairness guarantee
		// takes over from here, not luck.
		time.Sleep(20 * time.Millisecond)
		gate.Unlock()

		// Give both dispatches time to land (whichever branch they take)
		// before any stream is allowed to finish and the count is read.
		time.Sleep(50 * time.Millisecond)
		close(model.release)
		wg.Wait()

		require.Equal(t, int32(1), model.maxSeen.Load(),
			"trial %d: at most one run may become active for a session at a time - "+
				"the accept decision must be atomic under s.mu, not check-then-act", trial)
	}
}
