package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/stretchr/testify/require"
)

// doctorTestConfig builds a Config with one provider ("openai") offering a
// reasoning model ("o1") and a non-reasoning one ("gpt-4o-mini"), ready for
// SetupAgents.
func doctorTestConfig(t *testing.T) *Config {
	t.Helper()
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("openai", ProviderConfig{
		ID: "openai",
		Models: []catwalk.Model{
			{ID: "o1", CanReason: true},
			{ID: "gpt-4o-mini"},
		},
	})
	return &Config{
		Options:   &Options{},
		Providers: providers,
		Model:     SelectedModel{Provider: "openai", Model: "gpt-4o-mini"},
	}
}

func TestDoctorNilConfig(t *testing.T) {
	require.Empty(t, Doctor(nil))
}

func TestDoctorCleanConfig(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.SetupAgents()

	require.Empty(t, Doctor(cfg))
}

// TestDoctorAgentUnresolvedModel covers "agent with model not resolving to
// a provider/model" — the motivating case from the feature request: a
// sub-agent pinned to a model that doesn't exist should surface as a
// Problem, not just a log line.
func TestDoctorAgentUnresolvedModel(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "x/y"},
	}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.NotEmpty(t, problems)
	found := false
	for _, p := range problems {
		if p.Area == AreaAgent && p.Subject == "reviewer" {
			found = true
			require.Equal(t, SeverityWarn, p.Severity)
			require.Contains(t, p.Message, "reviewer")
			require.Contains(t, p.Message, "x/y")
			require.Contains(t, p.Message, "falls back to the main model")
		}
	}
	require.True(t, found, "expected a problem for the reviewer agent's unresolved model")

	// The reviewer agent itself must still work (fallback, not rejection).
	reviewer, ok := cfg.Agents["reviewer"]
	require.True(t, ok)
	require.Empty(t, reviewer.Model)
}

// TestDoctorAgentUnresolvedModel_ClearedOnFix verifies SetupAgents does not
// leave a stale Problem behind once the offending definition is fixed and
// SetupAgents runs again on the same live Config.
func TestDoctorAgentUnresolvedModel_ClearedOnFix(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "x/y"},
	}
	cfg.SetupAgents()
	require.NotEmpty(t, Doctor(cfg))

	cfg.Agents["reviewer"] = Agent{Prompt: "You review code.", Model: "openai/o1"}
	cfg.SetupAgents()
	require.Empty(t, Doctor(cfg))
}

func TestDoctorAgentReasoningEffortUnsupported(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "openai/gpt-4o-mini", ReasoningEffort: "high"},
	}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaAgent, problems[0].Area)
	require.Equal(t, SeverityWarn, problems[0].Severity)
	require.Contains(t, problems[0].Message, "does not support reasoning")
}

func TestDoctorAgentReasoningEffortSupported(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "openai/o1", ReasoningEffort: "high"},
	}
	cfg.SetupAgents()

	require.Empty(t, Doctor(cfg))
}

func TestDoctorAgentReasoningEffortInheritsMainModel(t *testing.T) {
	// No agent.Model set: the agent inherits cfg.Model, which is pinned to
	// the non-reasoning gpt-4o-mini in doctorTestConfig.
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", ReasoningEffort: "low"},
	}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Contains(t, problems[0].Message, "does not support reasoning")
}

func TestDoctorDisabledToolsUnknownName(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Options.DisabledTools = []string{"bash", "totally_not_a_tool"}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaAgent, problems[0].Area)
	require.Contains(t, problems[0].Message, "totally_not_a_tool")
}

func TestDoctorAllowedToolsUnknownName(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", AllowedTools: []string{"view", "not_a_real_tool"}},
	}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, "reviewer", problems[0].Subject)
	require.Contains(t, problems[0].Message, "not_a_real_tool")
}

// TestDoctorAllowedToolsAgentPseudoToolKnown verifies that a coder-delegate
// agent id (a legitimate pseudo-tool name) is not flagged.
func TestDoctorAllowedToolsAgentPseudoToolKnown(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code."},
		"planner":  {Prompt: "You plan.", AllowedTools: []string{"view", "reviewer"}},
	}
	cfg.SetupAgents()

	require.Empty(t, Doctor(cfg))
}

// TestDoctorAllowedToolsMCPPrefixSkipped verifies mcp_ prefixed tool names
// are not validated statically (MCP tool names are only known once the
// registry has connected).
func TestDoctorAllowedToolsMCPPrefixSkipped(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", AllowedTools: []string{"mcp_context7_query-docs"}},
	}
	cfg.SetupAgents()

	require.Empty(t, Doctor(cfg))
}

// TestDoctorPermissionsBypassEnabled verifies a persistent permissions.bypass
// surfaces as a warning, since it silently disables every permission prompt
// for the life of the process.
func TestDoctorPermissionsBypassEnabled(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Permissions = &Permissions{Bypass: true}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaPermission, problems[0].Area)
	require.Equal(t, SeverityWarn, problems[0].Severity)
	require.Equal(t, "permissions.bypass", problems[0].Subject)
	require.Contains(t, problems[0].Message, "never asks for permission")
}

// TestDoctorPermissionsBypassDisabled covers both the explicit-false and
// unset (nil Permissions) cases: neither should produce a Problem.
func TestDoctorPermissionsBypassDisabled(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.SetupAgents()
	require.Empty(t, Doctor(cfg))

	cfg.Permissions = &Permissions{Bypass: false}
	require.Empty(t, Doctor(cfg))
}

// TestDoctorProviderMissingAPIKey covers a custom provider that survives
// config load (local providers legitimately have no key) but is still
// worth flagging.
func TestDoctorProviderMissingAPIKey(t *testing.T) {
	cfg := &Config{Options: &Options{}, Problems: []Problem{
		{Severity: SeverityWarn, Area: AreaProvider, Subject: "local", Message: "provider local has no api_key"},
	}}
	cfg.SetupAgents() // must not drop provider-area problems.

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaProvider, problems[0].Area)
	require.Equal(t, "local", problems[0].Subject)
}

// TestDoctorMainModelFallback exercises resolveSelectedModel's fallback
// path end to end: an unresolvable configured main model must both fall
// back (existing behavior) and be recorded as a Problem (new behavior).
func TestDoctorMainModelFallback(t *testing.T) {
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("openai", ProviderConfig{
		ID:     "openai",
		Models: []catwalk.Model{{ID: "gpt-4o-mini", DefaultMaxTokens: 4096}},
	})
	cfg := &Config{
		Options:   &Options{},
		Providers: providers,
		Model:     SelectedModel{Provider: "openai", Model: "does-not-exist"},
	}

	knownProviders := []catwalk.Provider{{
		ID:                  catwalk.InferenceProviderOpenAI,
		DefaultLargeModelID: "gpt-4o-mini",
		Models:              []catwalk.Model{{ID: "gpt-4o-mini", DefaultMaxTokens: 4096}},
	}}
	resolved, err := resolveSelectedModel(cfg, knownProviders)
	require.NoError(t, err)
	require.True(t, resolved.Fallback)
	cfg.Model = resolved.Model

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaModel, problems[0].Area)
	require.Equal(t, SeverityError, problems[0].Severity)
	require.Contains(t, problems[0].Message, "does-not-exist")
}
