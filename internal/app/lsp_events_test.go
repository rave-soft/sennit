package app

import (
	"fmt"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/stretchr/testify/require"
)

// TestLSPEvents_DiagnosticsUpdateDoesNotClobberConcurrentStateChange is the
// regression test for updateLSPDiagnostics' non-atomic get-modify-set
// racing with updateLSPState on the same client name.
//
// updateLSPDiagnostics preserves every field but DiagnosticCount from its
// own Get, including State. If that Get lands before a concurrent
// updateLSPState call's Set, but updateLSPDiagnostics' own Set lands
// after it, the diagnostics call's stale snapshot silently reverts the
// state change it never saw. Given a client seeded at StateStarting, and
// updateLSPState(..., StateReady, ...) racing updateLSPDiagnostics(...)
// on the same name, the client's State can only ever end up back at
// StateReady under correct serialization (see the analysis in the code
// review this fixes): whichever call runs last always observes the
// other's fully-applied write, so State never regresses. Without
// serializing the two get-modify-set sequences, State can be observed
// back at StateStarting — the exact anomaly this test rules out over
// many trials, since any single trial can get lucky with scheduling.
func TestLSPEvents_DiagnosticsUpdateDoesNotClobberConcurrentStateChange(t *testing.T) {
	const trials = 2000
	name := "gopls"

	for i := 0; i < trials; i++ {
		le := newLSPEvents()
		le.updateLSPState(name, lsp.StateStarting, nil, nil)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			le.updateLSPDiagnostics(name, 5)
		}()
		go func() {
			defer wg.Done()
			le.updateLSPState(name, lsp.StateReady, nil, nil)
		}()
		wg.Wait()

		info, ok := le.GetLSPState(name)
		require.True(t, ok)
		require.Equal(t, lsp.StateReady, info.State,
			"trial %d: a concurrent diagnostics update must never revert a state change it raced with", i)
	}
}

// TestLSPEvents_StateUpdateKeepsDiagnosticCount pins what a state update
// is allowed to answer for. Every tool call reaches Manager.Start, and a
// reusable client turns that into a synchronous state callback, so a state
// update that reported zero diagnostics republished zero on every read and
// every edit: the header's error tally emptied while the LSP block below
// it, which reads the client's live counters instead, still showed the
// real number.
func TestLSPEvents_StateUpdateKeepsDiagnosticCount(t *testing.T) {
	t.Parallel()

	le := newLSPEvents()
	t.Cleanup(le.broker.Shutdown)
	const name = "gopls"

	le.updateLSPState(name, lsp.StateReady, nil, nil)
	le.updateLSPDiagnostics(name, 7)

	// A tool call re-entering Start with an already-usable client.
	le.updateLSPState(name, lsp.StateReady, nil, nil)

	info, ok := le.states.Get(name)
	require.True(t, ok)
	require.Equal(t, 7, info.DiagnosticCount, "a state update must not answer for diagnostics")
}

// TestLSPEvents_TerminalStateClearsDiagnosticCount is the other half: a
// server that stopped, failed or never started has no diagnostics to
// report, and holding the last count would leave a dead server showing
// errors it can no longer prove.
func TestLSPEvents_TerminalStateClearsDiagnosticCount(t *testing.T) {
	t.Parallel()

	for _, state := range []lsp.ServerState{lsp.StateStopped, lsp.StateUnstarted, lsp.StateError} {
		t.Run(fmt.Sprintf("%v", state), func(t *testing.T) {
			t.Parallel()
			le := newLSPEvents()
			t.Cleanup(le.broker.Shutdown)
			const name = "gopls"

			le.updateLSPState(name, lsp.StateReady, nil, nil)
			le.updateLSPDiagnostics(name, 7)
			le.updateLSPState(name, state, nil, nil)

			info, ok := le.states.Get(name)
			require.True(t, ok)
			require.Zero(t, info.DiagnosticCount)
		})
	}
}
