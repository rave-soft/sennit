package agent

import (
	"testing"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// agentModelCoordinator builds a hermetic coordinator whose only provider
// ("mock") lists the app's main model plus a second model no config slot
// points at, so a user agent can name it explicitly. Nothing is dialed:
// the base URL points at a loopback port nobody listens on and fantasy
// builds language models lazily.
func agentModelCoordinator(t *testing.T) (*coordinator, *prompt.Prompt) {
	t.Helper()
	env := testEnv(t)

	sennitJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [
      {"id": "main-model", "name": "Main", "context_window": 8192, "default_max_tokens": 128},
      {"id": "agent-model", "name": "Agent", "context_window": 8192, "default_max_tokens": 128}
    ]}},
  "model": {"provider": "mock", "model": "main-model"}
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

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	return coord, p
}

// TestCoordinatorAgentModelSelection pins down which model a user agent ends
// up running on, stated purely in terms of "the agent's model" so it keeps
// holding once the large/small pair collapses into a single model. There are
// only two rules: a non-empty Agent.Model names the agent's model outright,
// and an empty one means "whatever the app's main model is".
func TestCoordinatorAgentModelSelection(t *testing.T) {
	t.Run("non-empty agent model is the agent's model", func(t *testing.T) {
		coord, p := agentModelCoordinator(t)
		agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
		agentCfg.Model = "mock/agent-model"

		// The resolution itself, independent of how buildAgent wires the
		// result into the session agent.
		model, err := coord.buildCustomAgentModel(t.Context(), agentCfg, false)
		require.NoError(t, err)
		require.Equal(t, config.SelectedModel{Provider: "mock", Model: "agent-model"}, model.ModelCfg)
		require.Equal(t, "agent-model", model.CatalogCfg.ID)

		// And the model the built agent actually runs on, which must be
		// that same model and not the app's main model.
		result, err := coord.buildAgent(t.Context(), p, agentCfg, false)
		require.NoError(t, err)
		require.Equal(t, "mock", result.Model().ModelCfg.Provider)
		require.Equal(t, "agent-model", result.Model().ModelCfg.Model)
		require.NotEqual(t, coord.cfg.Config().Model.Model, result.Model().ModelCfg.Model)
	})

	t.Run("empty agent model inherits the app's main model", func(t *testing.T) {
		coord, p := agentModelCoordinator(t)
		agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
		agentCfg.Model = ""

		main := coord.cfg.Config().Model
		require.Equal(t, "main-model", main.Model)

		result, err := coord.buildAgent(t.Context(), p, agentCfg, false)
		require.NoError(t, err)
		require.Equal(t, main.Provider, result.Model().ModelCfg.Provider)
		require.Equal(t, main.Model, result.Model().ModelCfg.Model)
	})
}
