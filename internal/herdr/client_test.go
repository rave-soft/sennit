package herdr

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	for _, key := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

// recordingSender captures state transitions without connecting to a
// real Unix socket.
type recordingSender struct {
	states  []string
	methods []string
}

func (r *recordingSender) send(req reportRequest) error {
	r.states = append(r.states, req.Params.State)
	r.methods = append(r.methods, req.Method)
	return nil
}

func (r *recordingSender) close() {}

// newTestClient creates a Client that records state transitions
// without connecting to a real Unix socket.
func newTestClient() *Client {
	rec := &recordingSender{states: make([]string, 0, 16)}
	return &Client{
		state: stateIdle,
		snd:   rec,
	}
}

// reportedStates returns the states recorded by the test sender.
func reportedStates(c *Client) []string {
	return c.snd.(*recordingSender).states
}

func TestBasicLifecycle(t *testing.T) {
	t.Parallel()
	c := newTestClient()

	// Assistant message starts working.
	c.HandleEvent(AssistantMessage{SessionID: "sess-1"})
	assert.Equal(t, []string{stateWorking}, reportedStates(c))

	// Run complete returns to idle.
	c.HandleEvent(RunComplete{SessionID: "sess-1"})
	assert.Equal(t, []string{stateWorking, stateIdle}, reportedStates(c))
}

func TestPermissionBlockAndUnblock(t *testing.T) {
	t.Parallel()
	c := newTestClient()

	// Start working.
	c.HandleEvent(AssistantMessage{SessionID: "sess-1"})

	// Permission request blocks.
	c.HandleEvent(PermissionRequested{})
	assert.Equal(t, []string{stateWorking, stateBlocked}, reportedStates(c))

	// Permission granted returns to working (run still active).
	c.HandleEvent(PermissionResolved{})
	assert.Equal(t, []string{stateWorking, stateBlocked, stateWorking}, reportedStates(c))

	// Run complete returns to idle.
	c.HandleEvent(RunComplete{SessionID: "sess-1"})
	assert.Equal(t, []string{stateWorking, stateBlocked, stateWorking, stateIdle}, reportedStates(c))
}

func TestPermissionBeforeAssistantMessage(t *testing.T) {
	t.Parallel()
	c := newTestClient()

	// Permission request arrives before any assistant message.
	// This can happen when tool calls fire before text output.
	c.HandleEvent(PermissionRequested{})
	assert.Equal(t, []string{stateBlocked}, reportedStates(c))

	// Permission resolved should return to working, not idle,
	// because the permission request implied a run was active.
	c.HandleEvent(PermissionResolved{})
	assert.Equal(t, []string{stateBlocked, stateWorking}, reportedStates(c))
}

func TestSessionIDPropagation(t *testing.T) {
	t.Parallel()
	c := newTestClient()

	// SetSessionID before events.
	c.SetSessionID("early-session")
	assert.Equal(t, "early-session", c.sessionID)

	// RunComplete also updates session ID.
	c.HandleEvent(RunComplete{SessionID: "final-session"})
	assert.Equal(t, "final-session", c.sessionID)
}

func TestDedupSkipsRedundantState(t *testing.T) {
	t.Parallel()
	c := newTestClient()

	// Two assistant messages in a row should only report working once.
	c.HandleEvent(AssistantMessage{SessionID: "s1"})
	c.HandleEvent(AssistantMessage{SessionID: "s1"})
	assert.Equal(t, []string{stateWorking}, reportedStates(c))
}

func TestSummarizingTriggersWorking(t *testing.T) {
	t.Parallel()
	c := newTestClient()

	// Summarizing event should trigger working.
	c.HandleEvent(Summarizing{})
	assert.Equal(t, []string{stateWorking}, reportedStates(c))

	// Second summarizing should not trigger another state change.
	c.HandleEvent(Summarizing{})
	assert.Equal(t, []string{stateWorking}, reportedStates(c))
}

func TestNilClientSafe(t *testing.T) {
	t.Parallel()
	var c *Client
	// These should not panic on a nil receiver.
	c.SetSessionID("s1")
	c.HandleEvent(AssistantMessage{SessionID: "s1"})
	c.HandleEvent(RunComplete{SessionID: "s1"})
	c.HandleEvent(PermissionRequested{})
	c.HandleEvent(PermissionResolved{})
	c.HandleEvent(Summarizing{})
}

func TestRegisterInitial(t *testing.T) {
	t.Parallel()
	rec := &recordingSender{states: make([]string, 0, 16)}
	c := &Client{
		state: stateIdle,
		seq:   100,
		snd:   rec,
	}
	c.registerInitial()
	assert.Equal(t, []string{stateIdle}, rec.states)
	// seq must strictly increase so herdr accepts the report.
	assert.Equal(t, uint64(101), c.seq)
}

// TestCloseReleaseGoesThroughSender guards against releaseAgent sending
// directly on the socket instead of through c.snd (the same queue every
// other report uses). If release bypasses the queue, a buffered report
// still sitting there can be delivered after the release reaches herdr,
// so herdr sees a stale "working" state after the pane was released.
// This is checked here by injecting a plain in-memory sender: unlike a
// real Unix socket, it has no transport of its own, so a release that
// bypasses it (as the old dialSend call did, targeting an empty
// socketPath) would silently vanish instead of being recorded.
func TestCloseReleaseGoesThroughSender(t *testing.T) {
	t.Parallel()
	c := newTestClient()

	c.HandleEvent(AssistantMessage{SessionID: "s1"})
	c.Close()

	rec := c.snd.(*recordingSender)
	assert.Equal(t, []string{stateWorking, ""}, rec.states)
	assert.Equal(t, []string{"pane.report_agent", "pane.release_agent"}, rec.methods)
}

// TestUnixSender_SendDuringClose hammers send concurrently with close: the
// forward goroutines in translate.go keep calling HandleEvent (and so
// send) until their own context is cancelled, with nothing guaranteeing
// that happens before Close() runs. A send racing a close that has
// already closed the channel must not panic.
func TestUnixSender_SendDuringClose(t *testing.T) {
	t.Parallel()
	s := newUnixSender("/nonexistent/herdr-test.sock")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.send(reportRequest{})
				}
			}
		})
	}

	s.close()
	close(stop)
	wg.Wait()
}

// TestUnixSender_CloseTwiceIsNoOp guards against a regression to the old
// unconditional close(s.ch): an App that shares a herdr client with
// another App (a spawned thread before that sharing was fixed) can have
// close called on the same sender from two independent shutdowns, and the
// second call must return promptly instead of panicking on a
// double-close of the channel.
func TestUnixSender_CloseTwiceIsNoOp(t *testing.T) {
	t.Parallel()
	s := newUnixSender("/nonexistent/herdr-test.sock")

	s.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.close()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second close blocked")
	}
}

func TestNewFromEnvInitializesWithInjectedSender(t *testing.T) {
	values := map[string]string{
		"HERDR_ENV":         "1",
		"HERDR_SOCKET_PATH": "/tmp/herdr.sock",
		"HERDR_PANE_ID":     "test:pane",
	}
	recorder := &recordingSender{}
	client := newFromEnv(func(key string) string { return values[key] }, func(string) sender { return recorder })

	assert.Equal(t, "/tmp/herdr.sock", client.socketPath)
	assert.Equal(t, "test:pane", client.paneID)
	assert.Equal(t, []string{stateIdle}, recorder.states)
	assert.Equal(t, []string{"pane.report_agent"}, recorder.methods)
}
