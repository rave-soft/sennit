package stats_test

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/stretchr/testify/require"
)

// stubQuerier serves canned rows, so these tests exercise the aggregation
// rather than SQLite. The query-level behavior (project scoping, the
// recursive session-tree walk, the --since cutoff) is covered against a
// real database in internal/cmd's stat tests.
type stubQuerier struct {
	tree        []db.ListSessionTreeSinceRow
	treeMsgs    []db.ListSessionTreeAssistantMessagesRow
	projectRows []db.ListSessionsSinceWithAgentRow
	projectMsgs []db.ListAssistantMessagesSinceRow
	allRows     []db.ListAllSessionsSinceRow
	allMsgs     []db.ListAllAssistantMessagesSinceRow
	delegations []db.ListDelegationOutcomesSinceRow
	allDelegs   []db.ListAllDelegationOutcomesSinceRow
	projects    []db.ProjectStatsSinceRow
	latency     []db.ListLatencyEventsSinceRow
	allLatency  []db.ListAllLatencyEventsSinceRow
}

func (s *stubQuerier) ListSessionTreeSince(context.Context, string) ([]db.ListSessionTreeSinceRow, error) {
	return s.tree, nil
}

func (s *stubQuerier) ListSessionTreeAssistantMessages(context.Context, string) ([]db.ListSessionTreeAssistantMessagesRow, error) {
	return s.treeMsgs, nil
}

func (s *stubQuerier) ListSessionsSinceWithAgent(context.Context, db.ListSessionsSinceWithAgentParams) ([]db.ListSessionsSinceWithAgentRow, error) {
	return s.projectRows, nil
}

func (s *stubQuerier) ListAssistantMessagesSince(context.Context, db.ListAssistantMessagesSinceParams) ([]db.ListAssistantMessagesSinceRow, error) {
	return s.projectMsgs, nil
}

func (s *stubQuerier) ListAllSessionsSince(context.Context, int64) ([]db.ListAllSessionsSinceRow, error) {
	return s.allRows, nil
}

func (s *stubQuerier) ListAllAssistantMessagesSince(context.Context, int64) ([]db.ListAllAssistantMessagesSinceRow, error) {
	return s.allMsgs, nil
}

func (s *stubQuerier) ListDelegationOutcomesSince(context.Context, db.ListDelegationOutcomesSinceParams) ([]db.ListDelegationOutcomesSinceRow, error) {
	return s.delegations, nil
}

func (s *stubQuerier) ListAllDelegationOutcomesSince(context.Context, int64) ([]db.ListAllDelegationOutcomesSinceRow, error) {
	return s.allDelegs, nil
}

func (s *stubQuerier) ListSkillLoadsSince(context.Context, db.ListSkillLoadsSinceParams) ([]db.ListSkillLoadsSinceRow, error) {
	return nil, nil
}

func (s *stubQuerier) ListLatencyEventsSince(context.Context, db.ListLatencyEventsSinceParams) ([]db.ListLatencyEventsSinceRow, error) {
	return s.latency, nil
}

func (s *stubQuerier) ListAllLatencyEventsSince(context.Context, int64) ([]db.ListAllLatencyEventsSinceRow, error) {
	return s.allLatency, nil
}

func (s *stubQuerier) ProjectStatsSince(context.Context, int64) ([]db.ProjectStatsSinceRow, error) {
	return s.projects, nil
}

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

// The agent column groups by agent_id, and falls back to the session
// title only when there is no agent id — the case for sessions recorded
// before that column existed and for generic task-tool delegations.
func TestComputeAgents_PrefersAgentIDOverTitle(t *testing.T) {
	t.Parallel()

	sessions := []stats.Session{
		{ID: "root", Title: "root session"},
		{ID: "a1", ParentID: "root", AgentID: "reviewer", Title: "Ревью итерации 1", PromptTokens: 100, CompletionTokens: 10, Cost: 0.5, CreatedAt: 10, UpdatedAt: 40},
		{ID: "a2", ParentID: "root", AgentID: "reviewer", Title: "Ревью итерации 2", PromptTokens: 200, CompletionTokens: 20, Cost: 0.5, CreatedAt: 40, UpdatedAt: 60},
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

// The session scope walks one session's tree and reports every session in
// it, which is the number that answers "how much did this run delegate".
func TestGather_SessionScopeCountsWholeTree(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		tree: []db.ListSessionTreeSinceRow{
			{ID: "root", Title: "root", PromptTokens: 100, CompletionTokens: 10, Cost: 1, CreatedAt: 0, UpdatedAt: 60},
			{ID: "kid", Title: "reviewer", AgentID: "reviewer", PromptTokens: 50, CompletionTokens: 5, CreatedAt: 10, UpdatedAt: 30},
		},
		treeMsgs: []db.ListSessionTreeAssistantMessagesRow{
			{SessionID: "root", Model: "opus", Provider: "anthropic", CreatedAt: 0, FinishedAt: 5},
		},
		allDelegs: []db.ListAllDelegationOutcomesSinceRow{
			{ID: "t1", SessionID: "kid", Status: "completed"},
			// Belongs to some other session's tree: must not be counted.
			{ID: "t2", SessionID: "elsewhere", Status: "completed"},
		},
	}

	// The tree row for "kid" has no parent set in this fixture, so give
	// it one the way the real query returns it.
	q.tree[1].ParentSessionID.String, q.tree[1].ParentSessionID.Valid = "root", true

	snap, err := stats.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeSession, SessionID: "root"})
	require.NoError(t, err)

	require.Equal(t, int64(2), snap.Totals.Sessions, "the session tab counts the whole tree, not just its root")
	require.Equal(t, int64(1), snap.Outcome.Total, "only delegations run by sessions in this tree count")
	require.Equal(t, int64(1), snap.Outcome.Landed)
	require.Len(t, snap.Agents, 1)
	require.Equal(t, "reviewer", snap.Agents[0].Name)
}

// The global scope adds the per-project breakdown, with a trailing TOTAL.
func TestGather_GlobalScopeAddsProjects(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		allRows: []db.ListAllSessionsSinceRow{{ID: "s1", Title: "one", PromptTokens: 10, Cost: 1}},
		projects: []db.ProjectStatsSinceRow{
			{ProjectPath: "/a", Sessions: 2, PromptTokens: int64(100), CompletionTokens: int64(10), Cost: 2.0, TimeSeconds: int64(30)},
			{ProjectPath: "/b", Sessions: 1, PromptTokens: int64(500), CompletionTokens: int64(50), Cost: 3.0, TimeSeconds: int64(60)},
		},
	}

	snap, err := stats.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeGlobal})
	require.NoError(t, err)

	require.Len(t, snap.Projects, 3, "two projects plus the TOTAL row")
	require.Equal(t, "/b", snap.Projects[0].Path, "projects sort by token total, busiest first")
	total := snap.Projects[len(snap.Projects)-1]
	require.Equal(t, "TOTAL", total.Path)
	require.Equal(t, int64(660), total.Tokens())
	require.InDelta(t, 5.0, total.Cost, 0.0001)
}

// An empty scope is not an error: a fresh project has nothing recorded,
// and the screen says so rather than showing a failure.
func TestGather_EmptyScopeIsNotAnError(t *testing.T) {
	t.Parallel()

	snap, err := stats.Gather(context.Background(), &stubQuerier{}, stats.Request{Scope: stats.ScopeProject, ProjectPath: "/nowhere"})
	require.NoError(t, err)
	require.True(t, snap.Empty())
}

func TestGather_SessionScopeNeedsASessionID(t *testing.T) {
	t.Parallel()

	_, err := stats.Gather(context.Background(), &stubQuerier{}, stats.Request{Scope: stats.ScopeSession})
	require.Error(t, err)
}
