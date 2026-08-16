package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUserInputSignal covers the channel a waiting tool selects on: the
// same channel while nobody has spoken, closed exactly once when a prompt
// is queued, and a fresh one after that so a tool interrupted earlier in
// the turn can arm itself again.
func TestUserInputSignal(t *testing.T) {
	t.Parallel()

	d := newDispatcher()

	first := d.userInputChan("s1")
	require.NotNil(t, first)
	require.Equal(t, first, d.userInputChan("s1"),
		"a second waiter in the same turn must watch the same signal")

	select {
	case <-first:
		t.Fatal("the signal fired before any prompt was queued")
	default:
	}

	d.signalUserInput("s1")
	select {
	case <-first:
	default:
		t.Fatal("queuing a prompt must release the waiter")
	}

	second := d.userInputChan("s1")
	select {
	case <-second:
		t.Fatal("the next wait must start from an unfired signal")
	default:
	}

	// Signalling a session nobody waits on is a no-op, not a panic: most
	// turns never call a waiting tool at all.
	require.NotPanics(t, func() { d.signalUserInput("nobody-is-waiting") })
	// And a second signal for a session whose channel was already closed
	// and dropped must not close it twice.
	d.signalUserInput("s1")
	require.NotPanics(t, func() { d.signalUserInput("s1") })
}

// TestUserInputSignalIsPerSession: one session's prompt must not release a
// tool waiting in another.
func TestUserInputSignalIsPerSession(t *testing.T) {
	t.Parallel()

	d := newDispatcher()
	other := d.userInputChan("s2")
	d.userInputChan("s1")

	d.signalUserInput("s1")
	select {
	case <-other:
		t.Fatal("a prompt in one session released a waiter in another")
	default:
	}
}
