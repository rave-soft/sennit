package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfig_Load_RuntimeEnvironmentShellCommandRunsOnce guards against
// runtime.go recomputing the runtime environment (and so re-running every
// $(...) command in Env) on each RuntimeEnvironment/ResolveValue call. It
// exercises the real production path: a project sennit.json with a
// command-substitution Env entry, loaded through the normal buildConfig
// pipeline (loadRuntimeForTest -> LoadWithProcessor), which itself resolves
// many values while configuring known providers. Before the fix, that
// alone re-ran the command once per resolved value; a few explicit
// ResolveValue calls afterward would each add another run.
func TestConfig_Load_RuntimeEnvironmentShellCommandRunsOnce(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	counterFile := filepath.Join(t.TempDir(), "counter")
	workingDir := t.TempDir()
	projectSeed := fmt.Sprintf(`{"env": {"SIDE_EFFECT": "$(echo hit >> %s)"}}`, counterFile)
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "sennit.json"), []byte(projectSeed), 0o644))
	require.NoError(t, Trust(workingDir))

	store, err := loadRuntimeForTest(workingDir, "", false)
	require.NoError(t, err)

	// Resolve several more, unrelated values through the same config's
	// runtime resolver: none of them should re-run the Env command.
	resolver := store.Config().RuntimeResolver()
	for i := range 3 {
		_, err := resolver.ResolveValue(fmt.Sprintf("value-%d", i))
		require.NoError(t, err)
	}

	data, err := os.ReadFile(counterFile)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "hit"),
		"the $(...) command in an Env entry must run exactly once, when the config is built, not once per resolved value")
}

// TestConfig_RuntimeEnvironment_Precedence pins the three-layer precedence
// RuntimeEnvironment must preserve exactly: process environment, overlaid
// by each Env entry (resolved in sorted key order), overlaid by
// SENNIT_-prefixed process variables.
func TestConfig_RuntimeEnvironment_Precedence(t *testing.T) {
	t.Setenv("SENNIT_RUNTIME_ENV_TEST", "process-value")
	t.Setenv("SENNIT_SENNIT_RUNTIME_ENV_TEST", "prefixed-value")

	cfg := &Config{Env: map[string]string{
		"SENNIT_RUNTIME_ENV_TEST": "config-value",
	}}
	cfg.populateRuntimeEnvironment()

	got := cfg.RuntimeEnvironment()
	require.Equal(t, "prefixed-value", got.Get("SENNIT_RUNTIME_ENV_TEST"),
		"a SENNIT_-prefixed process variable must win over both the process variable and the Env entry it shadows")
}
