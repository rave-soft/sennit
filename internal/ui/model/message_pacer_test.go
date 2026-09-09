package model

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
)

// recorder collects what the pacer forwarded, in order.
type recorder struct {
	mu   sync.Mutex
	sent []any
}

func (r *recorder) send(msg any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, msg)
}

func (r *recorder) snapshot() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.sent...)
}

func update(id, sessionID string) pubsub.Event[message.Message] {
	return pubsub.Event[message.Message]{
		Type:    pubsub.UpdatedEvent,
		Payload: message.Message{ID: id, SessionID: sessionID},
	}
}

// waitFor polls until cond holds or the deadline passes, so a test never
// depends on a single sleep landing after the pacer's timer.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before the deadline")
}

// TestPaceMessages_BatchesConcurrentStreams is the reason the pacer
// exists: updates for many different messages must reach the program as
// ONE message, since bubbletea re-lays the UI out once per message it
// delivers. Collapsing duplicates alone would not help — a dozen
// delegations streaming produce a dozen distinct ids.
func TestPaceMessages_BatchesConcurrentStreams(t *testing.T) {
	t.Parallel()

	var rec recorder
	send, stop := PaceMessages(rec.send)
	defer stop()

	for i := range 12 {
		send(update(string(rune('a'+i)), "session-1"))
	}

	waitFor(t, func() bool { return len(rec.snapshot()) > 0 })

	sent := rec.snapshot()
	require.Len(t, sent, 1, "twelve streaming messages must cost one re-layout, not twelve")
	batch, ok := sent[0].(MessagesUpdatedMsg)
	require.True(t, ok, "expected a batch, got %T", sent[0])
	require.Len(t, batch.Events, 12, "every message's latest state must survive the batch")
}

// TestPaceMessages_CollapsesSupersededUpdates pins the other half: repeated
// updates to one message keep only the newest, in the slot the first one
// took, so a batch stays in arrival order.
func TestPaceMessages_CollapsesSupersededUpdates(t *testing.T) {
	t.Parallel()

	var rec recorder
	send, stop := PaceMessages(rec.send)
	defer stop()

	send(update("first", "s"))
	send(update("second", "s"))
	for range 20 {
		send(update("first", "s"))
	}

	waitFor(t, func() bool { return len(rec.snapshot()) > 0 })

	batch := rec.snapshot()[0].(MessagesUpdatedMsg)
	require.Len(t, batch.Events, 2)
	require.Equal(t, "first", batch.Events[0].Payload.ID, "a superseded update keeps its place in the batch")
	require.Equal(t, "second", batch.Events[1].Payload.ID)
}

// TestPaceMessages_FlushesBeforeNonUpdates guards ordering: a delete must
// not overtake a held update for the same message, which would remove it
// from the chat and then put it back.
func TestPaceMessages_FlushesBeforeNonUpdates(t *testing.T) {
	t.Parallel()

	var rec recorder
	send, stop := PaceMessages(rec.send)
	defer stop()

	send(update("doomed", "s"))
	deleted := pubsub.Event[message.Message]{
		Type:    pubsub.DeletedEvent,
		Payload: message.Message{ID: "doomed", SessionID: "s"},
	}
	send(deleted)

	sent := rec.snapshot()
	require.Len(t, sent, 2, "the delete must be forwarded straight away, behind the flush")
	batch, ok := sent[0].(MessagesUpdatedMsg)
	require.True(t, ok, "the held update must be released first, got %T", sent[0])
	require.Len(t, batch.Events, 1)
	require.Equal(t, deleted, sent[1])
}

// TestPaceMessages_PassesOtherMessagesThrough checks that the pacer is
// invisible to everything that is not a message update.
func TestPaceMessages_PassesOtherMessagesThrough(t *testing.T) {
	t.Parallel()

	var rec recorder
	send, stop := PaceMessages(rec.send)
	defer stop()

	type unrelated struct{ n int }
	send(unrelated{1})
	send(unrelated{2})

	require.Equal(t, []any{unrelated{1}, unrelated{2}}, rec.snapshot())
}

// TestPaceMessages_StopDropsPending makes sure a batch cannot land on a
// program that has already finished running.
func TestPaceMessages_StopDropsPending(t *testing.T) {
	t.Parallel()

	var rec recorder
	send, stop := PaceMessages(rec.send)

	send(update("held", "s"))
	stop()
	send(update("after", "s"))

	time.Sleep(4 * pacerFrameInterval)
	require.Empty(t, rec.snapshot(), "nothing may be delivered once the pacer is stopped")
}

func sessionUpdate(id string) pubsub.Event[session.Session] {
	return pubsub.Event[session.Session]{
		Type:    pubsub.UpdatedEvent,
		Payload: session.Session{ID: id},
	}
}

// TestPaceMessages_PacesSessionUpdates is the other half of the reason the
// pacer exists. The session store publishes on every write with no debounce
// of its own, so a streaming turn emits session updates at least as fast as
// message updates. While they went through the non-update path, each one
// released the message batch early and then laid the UI out again for
// itself — two re-layouts per update on a pacer meant to allow one per
// frame. Held and collapsed, a frame's worth of them costs one.
func TestPaceMessages_PacesSessionUpdates(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	send, stop := PaceMessages(rec.send)
	defer stop()

	for range 20 {
		send(sessionUpdate("s1"))
	}
	send(sessionUpdate("s2"))

	waitFor(t, func() bool { return len(rec.snapshot()) == 2 })
	sent := rec.snapshot()
	require.Len(t, sent, 2, "one event per distinct session, not per update")
	require.Equal(t, "s1", sent[0].(pubsub.Event[session.Session]).Payload.ID)
	require.Equal(t, "s2", sent[1].(pubsub.Event[session.Session]).Payload.ID)
}

// TestPaceMessages_SessionUpdateDoesNotFlushMessages guards the regression
// directly: a session update must no longer cut a message batch short, or
// pacing collapses back to one re-layout per streamed chunk.
func TestPaceMessages_SessionUpdateDoesNotFlushMessages(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	send, stop := PaceMessages(rec.send)
	defer stop()

	send(update("m1", "s1"))
	send(sessionUpdate("s1"))
	send(update("m2", "s1"))

	waitFor(t, func() bool { return len(rec.snapshot()) == 2 })
	sent := rec.snapshot()
	batch, ok := sent[0].(MessagesUpdatedMsg)
	require.True(t, ok, "the message batch leads the frame")
	require.Len(t, batch.Events, 2, "both updates stayed in one batch")
	require.IsType(t, pubsub.Event[session.Session]{}, sent[1], "session update follows the batch")
}

// TestPaceMessages_OneDeliveryAtATime is the pacer's backpressure. The
// program's Send blocks once its message queue is full, and what the pacer
// does while blocked decides whether a busy workspace degrades or collapses:
// load must become a fuller batch, not another goroutine parked behind the
// same wall. A wedged UI was once found holding 922 of those.
//
// The load has to keep arriving for the whole time the program is wedged,
// which is what a streaming turn does. A single burst proves nothing: the
// pacer arms its frame timer per arrival, so one burst can only ever cost
// one extra goroutine, whether or not the guard being tested is there.
func TestPaceMessages_OneDeliveryAtATime(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var inFlight, maxInFlight, calls atomic.Int64
	rec := &recorder{}

	send := func(msg any) {
		n := inFlight.Add(1)
		for {
			was := maxInFlight.Load()
			if n <= was || maxInFlight.CompareAndSwap(was, n) {
				break
			}
		}
		calls.Add(1)
		<-release // stand in for a program whose queue is full
		rec.send(msg)
		inFlight.Add(-1)
	}

	paced, stop := PaceMessages(send)
	defer stop()

	// Get one delivery in flight and wedged.
	paced(update("m0", "s1"))
	waitFor(t, func() bool { return calls.Load() == 1 })

	// Keep feeding it for many frame intervals while the program is not
	// accepting. Every one of these is a chance to arm another timer.
	deadline := time.Now().Add(20 * pacerFrameInterval)
	for i := 0; time.Now().Before(deadline); i++ {
		paced(update("m"+strconv.Itoa(i%5), "s1"))
		time.Sleep(pacerFrameInterval / 8)
	}

	require.Equal(t, int64(1), calls.Load(),
		"no delivery may start while another is blocked in the program's Send")

	close(release)
	waitFor(t, func() bool { return inFlight.Load() == 0 && len(rec.snapshot()) >= 2 })

	require.Equal(t, int64(1), maxInFlight.Load(), "only ever one goroutine in the program's Send")
	require.Less(t, calls.Load(), int64(10),
		"a wedged program must collapse the backlog into a couple of batches, not queue up")

	// The batch delivered after the block must carry every distinct message
	// that arrived during it, collapsed to one event each.
	sent := rec.snapshot()
	last := sent[len(sent)-1].(MessagesUpdatedMsg)
	ids := map[string]int{}
	for _, e := range last.Events {
		ids[e.Payload.ID]++
	}
	require.Len(t, ids, 5, "one slot per distinct message id")
	for id, n := range ids {
		require.Equal(t, 1, n, "message %s appears once, superseded updates dropped", id)
	}
}
