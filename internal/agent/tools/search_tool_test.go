package tools

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNewSearchToolFallsBackToGrep(t *testing.T) {
	t.Parallel()

	tool := newSearchTool(nil, t.TempDir(), config.ToolGrep{}, "")
	require.Equal(t, GrepToolName, tool.Info().Name)
}

func TestLookupRipgrep(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/tools/rg", lookupRipgrep(func(string) (string, error) { return "/tools/rg", nil }))
	require.Empty(t, lookupRipgrep(func(string) (string, error) { return "", os.ErrNotExist }))
}
