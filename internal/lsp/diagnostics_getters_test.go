package lsp

import (
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

// TestGetFileDiagnostics_ReturnsCopy pins the bug where getFileDiagnostics
// handed back the store's own slice. A caller mutating the returned slice
// (or appending to it within capacity) would silently corrupt or leak
// changes into the store's internal state, exactly the hazard getDiagnostics
// already avoids by copying.
func TestGetFileDiagnostics_ReturnsCopy(t *testing.T) {
	t.Parallel()

	d := newDiagnosticsStore("test", &clientGeneration{})
	t.Cleanup(d.requestShutdown)

	uri := protocol.DocumentURI("file:///a.go")
	d.store.Set(uri, []protocol.Diagnostic{{Message: "original"}})

	got := d.getFileDiagnostics(uri)
	require.Len(t, got, 1)
	got[0].Message = "mutated"

	again := d.getFileDiagnostics(uri)
	require.Equal(t, "original", again[0].Message,
		"mutating a returned slice must not leak into the store")
}
