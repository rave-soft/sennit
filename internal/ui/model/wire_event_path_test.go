package model

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// This file closes PR 0.7's own documented gap (CLIENT-SERVER.md's "PR 0.7"
// section): every other harness in this package feeds events straight into
// Update, bypassing ws.Subscribe entirely, so the event codec path (JSON
// under SENNIT_TEST_WIRE=1, protobuf-over-gRPC framing under
// SENNIT_TEST_WIRE=grpc) is never actually exercised by anything the "wire"
// CI job runs. The tests below call ws.Subscribe(send) for real and feed
// whatever it delivers into m.Update, the same way cmd/root.go's
// `go ws.Subscribe(pacedSend)` does -- so a Params type that doesn't
// survive the real codec (permission.PermissionRequest.UnmarshalJSON's
// registry dispatch, specifically) breaks here under grpc mode, not just
// in wsrpc's own TestEventRegistry_RoundTrips.

// wireEventPathWorkspace is countingWorkspace (already a valid grpc-wired
// stub; see session_busy_test.go) plus a working Subscribe/SubscribeWith
// that actually stores the callback, instead of the no-op every other
// harness in this package installs. Under SENNIT_TEST_WIRE=grpc, that
// callback belongs to grpcws's root event hub (NewServer starts it eagerly
// and calls SubscribeWith once, at construction -- CLIENT-SERVER.md, PR
// 1.4a), so emit below feeds the real hub -> gRPC stream -> Client.Subscribe
// pipeline, not just a local closure.
type wireEventPathWorkspace struct {
	*countingWorkspace

	mu   sync.Mutex
	send func(any)
}

func (w *wireEventPathWorkspace) Subscribe(send func(any)) {
	w.SubscribeWith(send)
}

func (w *wireEventPathWorkspace) SubscribeWith(send func(any)) func() {
	w.mu.Lock()
	w.send = send
	w.mu.Unlock()
	return func() {}
}

// emit delivers v the same way a real workspace's event stream would:
// through whatever Subscribe/SubscribeWith installed, which under grpc mode
// is the server's own hub-ingestion callback, not a direct call into this
// test's own m.Update. Calling it before Subscribe/SubscribeWith installed
// a callback is a harness bug, not a per-test assertion, so it panics
// rather than silently dropping v.
func (w *wireEventPathWorkspace) emit(v any) {
	w.mu.Lock()
	send := w.send
	w.mu.Unlock()
	if send == nil {
		panic("wireEventPathWorkspace.emit: called before Subscribe/SubscribeWith installed a callback")
	}
	send(v)
}

// newWireEventPathUI builds a UI over wireEventPathWorkspace, wired through
// maybeWireWorkspace exactly like newBusyUI, then installs a real
// subscription: events reach eventsCh (a background goroutine may deliver
// them, e.g. under grpc mode's own client pump) and drainEvents feeds them
// into m.Update from the test's own goroutine, one at a time, the same way
// bubbletea's runtime would from Update's single-goroutine contract.
//
// It keeps its own reference to the wired workspace.Workspace (wired)
// rather than reading it back off m.com.Workspace: that field's static
// type is common.Workspace (workspace.FrontendWorkspace), which doesn't
// include Subscribe -- the same reason newBusyUI itself never needs to
// call it back.
func newWireEventPathUI(t *testing.T) (*UI, *wireEventPathWorkspace, chan any) {
	t.Helper()
	ws := &wireEventPathWorkspace{countingWorkspace: &countingWorkspace{ready: true}}
	wired := maybeWireWorkspace(t, ws)
	com := common.DefaultCommon(context.Background(), wired)
	m := newBusyUIFromCommon(com)

	eventsCh := make(chan any, 16)
	// SubscribeWith, not Subscribe: Subscribe blocks its caller until the
	// workspace's lifetime ends (cmd/root.go runs it in its own goroutine
	// for exactly that reason), which would hang this test's own
	// goroutine forever.
	stop := wired.SubscribeWith(func(v any) { eventsCh <- v })
	t.Cleanup(stop)

	return m, ws, eventsCh
}

// drainEvents applies every event currently queued in eventsCh to m.Update,
// running any resulting tea.Cmd through runCmds -- the same shape
// cmd/root.go's real pump-into-program.Send loop has, minus bubbletea
// itself. It waits up to 2s for at least one event to arrive first, so a
// caller doesn't race an async delivery (grpc mode's own client pump
// goroutine) with an empty channel.
func drainEvents(t *testing.T, m *UI, eventsCh chan any) {
	t.Helper()
	deadline := time.Now().Add(raceWait(2 * time.Second))
	got := false
	for time.Now().Before(deadline) {
		select {
		case v := <-eventsCh:
			got = true
			_, cmd := m.Update(v)
			runCmds(m, cmd)
		default:
			if got {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	if !got {
		t.Fatal("no event arrived on eventsCh before the deadline")
	}
}

// renderDialogOverlay snapshots just m.dialog (not the whole UI -- m.Draw
// needs a fully computed m.lay.layout, which this fixture's plain
// newBusyUI-style construction never runs, unlike the golden-test harness
// in command_driving_golden_test.go), stripped of ANSI so substring checks
// don't have to account for styling codes.
func renderDialogOverlay(m *UI) string {
	canvas := uv.NewScreenBuffer(m.lay.width, m.lay.height)
	m.dialog.Draw(canvas, canvas.Bounds())
	return ansi.Strip(canvas.Render())
}

// renderChat is renderDialogOverlay's chat-list counterpart. SetSize is
// needed before Draw: unlike the dialog overlay, list.List (which Chat
// wraps) only lays out and renders within a size it's been explicitly
// given, and this fixture's plain newBusyUI-style construction never runs
// the resize path that would otherwise set it.
func renderChat(m *UI) string {
	m.chat.SetSize(m.lay.width, m.lay.height)
	canvas := uv.NewScreenBuffer(m.lay.width, m.lay.height)
	m.chat.Draw(canvas, canvas.Bounds())
	return ansi.Strip(canvas.Render())
}

// TestWireEventPath_PermissionRequestRendersDiff feeds a real
// permission.PermissionRequest for an "edit" tool call through
// ws.Subscribe -- proving proto.EditPermissionsParams decodes end to end
// (permission.PermissionRequest.UnmarshalJSON's registry dispatch, PR
// 0.2/0.7) and the permissions dialog renders the resulting diff, not a
// raw-JSON fallback (see ui/dialog/permissions.go's diffContentRenderer
// and its own TestDiffContentRenderer_GuardStopsBeforeToDiff).
func TestWireEventPath_PermissionRequestRendersDiff(t *testing.T) {
	m, ws, eventsCh := newWireEventPathUI(t)

	perm := permission.PermissionRequest{
		ID:          "perm-edit-1",
		SessionID:   "s1",
		ToolCallID:  "call-edit-1",
		ToolName:    proto.EditToolName,
		Description: "edit the file",
		Action:      "edit",
		Path:        "/tmp/wire-event-path.go",
		Params: proto.EditPermissionsParams{
			FilePath:   "/tmp/wire-event-path.go",
			OldContent: "func before() {}\n",
			NewContent: "func afterWireEventPath() {}\n",
		},
	}
	ws.emit(pubsub.Event[permission.PermissionRequest]{Type: pubsub.CreatedEvent, Payload: perm})
	drainEvents(t, m, eventsCh)

	require.True(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"a permission request delivered over Subscribe must open the permissions dialog")

	out := renderDialogOverlay(m)
	require.Contains(t, out, "afterWireEventPath",
		"the dialog must render the decoded EditPermissionsParams' new content, not a raw-JSON fallback")
}

// TestWireEventPath_MessageWithToolCallRendersBoth feeds a real
// message.Message (text + a ToolCall part) through ws.Subscribe and checks
// the chat renders both -- message.Message's own MarshalParts/UnmarshalParts
// codec (message/message.go) round-tripping every ContentPart concrete type
// over the wire, not just in wsrpc's own codec tests.
func TestWireEventPath_MessageWithToolCallRendersBoth(t *testing.T) {
	m, ws, eventsCh := newWireEventPathUI(t)

	msg := message.Message{
		ID:        "wire-msg-1",
		SessionID: "s1",
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "wire-event-path-text"},
			message.ToolCall{ID: "wire-call-1", Name: "bash", Input: `{"command":"echo wire-event-path"}`, Finished: true},
		},
	}
	ws.emit(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: msg})
	drainEvents(t, m, eventsCh)

	require.NotNil(t, m.chat.MessageItem("wire-msg-1"), "the assistant message must land in the chat")
	require.NotNil(t, m.chat.MessageItem("wire-call-1"), "the tool call must render its own chat item")

	out := renderChat(m)
	require.Contains(t, out, "wire-event-path-text", "the message's text part must render")
	require.Contains(t, out, "echo wire-event-path", "the tool call's command must render")
}

// TestWireEventPath_MessageEventDrivesBusyIndicator publishes a
// pubsub.Event[message.Message] (a run boundary -- see applyMessageEvent's
// CreatedEvent branch) through ws.Subscribe with the stub already
// configured busy, then checks the UI's busy indicator (m.isAgentBusy)
// picks it up. AgentIsBusy is class C (CLIENT-SERVER.md, PR 1.4b): under
// SENNIT_TEST_WIRE=grpc, the refresh this event dispatches reads
// grpcws.Client's own cache, fed by the root hub's client-state publisher
// (wsrpctest.ServeGRPC's short tick), not a direct call into this stub --
// so this proves a ClientState change reaches the indicator through the
// real cache/tick path, not just an in-process getter call.
func TestWireEventPath_MessageEventDrivesBusyIndicator(t *testing.T) {
	m, ws, eventsCh := newWireEventPathUI(t)
	ws.agentBusy = true

	require.False(t, m.isAgentBusy(), "must start idle: nothing has refreshed the busy cache yet")

	ws.emit(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "wire-busy-1", SessionID: "s1", Role: message.User},
	})
	drainEvents(t, m, eventsCh)

	waitFor(t, func() bool { return m.isAgentBusy() })
}

// TestWireEventPath_ConnectionLostRecovered severs the bufconn transport
// under a live gRPC subscription and checks the header's connection-lost
// indicator (update_connection.go) appears, then clears once the client
// reconnects -- CLIENT-SERVER.md's PR 1.4c. This is the one scenario in
// this file with no in-process/loopback equivalent: an in-process
// Workspace never publishes workspace.ConnectionEvent at all (see
// connectionState's own doc comment), so under SENNIT_TEST_WIRE unset or
// "1" this only checks the indicator's quiescent default -- a real
// assertion, just a different one than under "grpc", where the transport
// genuinely drops and recovers.
func TestWireEventPath_ConnectionLostRecovered(t *testing.T) {
	ws := &wireEventPathWorkspace{countingWorkspace: &countingWorkspace{ready: true}}

	if os.Getenv(wireEnvVar) != "grpc" {
		m := newBusyUI(t, ws)
		require.False(t, m.conn.lost, "an in-process/loopback workspace never reports a lost connection")
		return
	}

	srv, stopHub := grpcws.NewServer(ws, grpcws.WithClientStateTickInterval(10*time.Millisecond))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	dialer := &severingDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := grpcws.NewClient(conn)
	require.NoError(t, client.Connect(context.Background()))
	t.Cleanup(client.Shutdown)

	com := common.DefaultCommon(context.Background(), client)
	m := newBusyUIFromCommon(com)

	eventsCh := make(chan any, 16)
	stop := client.SubscribeWith(func(v any) { eventsCh <- v })
	t.Cleanup(stop)

	// A single sever() races runPump's own goroutine startup (client.
	// Connect launches it asynchronously -- see client_pump.go's connect):
	// if runPump hasn't yet opened its event stream when the one
	// connection Snapshot dialed goes down, gRPC's own automatic
	// reconnect can re-dial and heal it, over bufconn's zero-latency
	// loop, before anything in this process ever calls Recv() on the
	// dying transport or NewStream() on the not-yet-healed one -- so no
	// failure is ever observed and ConnectionLost never fires. That is
	// specific to bufconn's instant redial, not a real transport (a real
	// reconnect takes long enough that the pump's own retry always finds
	// it still down), so severUntilLost's fix belongs here, not in
	// client_pump.go: keep severing on a short interval until the drop is
	// actually observed, which guarantees the eventual sever() lands
	// while runPump is genuinely blocked in Recv() and so cannot go
	// unnoticed the way a single racing sever() can.
	severUntilLost(t, dialer, m, eventsCh)

	// The client's own pump reconnects on its next attempt (the dialer
	// itself is still live -- only the sever()'d connections were cut);
	// wait for the Recovered event. This direction has no equivalent
	// race (m.conn.lost is already true, so there is a real stream up
	// and an active Recv() for the reconnect to be observed through),
	// but the wait is still async and worth a generous, event-driven
	// budget rather than a fixed sleep.
	requireConnLost(t, m, eventsCh, false, "the indicator must clear once the connection recovers")
}

// severUntilLost severs every currently open connection, then keeps
// draining eventsCh into m.Update and re-severing on a short interval
// until m.conn.lost is observed true or the deadline passes -- see the
// race its caller documents above. A sever() that lands while runPump is
// not yet blocked in Recv() can heal invisibly, so a single attempt
// cannot be trusted; repeating it is what actually closes the race,
// since each attempt is aimed at whatever connection is live at that
// moment (severingDialer.dial records every redial, including gRPC's own
// automatic ones).
func severUntilLost(t *testing.T, dialer *severingDialer, m *UI, eventsCh chan any) {
	t.Helper()
	deadline := time.Now().Add(raceWait(5 * time.Second))
	dialer.sever()
	for time.Now().Before(deadline) && !m.conn.lost {
		select {
		case v := <-eventsCh:
			_, cmd := m.Update(v)
			runCmds(m, cmd)
		case <-time.After(20 * time.Millisecond):
			dialer.sever()
		}
	}
	require.True(t, m.conn.lost, "a severed connection must set the lost indicator")
}

// requireConnLost drains eventsCh into m.Update until m.conn.lost matches
// want or the deadline passes, then asserts it matches.
func requireConnLost(t *testing.T, m *UI, eventsCh chan any, want bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(raceWait(5 * time.Second))
	for time.Now().Before(deadline) && m.conn.lost != want {
		select {
		case v := <-eventsCh:
			_, cmd := m.Update(v)
			runCmds(m, cmd)
		case <-time.After(10 * time.Millisecond):
		}
	}
	require.Equal(t, want, m.conn.lost, msg)
}

// severingDialer wraps a bufconn.Listener's dialer, tracking every
// connection it hands out so sever can close them -- mirroring grpcws's
// own events_stream_test.go severableDialer, duplicated here rather than
// exported from an internal _test.go file across packages.
type severingDialer struct {
	lis *bufconn.Listener

	mu    sync.Mutex
	conns []net.Conn
}

func (d *severingDialer) dial(ctx context.Context, _ string) (net.Conn, error) {
	conn, err := d.lis.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conns = append(d.conns, conn)
	d.mu.Unlock()
	return conn, nil
}

func (d *severingDialer) sever() {
	d.mu.Lock()
	conns := d.conns
	d.conns = nil
	d.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}
