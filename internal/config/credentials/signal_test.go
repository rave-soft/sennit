package credentials

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSignalAuthComplete_RepeatedCallWithNoWaiterPreservesPreSignal guards
// against a bug where SignalAuthComplete unconditionally deleted the
// provider's entry from authSignals before checking whether the channel
// found there was already closed. Two SignalAuthComplete calls in a row
// with no WaitForTokenChange in between: the first call pre-creates and
// closes a channel (there is no waiter yet, so this is the "pre-signal"
// path documented on SignalAuthComplete); the second call found that
// closed channel, deleted it from the map regardless, and its own select
// then took the "already closed; nothing to do" branch — leaving nothing
// in the map. A later WaitForTokenChange registered a brand new, open
// channel and blocked for the full context timeout instead of returning
// immediately, contradicting WaitForTokenChange's own doc comment.
func TestSignalAuthComplete_RepeatedCallWithNoWaiterPreservesPreSignal(t *testing.T) {
	t.Parallel()

	m := New(newFakeStore(nil, "", ""))

	m.SignalAuthComplete("test-provider")
	m.SignalAuthComplete("test-provider") // repeated, no waiter in between

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := m.WaitForTokenChange(ctx, "test-provider")
	require.NoError(t, err, "a pre-signal must survive a repeated SignalAuthComplete call and let WaitForTokenChange return immediately")
}

// TestSignalAuthComplete_UnblocksExistingWaiter is the ordinary path this
// pre-signal machinery exists to support: a WaitForTokenChange already
// blocked when SignalAuthComplete fires must be released.
func TestSignalAuthComplete_UnblocksExistingWaiter(t *testing.T) {
	t.Parallel()

	m := New(newFakeStore(nil, "", ""))

	done := make(chan error, 1)
	go func() {
		done <- m.WaitForTokenChange(context.Background(), "test-provider")
	}()

	// Give WaitForTokenChange a moment to register before signaling.
	require.Eventually(t, func() bool {
		m.authSignalMu.Lock()
		defer m.authSignalMu.Unlock()
		_, ok := m.authSignals["test-provider"]
		return ok
	}, time.Second, time.Millisecond)

	m.SignalAuthComplete("test-provider")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTokenChange did not unblock after SignalAuthComplete")
	}
}
