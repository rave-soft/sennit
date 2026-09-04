package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

// TestBuiltinDelegatePrompt_CarriesTheReportingContract pins the fix for
// the built-in `agent` tool's own stateless delegate (params.SubagentType
// == "") never learning the same "your final message is the whole report"
// contract a named .sennit/agents delegate gets via delegatedAgentPrompt.
// Before the fix, runBackgroundAgent built this delegate straight from
// taskPrompt() (task.md.tpl alone), which told it the opposite: "avoid
// text before/after your response". A caller reading agentToolDescription
// (delegationReportContract) was promised a report that arrives by
// itself; the delegate had no way to know that.
func TestBuiltinDelegatePrompt_CarriesTheReportingContract(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p, err := builtinDelegatePrompt(prompt.WithWorkingDir(dir))
	require.NoError(t, err)

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{},
	}
	store := configtest.NewStore(t, cfg, configtest.WithWorkingDir(dir))
	built, err := p.Build(context.Background(), "anthropic", "claude", store)
	require.NoError(t, err)

	require.Contains(t, built, "your final message is the entire report",
		"the built-in delegate must carry the same reporting contract as a named agent")
	require.False(t, strings.Contains(built, "One word answers are best"),
		"task.md.tpl must not tell the delegate to suppress the report the contract just promised")
}
