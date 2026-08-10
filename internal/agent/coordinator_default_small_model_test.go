package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/braid/internal/config"
	"github.com/stretchr/testify/require"
)

// TestCoordinatorDefaultSmallModel covers defaultSmallModel, the helper
// that replaced the old cfg.Models[SelectedModelTypeSmall] read once Config
// stopped carrying a user-configurable small-model slot. It mirrors
// App.GetDefaultSmallModel: prefer the main model's provider's
// catwalk-declared default small model, and fall back to reusing the main
// model when the provider isn't in the known catalog (or the catalog names
// a small model the provider config doesn't actually list).
func TestCoordinatorDefaultSmallModel(t *testing.T) {
	t.Run("known provider prefers its catalog default small model", func(t *testing.T) {
		env := testEnv(t)
		// Minimax is a real embedded-catalog provider with a
		// default_small_model_id that resolves to one of its own listed
		// models, so it exercises the "found" branch without any network
		// I/O: defaultSmallModel only reads config, it never builds a
		// provider or dials anything.
		t.Setenv("MINIMAX_API_KEY", "test-key")

		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)

		coord := &coordinator{cfg: cfg}
		main := config.SelectedModel{Provider: "minimax", Model: "MiniMax-M3"}
		got := coord.defaultSmallModel(main)

		require.Equal(t, "minimax", got.Provider)
		require.Equal(t, "MiniMax-M2.7", got.Model)
	})

	t.Run("unknown provider falls back to the main model", func(t *testing.T) {
		env := testEnv(t)
		braidJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}}
}`
		require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "braid.json"), []byte(braidJSON), 0o644))

		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)

		coord := &coordinator{cfg: cfg}
		main := config.SelectedModel{Provider: "mock", Model: "mock-model"}
		got := coord.defaultSmallModel(main)

		require.Equal(t, main, got)
	})
}
