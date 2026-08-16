package tools

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNewSearchToolFallsBackToGrep(t *testing.T) {
	t.Parallel()

	// Under `go test`, getRg is disabled, so the search slot must resolve
	// to the pure-Go grep tool.
	tool := NewSearchTool(t.TempDir(), config.ToolGrep{})
	require.Equal(t, GrepToolName, tool.Info().Name)
}
