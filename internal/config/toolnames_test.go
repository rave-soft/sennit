package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The read tool was called "view" until it was renamed. Every test below
// covers one file a user already has on disk and will not edit: dropping
// its entry silently would either re-introduce a permission prompt the
// user had turned off, or re-enable a tool they had disabled.

func TestCanonicalToolName_FoldsLegacyAndPassesTheRestThrough(t *testing.T) {
	t.Parallel()

	require.Equal(t, "read", CanonicalToolName("view"))
	require.Equal(t, "read", CanonicalToolName("read"))
	require.Equal(t, "bash", CanonicalToolName("bash"))
	// MCP tools and user-defined agents are named at runtime, so an
	// unrecognized name is not necessarily a wrong one.
	require.Equal(t, "mcp_srv_tool", CanonicalToolName("mcp_srv_tool"))
	require.Equal(t, "reviewer", CanonicalToolName("reviewer"))
}

func TestCanonicalToolNames_DropsTheDuplicateFoldingCreates(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"bash", "read"}, canonicalToolNames([]string{"bash", "view", "read"}))
	require.Nil(t, canonicalToolNames(nil))
}

func TestSetDefaults_FoldsLegacyToolNamesInConfig(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Options:     &Options{DisabledTools: []string{"view"}},
		Permissions: &Permissions{AllowedTools: []string{"bash", "view"}},
	}
	cfg.setDefaults(t.TempDir(), t.TempDir())

	require.Equal(t, []string{"read"}, cfg.Options.DisabledTools)
	require.Equal(t, []string{"bash", "read"}, cfg.Permissions.AllowedTools)
}

// A disabled_tools entry naming the old tool must still disable the tool,
// which is only observable through the agent's resolved tool set.
func TestSetupAgents_LegacyDisabledToolStillDisables(t *testing.T) {
	t.Parallel()

	cfg := &Config{Options: &Options{DisabledTools: []string{"view"}}}
	cfg.setDefaults(t.TempDir(), t.TempDir())
	cfg.SetupAgents()

	require.NotContains(t, cfg.Agents[AgentCoder].AllowedTools, "read")
	require.NotContains(t, cfg.Agents[AgentCoder].AllowedTools, "view")
}

func TestDiscoverMarkdownAgents_FoldsLegacyToolName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, ".braid", "agents")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "reviewer.md"),
		[]byte("---\nname: reviewer\ntools: [view, grep]\n---\nYou review Go code."), 0o644))

	got := discoverMarkdownAgents(root, nil)
	require.Equal(t, []string{"read", "grep"}, got["reviewer"].AllowedTools)
}

func TestTranslateAgentTools_FoldsLegacyToolNameInsteadOfDropping(t *testing.T) {
	t.Parallel()

	mapped, dropped := translateAgentTools([]string{"view", "not_a_real_tool"})
	require.Equal(t, []string{"read"}, mapped)
	require.Equal(t, []string{"not_a_real_tool"}, dropped)
}

func TestDoctor_DoesNotWarnAboutALegacyToolName(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: map[string]Agent{
			"reviewer": {Prompt: "You review code.", AllowedTools: []string{"view"}},
		},
	}
	require.Empty(t, doctorToolNames(cfg))
}
