package agent

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// sameAgentDefinition decides whether a delegation rebuilds its delegate at
// call time (see buildCustomAgentTool): every field that shapes the
// delegate must count, and the one that doesn't (Description, which only
// feeds the tool's own description) must not, or every reload would force
// a rebuild.
func TestSameAgentDefinition(t *testing.T) {
	t.Parallel()

	base := config.Agent{
		ID: "reviewer", Name: "Reviewer", Description: "Reviews.", Prompt: "Review.",
		Model: "codex/gpt-5.6-luna", ReasoningEffort: "low",
		AllowedTools: []string{"view", "grep"}, ContextPaths: []string{"AGENTS.md"},
		AllowedMCP: map[string][]string{"jq": {"run"}},
	}
	require.True(t, sameAgentDefinition(base, base))

	same := base
	same.Description = "Edited description only."
	require.True(t, sameAgentDefinition(base, same), "a description edit must not rebuild the delegate")

	for name, mutate := range map[string]func(*config.Agent){
		"model":            func(a *config.Agent) { a.Model = "qwen36-local/Qwen3.8" },
		"reasoning effort": func(a *config.Agent) { a.ReasoningEffort = "high" },
		"prompt":           func(a *config.Agent) { a.Prompt = "Review harder." },
		"allowed tools":    func(a *config.Agent) { a.AllowedTools = []string{"view"} },
		"context paths":    func(a *config.Agent) { a.ContextPaths = nil },
		"allowed mcp":      func(a *config.Agent) { a.AllowedMCP = map[string][]string{"jq": {"run", "eval"}} },
	} {
		edited := base
		edited.AllowedTools = append([]string(nil), base.AllowedTools...)
		edited.ContextPaths = append([]string(nil), base.ContextPaths...)
		mutate(&edited)
		require.False(t, sameAgentDefinition(base, edited), "a %s edit must rebuild the delegate", name)
	}
}
