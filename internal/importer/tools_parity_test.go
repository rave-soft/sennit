package importer

import (
	"slices"
	"testing"

	"github.com/rave-soft/sennit/internal/toolmeta"
	"github.com/stretchr/testify/require"
)

func TestImportKnownToolsMatchesCanonicalRegistry(t *testing.T) {
	t.Parallel()

	known := importKnownTools()
	for _, name := range toolmeta.NamesAll() {
		require.Truef(t, known[name], "canonical tool %q must be accepted on import", name)
	}
	for name := range known {
		require.Truef(t, slices.Contains(toolmeta.NamesAll(), name), "import-only tool %q must be a foreign alias, not a duplicate built-in list", name)
	}
}

// TestTranslateAgentTools_FoldsLegacyToolNameInsteadOfDropping moved here
// with translateAgentTools itself: a foreign tool name Sennit has an alias
// for is folded onto the current name rather than reported as unknown, and
// only a name nothing maps to is dropped.
func TestTranslateAgentTools_FoldsLegacyToolNameInsteadOfDropping(t *testing.T) {
	t.Parallel()

	mapped, dropped := translateAgentTools([]string{"view", "not_a_real_tool"})
	require.Equal(t, []string{"read"}, mapped)
	require.Equal(t, []string{"not_a_real_tool"}, dropped)
}
