package lsp

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

func TestNormalizeWorkspaceSymbolResultsSupportsLegacyAndModern(t *testing.T) {
	tests := []struct {
		wire string
		want any
	}{
		{`[{"name":"Legacy","kind":12,"location":{"uri":"file:///workspace/legacy.go","range":{"start":{"line":2,"character":3},"end":{"line":2,"character":9}}}}]`, []protocol.SymbolInformation{}},
		{`[{"name":"Modern","kind":12,"location":{"uri":"file:///workspace/modern.go"}}]`, []protocol.WorkspaceSymbol{}},
	}
	for _, test := range tests {
		var raw protocol.Or_Result_workspace_symbol
		require.NoError(t, json.Unmarshal([]byte(test.wire), &raw))
		require.IsType(t, test.want, raw.Value)
		got, err := normalizeWorkspaceSymbolResults(raw)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NotEmpty(t, got[0].Path)
	}
}
