package agent

import (
	"testing"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// buildCustomModelCoordinator writes a hermetic config with one provider
// ("mock") offering the selected model plus a second model
// ("custom-model") that no slot points at, and returns a coordinator and the
// coder agent's prompt built against it. It follows the same
// dial-a-loopback-address-that-nothing-listens-on approach as
// TestBuildAgentReadinessSurvivesCallerCancellation, since buildAgent never
// needs to make a network call: LanguageModel construction is lazy.
func buildCustomModelCoordinator(t *testing.T) (*coordinator, *prompt.Prompt) {
	t.Helper()
	env := testEnv(t)

	sennitJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [
      {"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128},
      {"id": "custom-model", "name": "Custom", "context_window": 8192, "default_max_tokens": 128}
    ]}},
  "model": {"provider": "mock", "model": "mock-model"}
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

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	return coord, p
}

// TestBuildAgentCustomModel covers the ways config.Agent.Model can route
// buildAgent: empty (inherits the app's main model) and a "provider/model-id"
// string naming a model of its own. "large"/"small" are no longer special
// values for this field, so a bare "small" is exercised as just another
// unresolvable string.
func TestBuildAgentCustomModel(t *testing.T) {
	t.Run("empty model inherits the main model", func(t *testing.T) {
		coord, p := buildCustomModelCoordinator(t)
		agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
		agentCfg.Model = ""

		result, err := coord.delegation.buildAgent(t.Context(), p, agentCfg, false)
		require.NoError(t, err)
		require.Equal(t, "mock", result.Model().ModelCfg.Provider)
		require.Equal(t, "mock-model", result.Model().ModelCfg.Model)
	})

	t.Run("unresolvable model value returns an error", func(t *testing.T) {
		coord, p := buildCustomModelCoordinator(t)
		agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
		// "small" carries no special meaning for Agent.Model anymore; with no
		// provider named "small", buildAgent hands it straight to
		// buildCustomAgentModel like any other unresolvable string, which
		// fails. Falling back to the default happens earlier, in
		// Config.validUserAgents, not here.
		agentCfg.Model = "small"

		_, err := coord.delegation.buildAgent(t.Context(), p, agentCfg, false)
		require.Error(t, err)
	})

	t.Run("custom provider/model string", func(t *testing.T) {
		coord, p := buildCustomModelCoordinator(t)
		agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
		agentCfg.Model = "mock/custom-model"

		result, err := coord.delegation.buildAgent(t.Context(), p, agentCfg, false)
		require.NoError(t, err)
		require.Equal(t, "mock", result.Model().ModelCfg.Provider)
		require.Equal(t, "custom-model", result.Model().ModelCfg.Model)
	})

	t.Run("custom model reasoning effort override applies", func(t *testing.T) {
		coord, p := buildCustomModelCoordinator(t)
		agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
		agentCfg.Model = "mock/custom-model"
		agentCfg.ReasoningEffort = "high"

		result, err := coord.delegation.buildAgent(t.Context(), p, agentCfg, false)
		require.NoError(t, err)
		require.Equal(t, "high", result.Model().ModelCfg.ReasoningEffort)
	})

	t.Run("custom model naming an unconfigured provider fails safe", func(t *testing.T) {
		coord, p := buildCustomModelCoordinator(t)
		agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
		// Config-load validation would normally reject this and fall back to
		// the default (see Config.validUserAgents); this simulates the config
		// having drifted (e.g. a provider removed on reload) after the agent
		// was set up, and checks that buildAgent fails instead of panicking
		// or silently using the wrong model.
		agentCfg.Model = "unknown-provider/some-model"

		_, err := coord.delegation.buildAgent(t.Context(), p, agentCfg, false)
		require.Error(t, err)
	})
}
