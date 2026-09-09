package model

import (
	"sync"
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
