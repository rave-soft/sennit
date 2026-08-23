package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestImportKnownToolsCoversEveryToolName pins the two lists together.
// importKnownTools is a hand-written copy of Sennit's tool names (config
// cannot import the tools package without a cycle), and a name missing
// from it is reported as dropped during an import and removed from the
// agent's tool list — which is what happened to agentic_fetch and
// ask_parent.
func TestImportKnownToolsCoversEveryToolName(t *testing.T) {
	t.Parallel()

	var missing []string
	for _, name := range allToolNames() {
		if !importKnownTools[name] {
			missing = append(missing, name)
		}
	}
	require.Empty(t, missing, "every tool name must be accepted on import")
}
