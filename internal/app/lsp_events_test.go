package app

import (
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
		le.updateLSPState(name, lsp.StateStarting, nil, nil, 0)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			le.updateLSPDiagnostics(name, 5)
		}()
		go func() {
			defer wg.Done()
			le.updateLSPState(name, lsp.StateReady, nil, nil, 0)
		}()
		wg.Wait()

		info, ok := le.GetLSPState(name)
		require.True(t, ok)
		require.Equal(t, lsp.StateReady, info.State,
			"trial %d: a concurrent diagnostics update must never revert a state change it raced with", i)
	}
}
