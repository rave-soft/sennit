package message

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// slowUpdateQuerier wraps a [db.Querier] and forces UpdateMessage to
// hang on a release channel. Used to simulate an in-flight SQL write.
type slowUpdateQuerier struct {
	db.Querier
	release   chan struct{}
	started   chan struct{}
	startOnce sync.Once
}

type failingUpdateQuerier struct {
	db.Querier
	fail atomic.Bool
}

func (q *failingUpdateQuerier) UpdateMessage(ctx context.Context, arg db.UpdateMessageParams) error {
	if q.fail.Load() {
		return errors.New("update message failed")
	}
	return q.Querier.UpdateMessage(ctx, arg)
}

func (s *slowUpdateQuerier) UpdateMessage(ctx context.Context, arg db.UpdateMessageParams) error {
	s.startOnce.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Querier.UpdateMessage(ctx, arg)
}

// newTestService spins up a fresh in-memory message.Service backed by a
// temporary on-disk SQLite database. Returns the service plus a session
// ID to attach messages to.
func newTestService(t *testing.T, opts ...ServiceOption) (Service, string) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	svc := NewService(q, opts...)
	return svc, sess.ID
}

func TestListAllUserMessagesExcludesMachineGeneratedPrompts(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	root, err := sessions.Create(t.Context(), "root")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), "child", root.ID, "child")
	require.NoError(t, err)
	threadSession, err := sessions.Create(t.Context(), "thread")
	require.NoError(t, err)

	_, err = q.CreateThread(t.Context(), db.CreateThreadParams{
		ID:           "thread-1",
		Name:         "thread-1",
		ProjectPath:  "/test/project",
		Goal:         "delegated goal",
		BaseBranch:   "main",
		Branch:       "thread/thread-1",
		WorktreePath: "/tmp/thread-1",
		SessionID:    threadSession.ID,
		Status:       "running",
		MergePolicy:  "manual",
	})
	require.NoError(t, err)

	svc := NewService(q)
	for _, tc := range []struct {
		sessionID string
		text      string
	}{
		{sessionID: root.ID, text: "human prompt"},
		{sessionID: child.ID, text: "sub-agent prompt"},
		{sessionID: threadSession.ID, text: "thread prompt"},
	} {
		_, err = svc.Create(t.Context(), tc.sessionID, CreateMessageParams{
			Role:  User,
			Parts: []ContentPart{TextContent{Text: tc.text}},
		})
		require.NoError(t, err)
	}

	messages, err := svc.ListAllUserMessages(t.Context())
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, root.ID, messages[0].SessionID)
	require.Equal(t, "human prompt", messages[0].Content().String())
}

func TestCreate_OriginDefaultsToPersonAndRoundTrips(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t)

	// Zero-value Origin (the common case: nothing explicitly set).
	implicit, err := svc.Create(t.Context(), sessionID, CreateMessageParams{
		Role:  User,
		Parts: []ContentPart{TextContent{Text: "typed by hand"}},
	})
	require.NoError(t, err)
	require.Equal(t, OriginPerson, implicit.Origin)

	// Explicit OriginAgent.
	delegated, err := svc.Create(t.Context(), sessionID, CreateMessageParams{
		Role:   User,
		Parts:  []ContentPart{TextContent{Text: "dispatched by an agent"}},
		Origin: OriginAgent,
	})
	require.NoError(t, err)
	require.Equal(t, OriginAgent, delegated.Origin)

	// Round-trip through Get.
	got, err := svc.Get(t.Context(), implicit.ID)
	require.NoError(t, err)
	require.Equal(t, OriginPerson, got.Origin)

	got, err = svc.Get(t.Context(), delegated.ID)
	require.NoError(t, err)
	require.Equal(t, OriginAgent, got.Origin)

	// Round-trip through List.
	all, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)
	origins := make(map[string]Origin, len(all))
	for _, m := range all {
		origins[m.ID] = m.Origin
	}
	require.Equal(t, OriginPerson, origins[implicit.ID])
	require.Equal(t, OriginAgent, origins[delegated.ID])
}

// eventCollector consumes broker events into a slice in a goroutine
// and exposes thread-safe Snapshot / Reset helpers for assertions.
type eventCollector struct {
	mu     sync.Mutex
	events []pubsub.Event[Message]
}

func collect(ctx context.Context, sub <-chan pubsub.Event[Message]) *eventCollector {
	c := &eventCollector{}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				c.mu.Lock()
				c.events = append(c.events, ev)
				c.mu.Unlock()
			}
		}
	}()
	return c
}

func (c *eventCollector) snapshot() []pubsub.Event[Message] {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]pubsub.Event[Message], len(c.events))
	copy(out, c.events)
	return out
}

func (c *eventCollector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = nil
}

func TestUpdate_DebouncesTextDeltas(t *testing.T) {
	t.Parallel()

	// Long-enough debounce that we can verify nothing flushes prematurely.
	svc, sessionID := newTestService(t, WithDebounce(50*time.Millisecond))

	subCtx, cancelSub := context.WithCancel(t.Context())
	defer cancelSub()
	sub := svc.Subscribe(subCtx)
	collector := collect(subCtx, sub)

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{
		Role: Assistant,
	})
	require.NoError(t, err)
	// Drop the CreatedEvent emitted by Create.
	time.Sleep(5 * time.Millisecond)
	collector.reset()

	// Push 5 deltas inside a single debounce window.
	for i := 0; i < 5; i++ {
		msg.AppendContent("a")
		require.NoError(t, svc.Update(t.Context(), msg))
	}

	// Before the debounce expires no UpdatedEvent should have landed.
	time.Sleep(10 * time.Millisecond)
	require.Empty(t, collector.snapshot(), "no events should land before debounce window expires")

	// Wait for the debounce timer to fire.
	require.Eventually(t, func() bool {
		return len(collector.snapshot()) >= 1
	}, time.Second, 5*time.Millisecond)
	events := collector.snapshot()
	require.Len(t, events, 1, "5 deltas should coalesce into 1 UpdatedEvent")
	require.Equal(t, pubsub.UpdatedEvent, events[0].Type)
	require.Equal(t, "aaaaa", events[0].Payload.Content().Text)

	// Final state must be persisted.
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "aaaaa", got.Content().Text)
}

func TestUpdate_TerminalUpdatesFlushSynchronously(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	subCtx, cancelSub := context.WithCancel(t.Context())
	defer cancelSub()
	sub := svc.Subscribe(subCtx)
	collector := collect(subCtx, sub)

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	collector.reset()

	// AddFinish makes the message terminal; Update must flush
	// synchronously even with a 1-hour debounce.
	msg.AppendContent("done")
	msg.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(t.Context(), msg))

	require.Eventually(t, func() bool {
		return len(collector.snapshot()) >= 1
	}, time.Second, 5*time.Millisecond,
		"terminal update must publish without waiting for debounce")
	events := collector.snapshot()
	require.Len(t, events, 1)
	require.True(t, events[0].Payload.IsFinished())

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.True(t, got.IsFinished())
}

func TestUpdate_FinishedFlushEvictsPendingState(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))
	impl := svc.(*service)
	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("done")
	msg.AddFinish(FinishReasonEndTurn, "", "")

	require.NoError(t, svc.Update(t.Context(), msg))

	impl.mu.Lock()
	_, ok := impl.pending[msg.ID]
	impl.mu.Unlock()
	require.False(t, ok, "successful final flush must release its snapshots")
	require.NoError(t, svc.Flush(t.Context(), msg.ID), "evicted entries are cheap no-ops")
}

func TestUpdate_StructuralFlushRetainsPendingUntilFinished(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))
	impl := svc.(*service)
	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AddToolCall(ToolCall{ID: "tc1", Name: "view"})

	require.NoError(t, svc.Update(t.Context(), msg))
	impl.mu.Lock()
	_, ok := impl.pending[msg.ID]
	impl.mu.Unlock()
	require.True(t, ok, "tool-call structural flush is not a final message flush")

	msg.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(t.Context(), msg))
	impl.mu.Lock()
	_, ok = impl.pending[msg.ID]
	impl.mu.Unlock()
	require.False(t, ok)
}

func TestFlush_WriteErrorRetainsPendingStateForRetry(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	failing := &failingUpdateQuerier{Querier: q}
	failing.fail.Store(true)
	svc := NewService(failing, WithDebounce(time.Hour))
	impl := svc.(*service)

	msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AddFinish(FinishReasonEndTurn, "", "")
	require.Error(t, svc.Update(t.Context(), msg))
	impl.mu.Lock()
	p := impl.pending[msg.ID]
	dirty := p != nil && p.dirty
	impl.mu.Unlock()
	require.NotNil(t, p)
	require.True(t, dirty)

	failing.fail.Store(false)
	require.NoError(t, svc.Flush(t.Context(), msg.ID))
	impl.mu.Lock()
	_, ok := impl.pending[msg.ID]
	impl.mu.Unlock()
	require.False(t, ok)
}

func TestUpdate_ConcurrentFinalDeltaIsNotLostDuringFinalWrite(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	slow := &slowUpdateQuerier{
		Querier: q,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	svc := NewService(slow, WithDebounce(time.Hour))
	impl := svc.(*service)

	msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("first")
	msg.AddFinish(FinishReasonEndTurn, "", "")
	firstDone := make(chan error, 1)
	go func() { firstDone <- svc.Update(t.Context(), msg) }()
	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("final write never reached UpdateMessage")
	}

	msg.AppendContent(" second")
	secondDone := make(chan error, 1)
	go func() { secondDone <- svc.Update(t.Context(), msg) }()
	require.Eventually(t, func() bool {
		impl.mu.Lock()
		defer impl.mu.Unlock()
		p := impl.pending[msg.ID]
		return p != nil && p.dirty
	}, time.Second, time.Millisecond)

	close(slow.release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "first second", got.Content().Text)
	impl.mu.Lock()
	_, ok := impl.pending[msg.ID]
	impl.mu.Unlock()
	require.False(t, ok, "latest final delta should flush before eviction")
}

func TestUpdate_ToolCallStructuralChangeFlushes(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	// Adding a new tool call is a structural change → sync flush.
	msg.AddToolCall(ToolCall{ID: "tc1", Name: "view", Finished: false})
	require.NoError(t, svc.Update(t.Context(), msg))

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Len(t, got.ToolCalls(), 1)
	require.Equal(t, "tc1", got.ToolCalls()[0].ID)

	// Marking the tool call finished is also a structural change.
	msg.AddToolCall(ToolCall{ID: "tc1", Name: "view", Input: "{}", Finished: true})
	require.NoError(t, svc.Update(t.Context(), msg))

	got, err = svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.True(t, got.ToolCalls()[0].Finished)
}

func TestUpdate_ReasoningEndFlushes(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	// Reasoning deltas alone debounce.
	msg.AppendReasoningContent("hmm")
	require.NoError(t, svc.Update(t.Context(), msg))
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Empty(t, got.ReasoningContent().Thinking, "reasoning delta should still be in the debounce buffer")

	// FinishThinking sets FinishedAt → terminal flush.
	msg.FinishThinking()
	require.NoError(t, svc.Update(t.Context(), msg))

	got, err = svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "hmm", got.ReasoningContent().Thinking)
	require.NotZero(t, got.ReasoningContent().FinishedAt)
}

func TestFlush_DrainsPendingDebouncedUpdates(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("buffered")
	require.NoError(t, svc.Update(t.Context(), msg))

	// Without a flush the SQL row is unchanged from Create.
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Empty(t, got.Content().Text)

	require.NoError(t, svc.Flush(t.Context(), msg.ID))

	got, err = svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "buffered", got.Content().Text)

	// Subsequent Flush is a no-op.
	require.NoError(t, svc.Flush(t.Context(), msg.ID))
}

func TestFlushAll_DrainsAllPending(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	const n = 5
	msgs := make([]Message, n)
	for i := range msgs {
		m, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
		require.NoError(t, err)
		m.AppendContent("hi")
		require.NoError(t, svc.Update(t.Context(), m))
		msgs[i] = m
	}

	require.NoError(t, svc.FlushAll(t.Context()))

	for _, m := range msgs {
		got, err := svc.Get(t.Context(), m.ID)
		require.NoError(t, err)
		require.Equal(t, "hi", got.Content().Text, "FlushAll should drain every pending message")
	}
}

func TestUpdate_OrderingMatchesNonCoalesced(t *testing.T) {
	t.Parallel()

	// Compare the final state after coalesced vs zero-debounce updates.
	// A sequence of interleaved text/reasoning/tool-call updates must
	// converge to the same final DB row either way.
	build := func(svc Service, sessionID string) Message {
		msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
		require.NoError(t, err)
		msg.AppendReasoningContent("thinking 1 ")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AppendReasoningContent("thinking 2")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.FinishThinking()
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AppendContent("hello ")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AppendContent("world")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AddToolCall(ToolCall{ID: "tc", Name: "x", Finished: false})
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AddToolCall(ToolCall{ID: "tc", Name: "x", Input: "{}", Finished: true})
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AddFinish(FinishReasonEndTurn, "", "")
		require.NoError(t, svc.Update(t.Context(), msg))
		return msg
	}

	coalesced, sid1 := newTestService(t, WithDebounce(20*time.Millisecond))
	a := build(coalesced, sid1)
	require.NoError(t, coalesced.FlushAll(t.Context()))
	gotA, err := coalesced.Get(t.Context(), a.ID)
	require.NoError(t, err)

	immediate, sid2 := newTestService(t, WithDebounce(0))
	b := build(immediate, sid2)
	gotB, err := immediate.Get(t.Context(), b.ID)
	require.NoError(t, err)

	require.Equal(t, gotA.Content().Text, gotB.Content().Text)
	require.Equal(t, gotA.ReasoningContent().Thinking, gotB.ReasoningContent().Thinking)
	require.Equal(t, len(gotA.ToolCalls()), len(gotB.ToolCalls()))
	require.Equal(t, gotA.IsFinished(), gotB.IsFinished())
}

func TestDelete_DropsPendingState(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))
	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("dropped")
	require.NoError(t, svc.Update(t.Context(), msg))

	require.NoError(t, svc.Delete(t.Context(), msg.ID))

	// FlushAll after Delete must not write to the deleted row.
	require.NoError(t, svc.FlushAll(t.Context()))

	_, err = svc.Get(t.Context(), msg.ID)
	require.Error(t, err, "deleted message must remain deleted")
}

func TestBroker_PublishLossyDropCounter(t *testing.T) {
	t.Parallel()

	// Tiny channel buffer so we can saturate from a single sender.
	b := pubsub.NewBrokerWithOptions[int](1)
	defer b.Shutdown()

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := b.Subscribe(subCtx)
	require.NotNil(t, sub)

	// Don't read from sub. Saturate the buffer.
	for range 100 {
		b.Publish(pubsub.UpdatedEvent, 1)
	}

	require.GreaterOrEqual(t, b.DropCount(), uint64(1),
		"lossy Publish must increment the drop counter under contention")
}

func TestBroker_PublishMustDeliverHonorsTimeout(t *testing.T) {
	t.Parallel()

	b := pubsub.NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(20 * time.Millisecond)
	defer b.Shutdown()

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := b.Subscribe(subCtx)
	require.NotNil(t, sub)

	// Saturate: one event sits in the buffer, the second must wait.
	b.Publish(pubsub.UpdatedEvent, 1)

	// PublishMustDeliver should block up to 20ms then drop.
	start := time.Now()
	b.PublishMustDeliver(t.Context(), pubsub.UpdatedEvent, 2)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, 20*time.Millisecond,
		"PublishMustDeliver should block at least the timeout under contention")
	require.Less(t, elapsed, 200*time.Millisecond,
		"PublishMustDeliver must not block indefinitely")
	require.GreaterOrEqual(t, b.MustDeliverDropCount(), uint64(1),
		"timeout must increment the must-deliver drop counter")
}

func TestBroker_PublishMustDeliverWithReader(t *testing.T) {
	t.Parallel()

	b := pubsub.NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(50 * time.Millisecond)
	defer b.Shutdown()

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := b.Subscribe(subCtx)

	var received atomic.Uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-subCtx.Done():
				return
			case _, ok := <-sub:
				if !ok {
					return
				}
				received.Add(1)
			}
		}
	}()

	for i := range 10 {
		b.PublishMustDeliver(t.Context(), pubsub.UpdatedEvent, i)
	}

	// All 10 should land within the must-deliver timeout window.
	require.Eventually(t, func() bool { return received.Load() == 10 },
		time.Second, 5*time.Millisecond,
		"all must-deliver events should reach an active subscriber")
	require.Zero(t, b.MustDeliverDropCount(),
		"no drops expected when subscriber drains promptly")
}

func TestUpdate_TerminalEventUsesMustDeliver(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := svc.Subscribe(subCtx)

	var seenFinish atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-subCtx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				if ev.Type == pubsub.UpdatedEvent && ev.Payload.IsFinished() {
					seenFinish.Store(true)
				}
			}
		}
	}()

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("final")
	msg.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(t.Context(), msg))

	require.Eventually(t, func() bool { return seenFinish.Load() },
		time.Second, 10*time.Millisecond,
		"terminal update must reach subscribers via the must-deliver path")
}

func TestUpdate_ZeroDebounceFlushesEveryUpdate(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(0))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		msg.AppendContent("x")
		require.NoError(t, svc.Update(t.Context(), msg))
		got, err := svc.Get(t.Context(), msg.ID)
		require.NoError(t, err)
		require.Len(t, got.Content().Text, i+1, "every update must land synchronously when debounce is 0")
	}
}

// TestFlush_WaitsForInFlightWrite reproduces the failure where Flush
// or FlushAll could return before a concurrent in-flight SQL write
// completed. We block UpdateMessage on a release channel, fire the
// debounce timer, then call Flush and assert it does not return until
// the in-flight write actually lands.
func TestFlush_WaitsForInFlightWrite(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	slow := &slowUpdateQuerier{
		Querier: q,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	// Short debounce so the timer fires quickly.
	svc := NewService(slow, WithDebounce(10*time.Millisecond))

	msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("payload")
	require.NoError(t, svc.Update(t.Context(), msg))

	// Wait for the timer-fired flush to enter UpdateMessage.
	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("timer-fired flush never reached UpdateMessage")
	}

	// At this point the buffer is dirty=false but flushing=true. A
	// naive Flush would early-return on !dirty. Spawn Flush in a
	// goroutine and assert it has not returned while the write is
	// still blocked.
	flushDone := make(chan error, 1)
	go func() { flushDone <- svc.Flush(t.Context(), msg.ID) }()

	select {
	case err := <-flushDone:
		t.Fatalf("Flush returned %v while in-flight write was still blocked", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: Flush is correctly waiting.
	}

	// Release the slow write; Flush must now return cleanly.
	close(slow.release)
	select {
	case err := <-flushDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Flush did not return after in-flight write completed")
	}

	// The SQL row should now reflect the buffered payload.
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "payload", got.Content().Text)
}

// TestFlushAll_WaitsForInFlightWrite asserts FlushAll picks up IDs
// whose buffer is currently flushing (dirty=false) so shutdown and
// session-switch callers don't return while an SQL write is mid-flight.
func TestFlushAll_WaitsForInFlightWrite(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	slow := &slowUpdateQuerier{
		Querier: q,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	svc := NewService(slow, WithDebounce(10*time.Millisecond))

	msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("payload")
	require.NoError(t, svc.Update(t.Context(), msg))

	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("timer-fired flush never reached UpdateMessage")
	}

	flushDone := make(chan error, 1)
	go func() { flushDone <- svc.FlushAll(t.Context()) }()

	select {
	case err := <-flushDone:
		t.Fatalf("FlushAll returned %v while in-flight write was still blocked", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(slow.release)
	select {
	case err := <-flushDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("FlushAll did not return after in-flight write completed")
	}

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "payload", got.Content().Text)
}

// TestUpdate_StructuralFlushUsesMustDeliver covers the second review
// finding: structural terminal events (tool-call add, tool-call
// finish, reasoning end) must publish via the must-deliver path even
// when the message itself is not yet IsFinished.
//
// We detect which path was taken by saturating a subscriber's channel
// buffer with no reader. With a short must-deliver timeout, the
// must-deliver path increments [pubsub.Broker.MustDeliverDropCount]
// after the timeout expires; the lossy path increments
// [pubsub.Broker.DropCount] immediately. The two counters are
// disjoint, so they precisely identify which call site published the
// event.
func TestUpdate_StructuralFlushUsesMustDeliver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*Message)
	}{
		{
			name: "tool call add",
			mut: func(m *Message) {
				m.AddToolCall(ToolCall{ID: "tc1", Name: "view"})
			},
		},
		{
			name: "tool call finish",
			mut: func(m *Message) {
				m.AddToolCall(ToolCall{ID: "tc1", Name: "view", Input: "{}", Finished: true})
			},
		},
		{
			name: "reasoning end",
			mut: func(m *Message) {
				m.AppendReasoningContent("hmm")
				m.FinishThinking()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conn, err := db.Connect(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			q := db.New(conn)
			sessions := session.NewService(q, conn, "/test/project")
			sess, err := sessions.Create(t.Context(), "test")
			require.NoError(t, err)

			// Replace the default broker with a tiny buffer + short
			// must-deliver timeout so we can fully saturate from a
			// single sender and observe drops without long waits.
			svc := NewService(q, WithDebounce(time.Hour))
			impl := svc.(*service)
			impl.Shutdown()
			impl.Broker = pubsub.NewBrokerWithOptions[Message](1)
			impl.SetMustDeliverTimeout(40 * time.Millisecond)

			subCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sub := svc.Subscribe(subCtx)

			msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
			require.NoError(t, err)

			// Saturate the subscriber's buffer (capacity 1). The
			// CreatedEvent from Create above already left one event
			// in the buffer; we never read sub, so the next publish
			// has nowhere to go.
			_ = sub // intentionally not drained.

			// Drive the structural change. With debounce=1h, Update
			// flushes synchronously and routes through whichever
			// publish path the service chose for structural events.
			tc.mut(&msg)
			require.NoError(t, svc.Update(t.Context(), msg))

			// Must-deliver timeout (40ms) should have expired with
			// no drain. If structural events are routed through
			// must-deliver: MustDeliverDropCount > 0, DropCount
			// unchanged. If routed through lossy Publish:
			// DropCount > 0, MustDeliverDropCount == 0.
			require.Eventually(t, func() bool {
				return impl.MustDeliverDropCount() >= 1
			}, time.Second, 5*time.Millisecond,
				"structural terminal event should publish via must-deliver, not lossy Publish")
			require.Zero(t, impl.DropCount(),
				"structural terminal event must not be silently dropped via lossy Publish")
		})
	}
}

// TestUnmarshalParts_UnknownTypeSkipped verifies that an unrecognized
// part type in the middle of the array is dropped rather than failing
// the whole message, so a session written by a newer binary stays
// readable on this one.
func TestUnmarshalParts_UnknownTypeSkipped(t *testing.T) {
	t.Parallel()

	raw := `[
		{"type":"text","data":{"text":"before"}},
		{"type":"some_future_type","data":{"whatever":true}},
		{"type":"text","data":{"text":"after"}}
	]`

	parts, err := UnmarshalParts([]byte(raw), "msg-1")
	require.NoError(t, err)
	require.Equal(t, []ContentPart{
		TextContent{Text: "before"},
		TextContent{Text: "after"},
	}, parts)
}

// TestUnmarshalParts_Empty verifies an empty array unmarshals cleanly.
func TestUnmarshalParts_Empty(t *testing.T) {
	t.Parallel()

	parts, err := UnmarshalParts([]byte(`[]`), "msg-1")
	require.NoError(t, err)
	require.Empty(t, parts)
}

// TestUnmarshalParts_OldFormatNoMeta verifies a blob written before the
// "_meta" version marker existed (a plain array of part wrappers) is
// still read correctly with zero data migration.
func TestUnmarshalParts_OldFormatNoMeta(t *testing.T) {
	t.Parallel()

	raw := `[{"type":"text","data":{"text":"hello"}}]`

	parts, err := UnmarshalParts([]byte(raw), "msg-1")
	require.NoError(t, err)
	require.Equal(t, []ContentPart{TextContent{Text: "hello"}}, parts)
}

// TestMarshalUnmarshalParts_RoundTrip verifies a blob produced by the
// new marshalParts round-trips through unmarshalParts, and that the
// synthetic "_meta" element never surfaces as a ContentPart.
func TestMarshalUnmarshalParts_RoundTrip(t *testing.T) {
	t.Parallel()

	original := []ContentPart{
		TextContent{Text: "hi"},
		ToolCall{ID: "call1", Name: "bash", Input: "{}"},
		Finish{Reason: FinishReasonEndTurn, Time: 42},
	}

	data, err := MarshalParts(original)
	require.NoError(t, err)

	// The wrapper array must carry the "_meta" marker as element 0.
	var raw []json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Len(t, raw, len(original)+1)
	var firstWrapper struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(raw[0], &firstWrapper))
	require.Equal(t, "_meta", firstWrapper.Type)

	parts, err := UnmarshalParts(data, "msg-1")
	require.NoError(t, err)
	require.Equal(t, original, parts)
}

// TestUnmarshalParts_MalformedNotSwallowed verifies that real
// corruption (broken JSON, or a known type whose payload doesn't
// match its struct) still returns an error and is not accidentally
// treated as an unrecognized-type skip.
func TestUnmarshalParts_MalformedNotSwallowed(t *testing.T) {
	t.Parallel()

	t.Run("broken JSON syntax", func(t *testing.T) {
		t.Parallel()
		_, err := UnmarshalParts([]byte(`[{"type":"text","data":`), "msg-1")
		require.Error(t, err)
	})

	t.Run("known type with mismatched payload", func(t *testing.T) {
		t.Parallel()
		// finish's Time field is int64; a string payload must fail
		// to unmarshal rather than being silently dropped.
		raw := `[{"type":"finish","data":{"time":"not-a-number"}}]`
		_, err := UnmarshalParts([]byte(raw), "msg-1")
		require.Error(t, err)
	})
}

// TestUpdate_ConcurrentUpdateAndFlushDoesNotLoseData drives a stream of
// updates from one goroutine while another repeatedly calls Flush and
// Get concurrently. This exercises flushOne's channel-based wait for
// an in-flight write (rather than the old sleep-and-poll loop) under
// real contention: neither goroutine should ever observe a lost
// delta, a panic, or a stuck Flush.
func TestUpdate_ConcurrentUpdateAndFlushDoesNotLoseData(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(2*time.Millisecond))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			msg.AppendContent("x")
			require.NoError(t, svc.Update(t.Context(), msg))
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			require.NoError(t, svc.Flush(t.Context(), msg.ID))
			_, err := svc.Get(t.Context(), msg.ID)
			require.NoError(t, err)
		}
	}()

	wg.Wait()

	require.NoError(t, svc.Flush(t.Context(), msg.ID))
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Len(t, got.Content().Text, n, "no update should be lost across concurrent Update/Flush/Get")
}

// TestFlush_MultipleWaitersWakeAfterInFlightWrite verifies the
// channel-based wait mechanism wakes every queued waiter, not just
// one, when the in-flight write it is blocked behind completes. This
// is the case the old time.Sleep(time.Millisecond) poll loop handled
// only by accident (each waiter eventually re-checked on its own
// timer); here we assert it directly by racing several Flush callers
// against a single blocked SQL write and requiring all of them to
// return promptly once it is released.
func TestFlush_MultipleWaitersWakeAfterInFlightWrite(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	slow := &slowUpdateQuerier{
		Querier: q,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	svc := NewService(slow, WithDebounce(10*time.Millisecond))

	msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("payload")
	require.NoError(t, svc.Update(t.Context(), msg))

	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("timer-fired flush never reached UpdateMessage")
	}

	const waiters = 8
	results := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() { results <- svc.Flush(t.Context(), msg.ID) }()
	}

	// Give every goroutine a chance to reach the blocked wait before we
	// release the write, so this actually exercises multiple waiters
	// queued on the same flushDone channel rather than running serially.
	time.Sleep(20 * time.Millisecond)
	close(slow.release)

	for i := 0; i < waiters; i++ {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("a Flush waiter never woke after the in-flight write completed")
		}
	}

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "payload", got.Content().Text)
}

// TestFlushAll_DrainsQueuedUpdateUnderConcurrentWrites models the
// shutdown path: FlushAll is the sole drain mechanism in this design
// (no separate writer-goroutine lifecycle was introduced), so this
// proves a debounced update still queued when FlushAll runs survives
// it — the SQL row ends up updated, with no panic and no lost data —
// even while another goroutine keeps issuing updates concurrently.
func TestFlushAll_DrainsQueuedUpdateUnderConcurrentWrites(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			msg.AppendContent("y")
			require.NoError(t, svc.Update(t.Context(), msg))
		}
	}()

	// Race FlushAll against the still-running Update goroutine, the way
	// a shutdown path would race a stream that has not yet stopped.
	require.NoError(t, svc.FlushAll(t.Context()))
	wg.Wait()
	require.NoError(t, svc.FlushAll(t.Context()))

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Len(t, got.Content().Text, 20, "a queued update must survive FlushAll draining, even mid-stream")
}
