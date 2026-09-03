package stats_test

import (
	"testing"

	"github.com/rave-soft/sennit/internal/stats"
	"github.com/stretchr/testify/require"
)

// A delegation counts as landed only when it reached a state that means
// the work is in: completed or merged. Anything else — a failure, a
// cancellation, or a status this build has never heard of — must not be
// scored as a success, since the whole point of the number is to say how
// much of the delegated work actually arrived.
func TestOutcome_OnlyCompletedAndMergedCountAsLanded(t *testing.T) {
	t.Parallel()

	del := func(status string) stats.Delegation {
		return stats.Delegation{Status: status, AgentID: "developer"}
	}
	got := stats.ComputeOutcome([]stats.Delegation{
		del("completed"), del("merged"), del("failed"),
		del("cancelled"), del("running"), del("some-future-status"),
	})

	require.Equal(t, int64(6), got.Total)
	require.Equal(t, int64(2), got.Landed)
	require.Equal(t, int64(2), got.Failed)
	// Rate is over Total, not over landed+failed: a run still going has
	// not landed, and diluting the rate is the honest reading.
	require.InDelta(t, 2.0/6.0, got.Rate(), 0.0001)
}

// ComputeTotals sums cost and tokens across the whole tree — each session
// records only its own spend, so a delegation's cost is not folded into
// its parent's and must be added in separately. Sessions and TimeSeconds,
// though, stay top-level only: a delegation is not a session a person
// started, and its lifetime runs inside its parent's.
func TestComputeTotals_SumsCostAcrossTreeButSessionsAndTimeStayTopLevel(t *testing.T) {
	t.Parallel()

	sessions := []stats.Session{
		{ID: "root", PromptTokens: 1000, CompletionTokens: 500, Cost: 1.5, CreatedAt: 0, UpdatedAt: 100},
		{ID: "kid", ParentID: "root", AgentID: "reviewer", PromptTokens: 300, CompletionTokens: 100, Cost: 0.4, CreatedAt: 10, UpdatedAt: 40},
	}

	got := stats.ComputeTotals(sessions)

	require.Equal(t, int64(1), got.Sessions, "the sub-agent run is not a session a person started")
	require.Equal(t, int64(100), got.TimeSeconds, "the child's lifetime runs inside the parent's, so only the parent's span counts")
	require.Equal(t, int64(1300), got.PromptTokens)
	require.Equal(t, int64(600), got.CompletionTokens)
	require.InDelta(t, 1.9, got.Cost, 0.0001)
}

// The agent column groups by agent_id, and falls back to the session
// title only when there is no agent id — the case for sessions recorded
// before that column existed and for generic task-tool delegations.
func TestComputeAgents_PrefersAgentIDOverTitle(t *testing.T) {
	t.Parallel()

	sessions := []stats.Session{
		{ID: "root", Title: "root session"},
		{ID: "a1", ParentID: "root", AgentID: "reviewer", Title: "Review iteration 1", PromptTokens: 100, CompletionTokens: 10, Cost: 0.5, CreatedAt: 10, UpdatedAt: 40},
		{ID: "a2", ParentID: "root", AgentID: "reviewer", Title: "Review iteration 2", PromptTokens: 200, CompletionTokens: 20, Cost: 0.5, CreatedAt: 40, UpdatedAt: 60},
		{ID: "a3", ParentID: "root", Title: "New Agent Session", PromptTokens: 50, CompletionTokens: 5, CreatedAt: 10, UpdatedAt: 20},
	}

	agents := stats.ComputeAgents(sessions, nil)
	require.Len(t, agents, 2, "two differently-titled reviewer runs are one agent, not two")

	byName := map[string]stats.Agent{}
	for _, a := range agents {
		byName[a.Name] = a
	}

	reviewer := byName["reviewer"]
	require.Equal(t, int64(2), reviewer.Runs)
	require.Equal(t, int64(300), reviewer.PromptTokens)
	require.Equal(t, int64(50), reviewer.TimeSeconds)
	require.InDelta(t, 1.0, reviewer.Cost, 0.0001)

	// No agent id: the title is all there is to group by.
	_, ok := byName["New Agent Session"]
	require.True(t, ok)
}

// An agent's success column comes from the delegations its sessions ran.
func TestComputeAgents_AttributesDelegationOutcomes(t *testing.T) {
	t.Parallel()

	sessions := []stats.Session{
		{ID: "root", Title: "root"},
		{ID: "d1", ParentID: "root", AgentID: "developer"},
		{ID: "d2", ParentID: "root", AgentID: "developer"},
	}
	delegations := []stats.Delegation{
		{ID: "t1", SessionID: "d1", Status: "completed"},
		{ID: "t2", SessionID: "d2", Status: "failed"},
		// A delegation whose session is not in this scope still has its
		// own recorded agent to fall back on.
		{ID: "t3", SessionID: "gone", AgentID: "architect", Status: "merged"},
	}

	byName := map[string]stats.Agent{}
	for _, a := range stats.ComputeAgents(sessions, delegations) {
		byName[a.Name] = a
	}

	require.Equal(t, int64(2), byName["developer"].Delegations)
	require.Equal(t, int64(1), byName["developer"].Succeeded)
	require.Equal(t, int64(1), byName["architect"].Delegations)
	require.Equal(t, int64(1), byName["architect"].Succeeded)
}

// A delegation is attributed to the model that answered most of its
// session, whole — unlike tokens, an outcome cannot be split, since half
// a landed task is not a thing.
func TestComputeModels_AttributesDelegationsToDominantModel(t *testing.T) {
	t.Parallel()

	sessions := []stats.Session{
		{ID: "s1", PromptTokens: 300, CompletionTokens: 30},
	}
	messages := []stats.Message{
		{SessionID: "s1", Model: "opus", Provider: "anthropic"},
		{SessionID: "s1", Model: "opus", Provider: "anthropic"},
		{SessionID: "s1", Model: "haiku", Provider: "anthropic"},
	}
	delegations := []stats.Delegation{{ID: "t1", SessionID: "s1", Status: "completed"}}

	byModel := map[string]stats.Model{}
	for _, m := range stats.ComputeModels(sessions, messages, delegations) {
		byModel[m.Model] = m
	}

	require.Equal(t, int64(1), byModel["opus"].Delegations)
	require.Equal(t, int64(1), byModel["opus"].Succeeded)
	require.Zero(t, byModel["haiku"].Delegations, "the minority model must not also be credited with the task")
	// The mixed session still splits its tokens, and says so.
	require.True(t, byModel["opus"].Approximate)
	require.True(t, byModel["haiku"].Approximate)
}
