package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDoctorCmd_CleanConfig(t *testing.T) {
	seed := `{"providers": {"openai": {"api_key": "key", "models": [{"id": "gpt-4o-mini"}]}}}`
	setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	err := doctorCmd.RunE(testCmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "No config problems found.")
}

// TestDoctorCmd_ReportsIgnoredJSONAgentsBlock covers the JSON "agents" block
// removal: subagents are defined exclusively in .sennit/agents/*.md now, so
// a JSON agents block (wherever it's seeded from) must never be read, and
// `sennit doctor` must say so instead of silently dropping it.
func TestDoctorCmd_ReportsIgnoredJSONAgentsBlock(t *testing.T) {
	seed := `{"providers": {"openai": {"api_key": "key", "models": [{"id": "gpt-4o-mini"}]}},
		"agents": {"reviewer": {"prompt": "review code", "model": "does/not-exist"}}}`
	setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	// Only warnings (an ignored agents block does not block anything), so
	// exit is still clean.
	err := doctorCmd.RunE(testCmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "[agent]")
	require.Contains(t, stdout.String(), "agents in sennit.json are ignored — define agents as .sennit/agents/*.md files")
	// The JSON entry must never surface as a registered agent either.
	require.NotContains(t, stdout.String(), "falls back to the main model")
}

// TestDoctorCmd_MarkdownAgentClean covers the still-live path: a markdown
// agent (.sennit/agents/*.md) with no model override is loaded and reported
// clean. Unlike the old JSON path, a markdown agent's unresolved model
// string never reaches the doctor's Problem list at all — parseAgentFile
// (agents_markdown.go) resolves it or silently drops it before the agent
// ever lands in Config.Agents (see the model-resolution fallback tests in
// internal/config/doctor_test.go, which exercise that check directly).
func TestDoctorCmd_MarkdownAgentClean(t *testing.T) {
	seed := `{"providers": {"openai": {"api_key": "key", "models": [{"id": "gpt-4o-mini"}]}}}`
	setupHermeticConfigEnv(t, seed)

	cwd := t.TempDir()
	agentsDir := filepath.Join(cwd, ".sennit", "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "reviewer.md"),
		[]byte("---\nname: reviewer\n---\nreview code"), 0o644))

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", cwd))

	err := doctorCmd.RunE(testCmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "No config problems found.")
}

func TestDoctorCmd_JSON(t *testing.T) {
	seed := `{"providers": {"openai": {"api_key": "key", "models": [{"id": "gpt-4o-mini"}]}},
		"agents": {"reviewer": {"prompt": "review code", "model": "does/not-exist"}}}`
	setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))
	doctorJSON = true
	defer func() { doctorJSON = false }()

	err := doctorCmd.RunE(testCmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), `"area": "agent"`)
	require.Contains(t, stdout.String(), `"severity": "warn"`)
}

func TestDoctorCmd_MainModelFallbackIsBlocking(t *testing.T) {
	seed := `{"providers": {"openai": {"api_key": "key", "models": [{"id": "gpt-4o-mini"}]}},
		"model": {"provider": "openai", "model": "does-not-exist"}}`
	setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	err := doctorCmd.RunE(testCmd, nil)
	require.Error(t, err, "an error-severity problem must fail the command")
	require.Contains(t, stdout.String(), "[model]")
	require.Contains(t, fmt.Sprint(err), "blocking")
}
