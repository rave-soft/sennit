// This file exercises grpcws's Subscribe stream end to end (real gRPC
// over bufconn), the CLIENT-SERVER.md PR 1.2 acceptance tests:
// in-order delivery of several registered event types, reconnect without
// loss, and the lifecycle knobs (SubscribeWith's stop, Client.Shutdown)
// that Subscribe/SubscribeWith build on. events_hub_test.go (package
// grpcws) covers the buffer-overflow and slow-subscriber cases at the hub
// level directly, where they don't need a real network round trip.
package grpcws_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// waitFor polls cond until it's true or the deadline passes, failing t if
// it never is. Used instead of a fixed sleep for "the client's reconnect
// loop eventually notices" assertions.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.True(t, cond(), "condition never became true within %s", timeout)
}

// waitSubscribed blocks until this test's SubscribeWith/Subscribe call has
// reached the hub (via stub.SubscribeWithReady, which gives a
// happens-before edge -race can see -- a bare poll of
// stub.SubscribeWithSend races its write on the gRPC stream handler's own
// goroutine), then retries a harmless warmup event until count() confirms
// delivery, and returns the send func to use for the rest of the test.
//
// SubscribeWithReady firing only proves eventHub.ensureStarted has run
// (the hub's one-time upstream ws.SubscribeWith call); it does not prove
// this particular gRPC stream has registered itself with the hub yet --
// eventsServer.Subscribe does those two things one after the other on its
// own goroutine (see eventHub.subscribe), so there's a real, if narrow,
// gap in which a publish reaches nobody. Retrying the warmup event closes
// that gap instead of racing it: stub.SubscribeWithSend records only the
// latest call, so re-sending the same harmless event is safe.
//
// count, not a bool: every caller clears its recorder right after this
// call and then asserts an exact sequence, so a retry sent before
// delivery caught up -- still traveling the same real hub/gRPC pipeline
// as any other event once issued, not superseded by a later retry --
// must not be able to land after that clear. Under real delivery latency
// (the CI race job's own cross-package contention; see AGENTS.md's
// "wall-clock budgets under -race") more than one retry can be in flight
// by the time the first one is observed, so this waits for arrivals to
// go quiet for a short settle window before returning, on top of the
// count going positive at all. This reproduced without any change to the
// retry loop itself, just by widening its own deadline (raceWait) enough
// for several retries to stack up -- see the regression this settle
// phase fixes in git history's CI investigation.
func waitSubscribed(t *testing.T, stub *wsrpctest.StubWorkspace, count func() int) func(any) {
	t.Helper()
	select {
	case <-stub.SubscribeWithReady:
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("SubscribeWith was never called")
	}
	send := stub.SubscribeWithSend
	deadline := time.Now().Add(raceWait(5 * time.Second))
	for time.Now().Before(deadline) && count() == 0 {
		send(wsrpctest.SessionEvent)
		time.Sleep(5 * time.Millisecond)
	}
	require.Greater(t, count(), 0, "warmup event was never delivered to the new stream")

	settleDeadline := time.Now().Add(raceWait(2 * time.Second))
	last := count()
	stableSince := time.Now()
	for time.Now().Before(settleDeadline) {
		time.Sleep(5 * time.Millisecond)
		if n := count(); n != last {
			last = n
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= 25*time.Millisecond {
			break
		}
	}
	return send
}

func TestSubscribe_DeliversRegisteredEventTypesInOrder(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	client := newServerAndClient(t, stub)

	var mu sync.Mutex
	var got []any
	stop := client.SubscribeWith(func(v any) {
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
	})
	t.Cleanup(stop)

	send := waitSubscribed(t, stub, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	})
	mu.Lock()
	got = nil // drop the warmup event(s); only the sequence below counts.
	mu.Unlock()

	messageEvent := pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: wsrpctest.MessageSample[0]}
	permissionEvent := pubsub.Event[permission.PermissionRequest]{Type: pubsub.CreatedEvent, Payload: wsrpctest.PermissionRequestSample}

	send(wsrpctest.SessionEvent)
	send(messageEvent)
	send(permissionEvent)

	waitFor(t, raceWait(5*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	})

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []any{wsrpctest.SessionEvent, messageEvent, permissionEvent}, got)
}

// severableDialer wraps a bufconn.Listener's dialer, tracking every
// connection it hands out so a test can sever() them -- simulating a
// dropped network connection while the server keeps listening, so the
// client's own reconnect logic (not a fresh dial from the test) is what's
// under test.
type severableDialer struct {
	lis *bufconn.Listener

	mu    sync.Mutex
	conns []net.Conn
}

func (d *severableDialer) dial(ctx context.Context, _ string) (net.Conn, error) {
	conn, err := d.lis.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conns = append(d.conns, conn)
	d.mu.Unlock()
	return conn, nil
}

func (d *severableDialer) sever() {
	d.mu.Lock()
	conns := d.conns
	d.conns = nil
	d.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func TestSubscribe_ReconnectWithoutLoss(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	srv, stopHub := grpcws.NewServer(stub)
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	dialer := &severableDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

	var mu sync.Mutex
	var got []any
	var states []workspace.ConnectionState
	client.SubscribeWith(func(v any) {
		mu.Lock()
		defer mu.Unlock()
		if ce, ok := v.(pubsub.Event[workspace.ConnectionEvent]); ok {
			states = append(states, ce.Payload.State)
			return
		}
		got = append(got, v)
	})

	send := waitSubscribed(t, stub, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	})
	mu.Lock()
	got = nil
	states = nil
	mu.Unlock()

	send(wsrpctest.SessionEvent)
	waitFor(t, raceWait(5*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})

	// Sever the transport, then keep publishing while the client has no
	// live connection -- these must not be lost.
	dialer.sever()
	send(wsrpctest.SessionEvent)
	send(wsrpctest.SessionEvent)

	waitFor(t, raceWait(10*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	})

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []any{wsrpctest.SessionEvent, wsrpctest.SessionEvent, wsrpctest.SessionEvent}, got)
	require.Contains(t, states, workspace.ConnectionLost)
	require.Contains(t, states, workspace.ConnectionRecovered)
	lostAt := indexOf(states, workspace.ConnectionLost)
	recoveredAt := lastIndexOf(states, workspace.ConnectionRecovered)
	require.Greater(t, recoveredAt, lostAt, "a Recovered must follow the Lost")
}

// TestSubscribe_RecoversOnIdleStreamWithNoEvents is
// TestSubscribe_ReconnectWithoutLoss's counterpart with nothing published
// after the sever: eventsServer.Subscribe must still hand the client a
// Recv() to observe, or ConnectionRecovered never fires and the "connection
// lost" indicator sticks forever on an otherwise healthy, idle reconnect.
func TestSubscribe_RecoversOnIdleStreamWithNoEvents(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	srv, stopHub := grpcws.NewServer(stub)
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	dialer := &severableDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

	var mu sync.Mutex
	var got []any
	var states []workspace.ConnectionState
	client.SubscribeWith(func(v any) {
		mu.Lock()
		defer mu.Unlock()
		if ce, ok := v.(pubsub.Event[workspace.ConnectionEvent]); ok {
			states = append(states, ce.Payload.State)
			return
		}
		got = append(got, v)
	})

	send := waitSubscribed(t, stub, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	})
	mu.Lock()
	got = nil
	states = nil
	mu.Unlock()

	send(wsrpctest.SessionEvent)
	waitFor(t, raceWait(5*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})

	// Sever the transport and publish nothing else at all -- an idle
	// stream with no event to Recv() until the daemon's next real activity,
	// which on an idle connection may never come.
	dialer.sever()

	waitFor(t, raceWait(10*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(states) > 0 && states[len(states)-1] == workspace.ConnectionRecovered
	})

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, states, workspace.ConnectionLost)
	require.Contains(t, states, workspace.ConnectionRecovered)
}

func indexOf(states []workspace.ConnectionState, want workspace.ConnectionState) int {
	for i, s := range states {
		if s == want {
			return i
		}
	}
	return -1
}

func lastIndexOf(states []workspace.ConnectionState, want workspace.ConnectionState) int {
	idx := -1
	for i, s := range states {
		if s == want {
			idx = i
		}
	}
	return idx
}

func TestSubscribeWith_StopEndsDelivery(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	client := newServerAndClient(t, stub)

	var mu sync.Mutex
	var count int
	stop := client.SubscribeWith(func(any) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	send := waitSubscribed(t, stub, func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	})
	mu.Lock()
	count = 0
	mu.Unlock()

	send(wsrpctest.SessionEvent)
	waitFor(t, raceWait(5*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return count == 1
	})

	stop()

	send(wsrpctest.SessionEvent)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, count, "stop must end delivery to this subscription")
}

func TestClient_ShutdownEndsSubscribe(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	client := newServerAndClient(t, stub)

	subscribeReturned := make(chan struct{})
	go func() {
		client.Subscribe(func(any) {})
		close(subscribeReturned)
	}()

	select {
	case <-stub.SubscribeWithReady:
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("Subscribe never reached the server")
	}
	client.Shutdown()

	select {
	case <-subscribeReturned:
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("Subscribe did not return after Shutdown")
	}
}

// TestNewServer_EventHubNoGoroutineLeak checks that once a served hub's
// only stream disconnects and the server itself is stopped, nothing --
// the hub's upstream SubscribeWith goroutine included -- is left running.
func TestNewServer_EventHubNoGoroutineLeak(t *testing.T) {
	ignoreBaseline := goleak.IgnoreCurrent()

	func() {
		stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
		srv, stopHub := grpcws.NewServer(stub)
		lis := bufconn.Listen(bufSize)
		go func() { _ = srv.Serve(lis) }()

		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		require.NoError(t, err)
		client := grpcws.NewClient(conn)

		stop := client.SubscribeWith(func(any) {})
		select {
		case <-stub.SubscribeWithReady:
		case <-time.After(raceWait(5 * time.Second)):
			t.Fatal("SubscribeWith was never called")
		}
		stop()
		client.Shutdown()

		require.NoError(t, conn.Close())
		srv.Stop()
		stopHub()
		require.NoError(t, lis.Close())
	}()

	// The hub's stop() call and the server's own shutdown are both
	// synchronous from the caller's point of view, but the stream
	// handler goroutine and gRPC's own internal ones exit asynchronously
	// -- give them a moment before asserting no leak.
	waitFor(t, raceWait(5*time.Second), func() bool {
		return goleak.Find(ignoreBaseline) == nil
	})
}

// TestSubscribe_BufferOverflowTriggersResyncOverTheWire is
// TestEventHub_BufferOverflowTriggersResync's end-to-end counterpart: a
// server built with WithEventBufferSize(8) whose client gets disconnected
// (like TestSubscribe_ReconnectWithoutLoss) and misses more events than
// the buffer holds must see a ConnectionResync, and keep receiving live
// events afterward, rather than a silently incomplete replay.
func TestSubscribe_BufferOverflowTriggersResyncOverTheWire(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	srv, stopHub := grpcws.NewServer(stub, grpcws.WithEventBufferSize(8))
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	dialer := &severableDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

	var mu sync.Mutex
	var resyncs int
	var got []any
	client.SubscribeWith(func(v any) {
		mu.Lock()
		defer mu.Unlock()
		if ce, ok := v.(pubsub.Event[workspace.ConnectionEvent]); ok {
			if ce.Payload.State == workspace.ConnectionResync {
				resyncs++
			}
			return
		}
		got = append(got, v)
	})

	send := waitSubscribed(t, stub, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	})
	mu.Lock()
	got = nil
	resyncs = 0
	mu.Unlock()

	dialer.sever()
	for range 20 {
		send(wsrpctest.SessionEvent)
	}

	waitFor(t, raceWait(10*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return resyncs > 0
	})

	// Live delivery keeps working after the resync.
	send(wsrpctest.SessionEvent)
	waitFor(t, raceWait(5*time.Second), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
}
