package gather_test

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/stats/gather"
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

	snap, err := gather.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeSession, SessionID: "root"})
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

	snap, err := gather.Gather(context.Background(), q, stats.Request{Scope: stats.ScopeGlobal})
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

	snap, err := gather.Gather(context.Background(), &stubQuerier{}, stats.Request{Scope: stats.ScopeProject, ProjectPath: "/nowhere"})
	require.NoError(t, err)
	require.True(t, snap.Empty())
}

func TestGather_SessionScopeNeedsASessionID(t *testing.T) {
	t.Parallel()

	_, err := gather.Gather(context.Background(), &stubQuerier{}, stats.Request{Scope: stats.ScopeSession})
	require.Error(t, err)
}
