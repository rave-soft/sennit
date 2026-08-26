package agent

import (
	"maps"
	"slices"

	"github.com/rave-soft/sennit/internal/config"
)

type CustomAgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
}

// sameAgentDefinition decides whether a delegation rebuilds its delegate at
// call time (see buildCustomAgentTool): every field that shapes the
// delegate must count, and the one that doesn't (Description, which only
// feeds the tool's own description) must not, or every reload would force
// a rebuild.
func sameAgentDefinition(a, b config.Agent) bool {
	return a.Model == b.Model &&
		a.ReasoningEffort == b.ReasoningEffort &&
		a.Prompt == b.Prompt &&
		slices.Equal(a.AllowedTools, b.AllowedTools) &&
		slices.Equal(a.ContextPaths, b.ContextPaths) &&
		maps.EqualFunc(a.AllowedMCP, b.AllowedMCP, slices.Equal)
}
