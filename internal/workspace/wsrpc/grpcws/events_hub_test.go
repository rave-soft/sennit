package grpcws

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestEventHub_LiveDeliveryInOrder checks the FromSeq==0 path: a
// subscriber added after some history sees only what's published from
// then on, in order, each with the Seq publish assigned it.
func TestEventHub_LiveDeliveryInOrder(t *testing.T) {
	t.Parallel()

	h := newEventHub(8)
	sub, replay, resync := h.subscribe(0)
	require.Empty(t, replay)
	require.False(t, resync)

	h.publish(wsrpctest.SessionEvent)
	h.publish(wsrpctest.SessionEvent)

	first := <-sub.ch
	second := <-sub.ch
	require.Equal(t, uint64(1), first.Seq)
	require.Equal(t, uint64(2), second.Seq)
	require.NotNil(t, first.Event)
	require.Equal(t, "session", first.Event.Type)
}

// TestEventHub_ReplayFromSeq checks that a reconnecting subscriber whose
// FromSeq lands inside the buffer gets exactly the events from FromSeq
// onward, not FromSeq+1 onward and not from the start of the buffer.
func TestEventHub_ReplayFromSeq(t *testing.T) {
	t.Parallel()

	h := newEventHub(8)
	for range 5 {
		h.publish(wsrpctest.SessionEvent)
	}

	_, replay, resync := h.subscribe(3)
	require.False(t, resync)
	require.Len(t, replay, 3, "expected Seq 3,4,5 replayed")
	require.Equal(t, uint64(3), replay[0].seq)
	require.Equal(t, uint64(4), replay[1].seq)
	require.Equal(t, uint64(5), replay[2].seq)

	// FromSeq == nextSeq (one past the latest published): no gap, no
	// resync, nothing to replay -- purely live from here.
	_, replay, resync = h.subscribe(6)
	require.False(t, resync)
	require.Empty(t, replay)
}

// TestEventHub_ResyncWhenFromSeqTooOld checks the "N older than the
// oldest buffered Seq" half of the Resync rule.
func TestEventHub_ResyncWhenFromSeqTooOld(t *testing.T) {
	t.Parallel()

	h := newEventHub(4)
	for range 10 {
		h.publish(wsrpctest.SessionEvent)
	}
	// Buffer now holds Seq 7..10; Seq 1 is long gone.
	_, replay, resync := h.subscribe(1)
	require.True(t, resync)
	require.Empty(t, replay)
}

// TestEventHub_ResyncWhenFromSeqTooNew checks the "N newer than latest+1"
// half of the Resync rule -- a client claiming to have seen an event this
// hub never produced.
func TestEventHub_ResyncWhenFromSeqTooNew(t *testing.T) {
	t.Parallel()

	h := newEventHub(8)
	h.publish(wsrpctest.SessionEvent)
	_, replay, resync := h.subscribe(99)
	require.True(t, resync)
	require.Empty(t, replay)
}

// TestEventHub_BufferOverflowTriggersResync is the buffer-overflow
// acceptance test: capacity 8, 20 events published while nobody is
// subscribed, then a reconnect at FromSeq=1 must resync rather than
// silently replaying an incomplete tail.
func TestEventHub_BufferOverflowTriggersResync(t *testing.T) {
	t.Parallel()

	h := newEventHub(8)
	for range 20 {
		h.publish(wsrpctest.SessionEvent)
	}

	_, replay, resync := h.subscribe(1)
	require.True(t, resync)
	require.Empty(t, replay)

	// The buffer kept exactly the last 8: Seq 13..20.
	_, replay, resync = h.subscribe(13)
	require.False(t, resync)
	require.Len(t, replay, 8)
	require.Equal(t, uint64(13), replay[0].seq)
	require.Equal(t, uint64(20), replay[len(replay)-1].seq)
}

// TestEventHub_SlowSubscriberDoesNotBlockPublish is the slow-subscriber
// acceptance test: a subscriber that never drains its channel must not
// slow down delivery to one that does, and publish itself must never
// block on the saturated one. The correctness half (the fast subscriber
// still gets everything) is not skipped under -race; the wall-clock bound
// is loose enough to keep even there (see the repo's wall-clock-budget
// practice for tests that time something under -race).
func TestEventHub_SlowSubscriberDoesNotBlockPublish(t *testing.T) {
	t.Parallel()

	h := newEventHub(2)
	slow, _, _ := h.subscribe(0)
	fast, _, _ := h.subscribe(0)

	const n = 50
	got := make(chan struct{}, n)
	go func() {
		for range n {
			<-fast.ch
			got <- struct{}{}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range n {
			h.publish(wsrpctest.SessionEvent)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on the slow subscriber's full channel")
	}

	for range n {
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("fast subscriber did not receive every event")
		}
	}

	// The slow subscriber overflowed its 2-slot channel long ago: it was
	// closed after the first overflow (whatever was already queued, plus
	// maybe a best-effort Resync frame, drains before the close shows up
	// as !ok), and never blocked the loop above.
	ok := true
	for ok {
		select {
		case _, ok = <-slow.ch:
		case <-time.After(time.Second):
			t.Fatal("slow subscriber's channel never closed")
		}
	}
}

// TestEventHub_EncodeErrorDropsEventButHubSurvives feeds publish a value
// with no registry entry (wsrpc.EncodeEvent errors on it) between two
// good ones, and requires the hub to log-and-drop rather than crash: the
// good events either side must both still arrive, with consecutive Seqs
// (the failed one never got one).
func TestEventHub_EncodeErrorDropsEventButHubSurvives(t *testing.T) {
	t.Parallel()

	h := newEventHub(8)
	sub, _, _ := h.subscribe(0)

	h.publish(wsrpctest.SessionEvent)
	h.publish(42) // not a registered event type: EncodeEvent errors.
	h.publish(wsrpctest.SessionEvent)

	first := <-sub.ch
	second := <-sub.ch
	require.Equal(t, uint64(1), first.Seq)
	require.Equal(t, uint64(2), second.Seq, "the unencodable event must not have consumed a Seq")
}

// TestEventHub_EnsureStartedIsIdempotent checks that only one upstream
// SubscribeWith call happens no matter how many streams attach, and that
// close() stops it.
func TestEventHub_EnsureStartedIsIdempotent(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{}
	calls := 0
	stub.SubscribeWithStop = func() {}
	h := newEventHub(8)
	for range 3 {
		h.ensureStarted(wrappedSubscriber{stub, &calls})
	}
	require.Equal(t, 1, calls, "ensureStarted must call SubscribeWith exactly once")
	h.close()
}

// wrappedSubscriber counts SubscribeWith calls before delegating, since
// StubWorkspace itself only records the last one.
type wrappedSubscriber struct {
	stub  *wsrpctest.StubWorkspace
	calls *int
}

func (w wrappedSubscriber) SubscribeWith(send func(any)) func() {
	*w.calls++
	return w.stub.SubscribeWith(send)
}

// panicResolveHub is a resolveHub that panics, used to prove
// eventsServer.Subscribe's own recover (subscribe.go) turns a panic into
// a returned error instead of crashing the process -- see
// CLIENT-SERVER.md's PR 1.2 build step 3: grpc-go does not recover a
// streaming handler's panics on its own.
func panicResolveHub(context.Context) (*eventHub, error) {
	panic("boom: simulated panic inside resolveHub")
}

// fakeSubscribeServer is a minimal WorkspaceEventsSubscribeServer good
// enough to drive eventsServer.Subscribe directly, without a real gRPC
// connection.
type fakeSubscribeServer struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeSubscribeServer) Context() context.Context { return f.ctx }

func (f *fakeSubscribeServer) Send(*EventFrame) error { return nil }

func TestEventsServer_SubscribePanicIsRecovered(t *testing.T) {
	t.Parallel()

	s := &eventsServer{resolveHub: panicResolveHub}
	err := s.Subscribe(&SubscribeRequest{}, &fakeSubscribeServer{ctx: context.Background()})
	require.Error(t, err, "a panic inside Subscribe must come back as an error, not crash the process")
	require.Equal(t, codes.Internal, status.Code(err))
}
