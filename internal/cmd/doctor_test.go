package cmd

import (
	"fmt"
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

func TestDoctorCmd_ReportsUnresolvedAgentModel(t *testing.T) {
	seed := `{"providers": {"openai": {"api_key": "key", "models": [{"id": "gpt-4o-mini"}]}},
		"agents": {"reviewer": {"prompt": "review code", "model": "does/not-exist"}}}`
	setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	// Only warnings (an unresolved agent model falls back rather than
	// blocking anything), so exit is still clean.
	err := doctorCmd.RunE(testCmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "[agent]")
	require.Contains(t, stdout.String(), "reviewer")
	require.Contains(t, stdout.String(), "falls back to the main model")
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
