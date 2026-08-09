package chat

import (
	"testing"

	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/config"
	"github.com/stretchr/testify/require"
)

// toolsWithoutDedicatedRenderer lists the built-in tools (from
// config.AllToolNames, the actual source of truth for what tools exist)
// that intentionally have no entry in toolMessageItemFactories and fall
// back to the generic renderer. Anything not on this list must have a
// dedicated factory.
var toolsWithoutDedicatedRenderer = []string{
	tools.BraidInfoToolName,
	tools.BraidLogsToolName,
	tools.ListMCPResourcesToolName,
	tools.ReadMCPResourceToolName,
}

// TestToolMessageItemFactories_MatchExpectedNames checks
// toolMessageItemFactories against config.AllToolNames instead of a
// second, hand-maintained list of tool names living in this test file.
// Two hand-maintained lists drift silently: a new tool can be added to
// allToolNames() without anyone remembering to update a duplicate here,
// which defeats the entire point of this test — catching a tool that
// falls through to the generic renderer unnoticed.
func TestToolMessageItemFactories_MatchExpectedNames(t *testing.T) {
	t.Parallel()

	noRenderer := make(map[string]bool, len(toolsWithoutDedicatedRenderer))
	for _, name := range toolsWithoutDedicatedRenderer {
		noRenderer[name] = true
	}

	for _, name := range config.AllToolNames() {
		if noRenderer[name] {
			require.NotContainsf(t, toolMessageItemFactories, name,
				"tool %q is listed as having no dedicated renderer, but one is registered; remove it from toolsWithoutDedicatedRenderer", name)
			continue
		}
		require.Containsf(t, toolMessageItemFactories, name,
			"tool %q has no registered factory and will fall back to the generic renderer", name)
	}

	// Every registered factory must correspond to a real, known tool —
	// this catches dead renderer registrations for tools that no longer
	// exist, the same kind of debris a previously removed built-in tool
	// left behind elsewhere in the repo.
	known := make(map[string]bool, len(config.AllToolNames()))
	for _, name := range config.AllToolNames() {
		known[name] = true
	}
	for name := range toolMessageItemFactories {
		require.Truef(t, known[name], "tool %q has a registered factory but is not in config.AllToolNames()", name)
	}
}
