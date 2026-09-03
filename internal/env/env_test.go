package env

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOsEnv_Get(t *testing.T) {
	env := &osEnv{}
	t.Setenv("TEST_VAR", "test_value")

	require.Equal(t, "test_value", env.Get("TEST_VAR"))
	require.Empty(t, env.Get("NON_EXISTENT_VAR"))
}

func TestOsEnv_Env(t *testing.T) {
	envVars := (&osEnv{}).Env()
	require.NotEmpty(t, envVars)
	for _, envVar := range envVars {
		require.Contains(t, envVar, "=")
	}
}

// TestOsEnv_Env_StripsHerdrVars pins the same guarantee WithoutHerdrEnv
// gives everywhere else a subprocess env is built from the process
// environment: a value resolved through this Env (command substitution
// included) must not see HERDR_* vars, or the subprocess it runs could
// attach to the parent pane's agent authority.
func TestOsEnv_Env_StripsHerdrVars(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	t.Setenv("HERDR_PANE_ID", "wA:p1")

	for _, envVar := range (&osEnv{}).Env() {
		if len(envVar) >= 6 && envVar[:6] == "HERDR_" {
			t.Fatalf("herdr var not stripped: %s", envVar)
		}
	}
}

func TestSnapshot_MergesAndCopiesValues(t *testing.T) {
	overrides := map[string]string{"SHARED": "override", "ONLY_OVERRIDE": "value"}
	environment := Snapshot([]string{"BASE=value", "SHARED=base"}, overrides)
	overrides["SHARED"] = "changed"

	require.Equal(t, "value", environment.Get("BASE"))
	require.Equal(t, "override", environment.Get("SHARED"))
	require.Equal(t, "value", environment.Get("ONLY_OVERRIDE"))
	require.Equal(t, []string{"BASE=value", "ONLY_OVERRIDE=value", "SHARED=override"}, environment.Env())
}
