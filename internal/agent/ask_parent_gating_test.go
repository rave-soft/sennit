package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func askParentToolList() []fantasy.AgentTool {
	return []fantasy.AgentTool{
		&fakeTool{name: "agent"},
		&fakeTool{name: tools.AskParentToolName},
		&fakeTool{name: "bash"},
		&fakeTool{name: "view"},
	}
}

// A session with no registered parent cannot use ask_parent -- it can only
// fail -- so it must not be offered. A top-level session was handed it and
// used it to try to send instructions down to a thread, wasting the turn.
func TestWithoutUnusableParentTool_DropsItForAParentlessSession(t *testing.T) {
	d := newDispatcher()

	got := withoutUnusableParentTool(askParentToolList(), d, "top-level")

	require.Equal(t, []string{"agent", "bash", "view"}, agentToolNames(got))
}

// A delegation's own session has a parent to message, so it keeps the tool.
func TestWithoutUnusableParentTool_KeepsItForADelegation(t *testing.T) {
	d := newDispatcher()
	d.RegisterDelegationParent("thread-session", DelegationParent{ParentSessionID: "parent"})

	got := withoutUnusableParentTool(askParentToolList(), d, "thread-session")

	require.Equal(t, []string{"agent", tools.AskParentToolName, "bash", "view"}, agentToolNames(got))
}

// The caller's slice must not be edited underneath it: the same tool list
// is shared by every session this agent runs, and one parentless turn must
// not take ask_parent away from a delegation's turn.
func TestWithoutUnusableParentTool_DoesNotMutateTheSharedList(t *testing.T) {
	d := newDispatcher()
	shared := askParentToolList()

	_ = withoutUnusableParentTool(shared, d, "top-level")

	require.Equal(t, []string{"agent", tools.AskParentToolName, "bash", "view"}, agentToolNames(shared))
}

// The fixed provider policy stamps the cache-control breakpoint onto the
// last tool in the list (see NewSessionAgent), so removing one must not
// move the end of the list. buildTools sorts by name, which keeps
// ask_parent near the front; this pins the property the removal relies on.
func TestWithoutUnusableParentTool_LeavesTheLastToolInPlace(t *testing.T) {
	d := newDispatcher()
	in := askParentToolList()

	got := withoutUnusableParentTool(in, d, "top-level")

	require.Same(t, in[len(in)-1], got[len(got)-1])
}

// A list without the tool comes back untouched rather than reallocated.
func TestWithoutUnusableParentTool_NoopWithoutTheTool(t *testing.T) {
	d := newDispatcher()
	in := []fantasy.AgentTool{&fakeTool{name: "bash"}}

	require.Equal(t, agentToolNames(in), agentToolNames(withoutUnusableParentTool(in, d, "top-level")))
}

func agentToolNames(agentTools []fantasy.AgentTool) []string {
	names := make([]string, 0, len(agentTools))
	for _, tool := range agentTools {
		names = append(names, tool.Info().Name)
	}
	return names
}
