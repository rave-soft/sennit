package config

import (
	"encoding/json"

	"testing"

	"github.com/stretchr/testify/require"
)

// newAgentConfig returns a Config with just enough set up for SetupAgents,
// which reads Options for the disabled-tool and context-path defaults.
func newAgentConfig(t *testing.T, agentsJSON string) *Config {
	t.Helper()
	cfg := &Config{Options: &Options{}}
	if agentsJSON != "" {
		require.NoError(t, json.Unmarshal([]byte(agentsJSON), cfg))
	}
	return cfg
}

func TestSetupAgentsKeepsBuiltinsWithoutUserAgents(t *testing.T) {
	cfg := newAgentConfig(t, "")
	cfg.SetupAgents()

	require.Len(t, cfg.Agents, 2)
	require.Contains(t, cfg.Agents, AgentCoder)
	require.Contains(t, cfg.Agents, AgentTask)
}

func TestSetupAgentsRegistersUserAgent(t *testing.T) {
	cfg := newAgentConfig(t, `{"agents":{"reviewer":{
		"description":"Reviews code",
		"prompt":"You review code."
	}}}`)
	cfg.SetupAgents()

	reviewer, ok := cfg.Agents["reviewer"]
	require.True(t, ok, "user agent should survive SetupAgents")
	require.Equal(t, "reviewer", reviewer.ID)
	require.Equal(t, "reviewer", reviewer.Name, "name should default to the id")
	require.Equal(t, SelectedModelTypeLarge, reviewer.Model, "model should default to large")
	require.NotEmpty(t, reviewer.AllowedTools, "nil allowed_tools means all tools")

	// The coder delegates through a tool named after the agent, so that name
	// has to be in its allow-list or buildTools filters the tool away.
	require.Contains(t, cfg.Agents[AgentCoder].AllowedTools, "reviewer")
}

func TestSetupAgentsRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name  string
		agent string
	}{
		{"no prompt", `"noprompt":{"description":"x"}`},
		{"blank prompt", `"blank":{"prompt":"   "}`},
		{"id collides with builtin tool", `"grep":{"prompt":"x"}`},
		{"id has invalid characters", `"my agent":{"prompt":"x"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newAgentConfig(t, `{"agents":{`+tt.agent+`}}`)
			cfg.SetupAgents()

			require.Len(t, cfg.Agents, 2, "only the built-ins should remain")
			for id := range cfg.Agents {
				require.Contains(t, []string{AgentCoder, AgentTask}, id)
			}
		})
	}
}

func TestSetupAgentsSkipsDisabledAgent(t *testing.T) {
	cfg := newAgentConfig(t, `{"agents":{"reviewer":{
		"prompt":"You review code.",
		"disabled":true
	}}}`)
	cfg.SetupAgents()

	require.NotContains(t, cfg.Agents, "reviewer")
	require.NotContains(t, cfg.Agents[AgentCoder].AllowedTools, "reviewer")
}

func TestSetupAgentsHonoursExplicitEmptyToolList(t *testing.T) {
	cfg := newAgentConfig(t, `{"agents":{"planner":{
		"prompt":"You plan.",
		"allowed_tools":[]
	}}}`)
	cfg.SetupAgents()

	planner := cfg.Agents["planner"]
	require.NotNil(t, planner.AllowedTools)
	require.Empty(t, planner.AllowedTools, "an explicit empty list is a deliberate choice")
}

// SetupAgents runs again on config reload and workspace switches, so it must
// not treat the built-ins it wrote last time as user definitions, nor keep
// appending duplicate tool names to the coder.
func TestSetupAgentsIsIdempotent(t *testing.T) {
	cfg := newAgentConfig(t, `{"agents":{"reviewer":{"prompt":"You review code."}}}`)

	cfg.SetupAgents()
	first := cfg.Agents

	cfg.SetupAgents()
	second := cfg.Agents

	require.Equal(t, len(first), len(second))
	require.Contains(t, second, "reviewer")

	coderTools := second[AgentCoder].AllowedTools
	count := 0
	for _, name := range coderTools {
		if name == "reviewer" {
			count++
		}
	}
	require.Equal(t, 1, count, "repeated setup must not duplicate the delegation tool")
	require.Equal(t, first[AgentCoder].AllowedTools, coderTools, "tool list should be stable across runs")
}

func TestSetupAgentsMultipleRolesGetSortedTools(t *testing.T) {
	cfg := newAgentConfig(t, `{"agents":{
		"reviewer":{"prompt":"review"},
		"dba":{"prompt":"dba"},
		"security":{"prompt":"sec"}
	}}`)
	cfg.SetupAgents()

	for _, id := range []string{"reviewer", "dba", "security"} {
		require.Contains(t, cfg.Agents, id)
		require.Contains(t, cfg.Agents[AgentCoder].AllowedTools, id)
	}
}

func TestValidAgentID(t *testing.T) {
	valid := []string{"reviewer", "reviewer-dba", "developer_junior", "a1"}
	invalid := []string{"", "my agent", "review.er", "café", "agent/one"}

	for _, id := range valid {
		require.True(t, validAgentID(id), "%q should be valid", id)
	}
	for _, id := range invalid {
		require.False(t, validAgentID(id), "%q should be invalid", id)
	}
}

func TestSetupAgentsKeepsReasoningEffort(t *testing.T) {
	cfg := newAgentConfig(t, `{"agents":{"reviewer":{
		"prompt":"You review code.",
		"reasoning_effort":"low"
	}}}`)
	cfg.SetupAgents()

	require.Equal(t, "low", cfg.Agents["reviewer"].ReasoningEffort)
}
