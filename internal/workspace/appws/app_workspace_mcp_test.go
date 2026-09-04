package appws

import (
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/stretchr/testify/require"
)

// TestAppWorkspace_ListMCPPrompts_NilRegistryReturnsEmpty guards
// ListMCPPrompts against a nil app.MCP. A bare &app.App{} (sanctioned for
// tests, see app.go's comment on the pattern) leaves MCP nil, and
// *mcp.Registry.Prompts has no nil-receiver guard of its own - it
// dereferences the registry's catalogMu before LoadMCPPrompts ever gets a
// chance to see the (always non-nil) iterator it returns. Without a check
// on the caller's side, w.app.MCP.Prompts() panics before LoadMCPPrompts'
// own `prompts == nil` check becomes reachable.
func TestAppWorkspace_ListMCPPrompts_NilRegistryReturnsEmpty(t *testing.T) {
	t.Parallel()

	ws := &AppWorkspace{app: &app.App{}}

	prompts, err := ws.ListMCPPrompts(t.Context())
	require.NoError(t, err)
	require.Empty(t, prompts)
}

// TestAppWorkspace_ListMCPPrompts_RealRegistryStillWorks pins the normal
// path: a real, empty registry still returns cleanly through the same
// method the nil guard above short-circuits.
func TestAppWorkspace_ListMCPPrompts_RealRegistryStillWorks(t *testing.T) {
	t.Parallel()

	a := &app.App{}
	a.MCP = mcp.NewRegistry()
	ws := &AppWorkspace{app: a}

	prompts, err := ws.ListMCPPrompts(t.Context())
	require.NoError(t, err)
	require.Empty(t, prompts)
}
