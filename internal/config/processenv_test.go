package config

import (
	"os"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/stretchr/testify/require"
)

func TestRuntimeEnvironment_ResolvesConfigEnvInLexicographicOrder(t *testing.T) {
	t.Setenv("RUNTIME_ENVIRONMENT_BACKWARD_SOURCE", "process-backward")
	t.Setenv("RUNTIME_ENVIRONMENT_FORWARD_SOURCE", "process-forward")
	t.Setenv("RUNTIME_ENVIRONMENT_SELF", "process-self")

	environment := (&Config{Env: map[string]string{
		"A_RUNTIME_ENVIRONMENT_BACKWARD_SOURCE":  "config-backward",
		"B_RUNTIME_ENVIRONMENT_BACKWARD_DERIVED": "$A_RUNTIME_ENVIRONMENT_BACKWARD_SOURCE",
		"C_RUNTIME_ENVIRONMENT_FORWARD_DERIVED":  "$RUNTIME_ENVIRONMENT_FORWARD_SOURCE",
		"RUNTIME_ENVIRONMENT_FORWARD_SOURCE":     "config-forward",
		"RUNTIME_ENVIRONMENT_SELF":               "$RUNTIME_ENVIRONMENT_SELF",
	}}).RuntimeEnvironment()

	require.Equal(t, "config-backward", environment.Get("B_RUNTIME_ENVIRONMENT_BACKWARD_DERIVED"))
	require.Equal(t, "process-forward", environment.Get("C_RUNTIME_ENVIRONMENT_FORWARD_DERIVED"))
	require.Equal(t, "process-self", environment.Get("RUNTIME_ENVIRONMENT_SELF"))
	require.Equal(t, "process-backward", os.Getenv("RUNTIME_ENVIRONMENT_BACKWARD_SOURCE"))
	require.Equal(t, "process-forward", os.Getenv("RUNTIME_ENVIRONMENT_FORWARD_SOURCE"))
	require.Equal(t, "process-self", os.Getenv("RUNTIME_ENVIRONMENT_SELF"))
}

func TestRuntimeEnvironment_AppliesBareOverridesAfterConfigEnv(t *testing.T) {
	t.Setenv("RUNTIME_ENVIRONMENT_OVERRIDE", "process")
	t.Setenv(brand.EnvPrefix+"RUNTIME_ENVIRONMENT_OVERRIDE", "bootstrap")

	environment := (&Config{Env: map[string]string{
		"RUNTIME_ENVIRONMENT_OVERRIDE": "config",
	}}).RuntimeEnvironment()

	require.Equal(t, "bootstrap", environment.Get("RUNTIME_ENVIRONMENT_OVERRIDE"))
	require.Equal(t, "process", os.Getenv("RUNTIME_ENVIRONMENT_OVERRIDE"))
}

func TestRuntimeEnvironment_ConcurrentWorkspacesDoNotMutateProcessEnvironment(t *testing.T) {
	t.Setenv("RUNTIME_ENVIRONMENT_CONCURRENT", "process")

	const workspaces = 8
	results := make(chan string, workspaces)
	var wg sync.WaitGroup
	for i := range workspaces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value := string(rune('a' + i))
			environment := (&Config{Env: map[string]string{
				"RUNTIME_ENVIRONMENT_CONCURRENT": value,
			}}).RuntimeEnvironment()
			results <- environment.Get("RUNTIME_ENVIRONMENT_CONCURRENT")
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]bool)
	for value := range results {
		seen[value] = true
	}
	require.Len(t, seen, workspaces)
	require.Equal(t, "process", os.Getenv("RUNTIME_ENVIRONMENT_CONCURRENT"))
}
