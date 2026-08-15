package model

import (
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestLSPDiagnosticsUsesReadableLabels(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	got := stripANSI(lspDiagnostics(&sty, map[protocol.DiagnosticSeverity]int{
		protocol.SeverityError:   1,
		protocol.SeverityWarning: 2,
		protocol.SeverityHint:    9,
	}))

	require.Equal(t, "1 error, 2 warnings, 9 hints", got)
}

func TestLSPListShowsReadyAndCleanStatus(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	got := stripANSI(lspList(&sty, []LSPInfo{{
		LSPClientInfo: workspace.LSPClientInfo{Name: "gopls", State: lsp.StateReady},
		Diagnostics:   map[protocol.DiagnosticSeverity]int{},
	}}, 60, 1))

	require.Contains(t, got, "● gopls ready no issues")
}
