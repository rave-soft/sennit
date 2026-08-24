package config

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
