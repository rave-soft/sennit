package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestFindCatalogModelFirstMatchWins pins findCatalogModel's resolution rule
// for a provider config whose Models slice contains two entries sharing an
// ID: the first entry wins. buildAgentModel and buildCustomAgentModel both
// resolve their catalog entry through this one function (buildModel), so
// they cannot diverge on this again the way they did before — one used to
// scan without a break (last match wins) and the other with one (first
// match wins).
func TestFindCatalogModelFirstMatchWins(t *testing.T) {
	providerCfg := config.ProviderConfig{
		ID: "mock",
		Models: []catwalk.Model{
			{ID: "dup-model", Name: "First"},
			{ID: "dup-model", Name: "Second"},
		},
	}

	got := findCatalogModel(providerCfg, "dup-model")
	require.NotNil(t, got)
	require.Equal(t, "First", got.Name)
}

// TestBuildAgentModelDuplicateCatalogEntryFirstMatchWins exercises the same
// rule through the app's main-model path (buildAgentModel), which reaches
// providerCfg.Models directly and has no equivalent of
// config.ResolveModelString's ambiguity check to reject a duplicate ID
// beforehand — see this package's report on buildCustomAgentModel for why
// that path cannot reach the divergent case the same way.
func TestBuildAgentModelDuplicateCatalogEntryFirstMatchWins(t *testing.T) {
	env := testEnv(t)

	sennitJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [
      {"id": "dup-model", "name": "First", "context_window": 8192, "default_max_tokens": 128},
      {"id": "dup-model", "name": "Second", "context_window": 4096, "default_max_tokens": 64}
    ]}},
  "model": {"provider": "mock", "model": "dup-model"}
}`
	writeGlobalConfig(t, sennitJSON)

	cfg, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		background:  shell.NewBackgroundShellManager(),
	}
	coord.newCoordinatorComponents()

	model, err := coord.builder.buildAgentModel(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, "First", model.CatalogCfg.Name)
}
