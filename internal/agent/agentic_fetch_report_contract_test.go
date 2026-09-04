package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildAgenticFetchAgent_CarriesTheReportingContract pins the fix for
// the third of three delegate-creation paths (see buildAgenticFetchAgent's
// doc comment): agenticFetchToolDescription glues delegationReportContract
// onto the caller-facing description, promising that agentic_fetch's
// answer comes back as a self-contained report, but the delegate's own
// system prompt (agentic_fetch_prompt.md.tpl) never said so - only the
// built-in `agent` tool's delegate (builtinDelegatePrompt) and named
// .sennit/agents delegates (delegatedAgentPrompt) carried that contract.
// Before the fix this assertion fails: the built prompt has the
// <response_format> section from the template but none of the "your
// final message is the entire report" language the caller was promised.
func TestBuildAgenticFetchAgent_CarriesTheReportingContract(t *testing.T) {
	coord := newAgentToolTestCoordinator(t, nil)

	agent, err := coord.delegation.buildAgenticFetchAgent(t.Context(), nil, t.TempDir())
	require.NoError(t, err)

	sa, ok := agent.(*sessionAgent)
	require.True(t, ok, "buildAgenticFetchAgent must return a *sessionAgent")

	require.Contains(t, sa.systemPrompt.Get(), "your final message is the entire report",
		"the agentic_fetch delegate must carry the same reporting contract as the built-in and named delegates")
}
