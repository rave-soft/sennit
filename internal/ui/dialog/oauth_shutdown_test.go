package dialog

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestOAuthCopilotPollStopsOnShutdown pins the bug this rule exists for: a
// device-flow poll used to be built on context.Background(), so quitting the
// TUI mid-poll left the goroutine happily hitting GitHub's token endpoint
// forever. startPolling now derives its context from com.Context(), the
// program's own lifecycle context — cancel that (what happens on shutdown)
// and the poll must return immediately instead of waiting out its interval
// or timeout.
//
// The device code's ExpiresIn/Interval are set far larger than the test's
// own deadline, so a poll that is still running when the timeout below
// fires can only be explained by the context not actually being wired
// through — not by a slow but real deadline/interval firing on its own.
func TestOAuthCopilotPollStopsOnShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s := styles.SennitDark()
	com := &common.Common{Styles: &s, Ctx: ctx}

	// The stub flow blocks until its context is done, standing in for a
	// real poll whose interval and expiry are far larger than this test's
	// own deadline: a poll still running when the timeout below fires can
	// only be explained by the context not being wired through.
	m := &OAuthCopilot{com: com, flow: &stubDialogOAuthFlow{}}

	pollCmd := m.startPolling("test-device-code", 3600)

	// Simulate the TUI shutting down while the poll is in flight.
	cancel()

	result := make(chan tea.Msg, 1)
	go func() { result <- pollCmd() }()

	select {
	case msg := <-result:
		// stopPolling's own contract (see startPolling): a cancelled poll
		// reports nothing rather than an error, since the dialog is gone.
		require.Nil(t, msg, "a cancelled poll must not report an error to a dialog that no longer exists")
	case <-time.After(3 * time.Second):
		t.Fatal("poll did not stop when its context was cancelled")
	}
}
