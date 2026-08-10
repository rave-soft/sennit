package session

import (
	"testing"

	"github.com/rave-soft/braid/internal/db"
	"github.com/stretchr/testify/require"
)

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, fetched.EstimatedUsage)

	fetched.Todos = []Todo{{
		Content:    "Check estimate state",
		Status:     TodoStatusInProgress,
		ActiveForm: "Checking estimate state",
	}}

	updated, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.True(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, refetched.EstimatedUsage)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	saved.EstimatedUsage = false
	updated, err := sessions.Save(t.Context(), saved)
	require.NoError(t, err)
	require.False(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, refetched.EstimatedUsage)
}

// TestListExcludesAgentToolChildSessions is a regression test for the
// sessions dialog (ctrl+s): child sessions created for sub-agent
// delegations (agent/agentic_fetch tool calls) have a "$$"-formatted ID
// and a non-null ParentSessionID (see CreateTaskSession /
// CreateAgentToolSessionID), and must never show up in List() — the
// underlying ListSessions query filters on parent_session_id IS NULL.
func TestListExcludesAgentToolChildSessions(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	childID := sessions.CreateAgentToolSessionID("msg-1", "tc-1")
	_, err = sessions.CreateTaskSession(t.Context(), childID, parent.ID, "sub-agent delegation")
	require.NoError(t, err)

	// Sanity-check the child session actually exists and is fetchable by
	// ID before asserting it's absent from the listing.
	child, err := sessions.Get(t.Context(), childID)
	require.NoError(t, err)
	require.Equal(t, parent.ID, child.ParentSessionID)

	all, err := sessions.List(t.Context())
	require.NoError(t, err)
	for _, s := range all {
		require.NotEqual(t, childID, s.ID, "agent-tool child session must not appear in List()")
	}
	require.Len(t, all, 1, "only the parent session should be listed")
	require.Equal(t, parent.ID, all[0].ID)
}
