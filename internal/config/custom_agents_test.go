package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// newAgentConfig returns a Config with just enough set up for SetupAgents,
// which reads Options for the disabled-tool and context-path defaults.
// agentsJSON, if non-empty, is unmarshaled directly into the Config to seed
// cfg.Agents outside of the normal loadFromBytes pipeline — used only by
// tests asserting that SetupAgents does not trust a pre-existing Agents
// field, since a JSON "agents" block itself is never decoded on a real load
// (see loadFromBytes in load.go).
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

func TestSetupAgentsWithInheritedKeepsParentUserAgents(t *testing.T) {
	cfg := newAgentConfig(t, "")
	cfg.SetupAgentsWithInherited(map[string]Agent{
		"reviewer": {Prompt: "Review code."},
	})

	require.Contains(t, cfg.Agents, "reviewer")
	require.Contains(t, cfg.Agents[AgentCoder].AllowedTools, "reviewer")
	require.Equal(t, map[string]Agent{"reviewer": cfg.Agents["reviewer"]}, cfg.UserAgents())
}

func TestSetupAgentsTaskAgentHasNetworkTools(t *testing.T) {
	// The Task agent is read-only with respect to local state, but it may
	// still need to pull in outside context (docs, issue trackers, etc.),
	// so fetch/web_fetch/web_search are part of its default palette. Those
	// calls still go through the real permission.Service, same as the
	// coder's.
	cfg := newAgentConfig(t, "")
	cfg.SetupAgents()

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	require.Contains(t, taskAgent.AllowedTools, "fetch")
	require.Contains(t, taskAgent.AllowedTools, "web_fetch")
	require.Contains(t, taskAgent.AllowedTools, "web_search")
}

// An unresolvable model string must not throw the agent out entirely: it
// falls back to empty (the app's main model). Note that this only exercises
// validUserAgents' fallback path directly, via a hand-built Agent — a real
// markdown agent file with an unresolvable model never reaches this branch,
// since parseAgentFile (agents_markdown.go) already resolves or silently
// drops the model before the agent lands in c.Agents.
func TestValidUserAgentsFallsBackForUnresolvableModel(t *testing.T) {
	cfg := newAgentConfig(t, "")
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "nope/nope"},
	}

	valid, invalid := cfg.validUserAgents()
	require.Empty(t, invalid)
	require.Contains(t, valid, "reviewer")
	require.Empty(t, valid["reviewer"].Model)
}

func TestSetupAgentsRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
	}{
		{"no prompt", "noprompt.md", "---\nname: noprompt\ndescription: x\n---\n   \n"},
		{"id collides with builtin tool", "grep.md", "---\nname: grep\n---\nx"},
		{"id has invalid characters", "my-agent-bad.md", "---\nname: my agent\n---\nx"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeAgent(t, root, ".sennit/agents", tt.file, tt.content)

			cfg := newAgentConfig(t, "")
			cfg.workingDir = root
			cfg.SetupAgents()

			require.Len(t, cfg.Agents, 2, "only the built-ins should remain")
			for id := range cfg.Agents {
				require.Contains(t, []string{AgentCoder, AgentTask}, id)
			}
		})
	}
}

func TestSetupAgentsSkipsDisabledAgent(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".sennit/agents", "reviewer.md", "---\nname: reviewer\ndisabled: true\n---\nYou review code.")

	cfg := newAgentConfig(t, "")
	cfg.workingDir = root
	cfg.SetupAgents()

	require.NotContains(t, cfg.Agents, "reviewer")
	require.NotContains(t, cfg.Agents[AgentCoder].AllowedTools, "reviewer")
}

func TestSetupAgentsHonoursExplicitEmptyToolList(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".sennit/agents", "planner.md", "---\nname: planner\ntools: []\n---\nYou plan.")

	cfg := newAgentConfig(t, "")
	cfg.workingDir = root
	cfg.SetupAgents()

	planner := cfg.Agents["planner"]
	require.NotNil(t, planner.AllowedTools)
	require.Empty(t, planner.AllowedTools, "an explicit empty list is a deliberate choice")
}

// SetupAgents runs again on config reload and workspace switches, so it must
// not treat the built-ins it wrote last time as user definitions, nor keep
// appending duplicate tool names to the coder.
func TestSetupAgentsIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".sennit/agents", "reviewer.md", "---\nname: reviewer\n---\nYou review code.")

	cfg := newAgentConfig(t, "")
	cfg.workingDir = root

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
	root := t.TempDir()
	writeAgent(t, root, ".sennit/agents", "reviewer.md", "---\nname: reviewer\n---\nreview")
	writeAgent(t, root, ".sennit/agents", "dba.md", "---\nname: dba\n---\ndba")
	writeAgent(t, root, ".sennit/agents", "security.md", "---\nname: security\n---\nsec")

	cfg := newAgentConfig(t, "")
	cfg.workingDir = root
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
	root := t.TempDir()
	writeAgent(t, root, ".sennit/agents", "reviewer.md", "---\nname: reviewer\nreasoning_effort: low\n---\nYou review code.")

	cfg := newAgentConfig(t, "")
	cfg.workingDir = root
	cfg.SetupAgents()

	require.Equal(t, "low", cfg.Agents["reviewer"].ReasoningEffort)
}
