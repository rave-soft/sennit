package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// delegateUnevenHistory leaves ten sessions of the "developer" agent under
// one parent, of uneven sizes that together run well past
// maxCarriedSubAgentChars (about five of them fit under it), and returns
// the parent's id.
func delegateUnevenHistory(t *testing.T, env fakeEnv, coord *coordinator) string {
	t.Helper()
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	agent := newRecordingSubAgent(env, strings.Repeat("r", maxCarriedSubAgentChars/8))
	for i := range 10 {
		pad := strings.Repeat("p", (i%2)*maxCarriedSubAgentChars/10)
		delegate(t, coord, agent, parent.ID, "developer", fmt.Sprint(i), fmt.Sprintf("task %d: %s", i, pad))
	}
	return parent.ID
}

func messageIDs(msgs []message.Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

// TestLoadCarriedSessionsKeepsWhatTheBudgetKeeps pins the equivalence
// loadCarriedSessions rests on: for every budget up to its limit,
// applyCarryOverBudget picks the same messages from the loaded sessions as
// it did from all of them, while the loader reads fewer.
func TestLoadCarriedSessionsKeepsWhatTheBudgetKeeps(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)
	parentID := delegateUnevenHistory(t, env, coord)

	prior, err := env.sessions.ListSubAgentSessions(t.Context(), parentID, "developer", "")
	require.NoError(t, err)
	require.Len(t, prior, 10)

	var all [][]message.Message
	for _, s := range prior {
		msgs, err := env.messages.List(t.Context(), s.ID)
		require.NoError(t, err)
		if msgs = trimToSummary(s, msgs); len(msgs) != 0 {
			all = append(all, msgs)
		}
	}

	loaded, skipped, err := coord.delegation.loadCarriedSessions(t.Context(), prior, maxCarriedSubAgentChars)
	require.NoError(t, err)
	require.Less(t, len(loaded), len(all), "the loader must leave out sessions the budget cannot reach")
	require.Equal(t, len(all), len(loaded)+skipped)

	for _, budget := range []int{maxCarriedSubAgentChars, maxCarriedSubAgentChars - 1, maxCarriedSubAgentChars / 2, maxCarriedSubAgentChars / 7, 1, 0, -1} {
		want, _ := applyCarryOverBudget(all, budget)
		got, _ := applyCarryOverBudget(loaded, budget)
		require.Equal(t, messageIDs(want), messageIDs(got), "budget %d", budget)
	}
}

// TestSnapshotDelegationCarriesOnlyReachableHistory guards the delegation
// snapshot: it used to embed every prior session of the named agent, so
// the fiftieth delegation to one agent carried 90MB its prompt could use
// at most maxCarriedSubAgentChars of.
func TestSnapshotDelegationCarriesOnlyReachableHistory(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)
	parentID := delegateUnevenHistory(t, env, coord)

	definition := config.Agent{ID: "developer", Prompt: "Develop", AllowedTools: []string{"read"}}
	args := tools.TaskCreateArgs{AgentID: "developer", ParentSessionID: parentID, SessionID: "next$$call", Goal: "go"}
	require.NoError(t, coord.delegation.snapshotDelegation(&args, &definition, t.Context()))

	var spec DelegationExecution
	require.NoError(t, json.Unmarshal([]byte(args.Execution), &spec))
	require.NotEmpty(t, spec.History)
	require.Less(t, len(spec.History), 10)
	require.Less(t, len(args.Execution), 3*maxCarriedSubAgentChars)
	require.Contains(t, args.Execution, "task 9:", "the newest session must be carried")
	require.NotContains(t, args.Execution, "task 0:", "the oldest session must be left out")
}
