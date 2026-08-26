package session

import (
	"database/sql"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
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

	sessions := NewService(db.New(conn), conn, dataDir)

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

	sessions := NewService(db.New(conn), conn, dataDir)

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

	sessions := NewService(db.New(conn), conn, dataDir)

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

// TestDeleteRemovesDescendantSessions pins the behaviour that
// parent_session_id cannot express as a foreign key: deleting a session
// has to take its sub-sessions (and their own children) with it, or they
// are left orphaned and reachable only by `sennit gc`.
func TestDeleteRemovesDescendantSessions(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	queries := db.New(conn)
	sessions := NewService(queries, conn, dataDir)

	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), "child-1", parent.ID, "delegation")
	require.NoError(t, err)
	grandchild, err := sessions.CreateTaskSession(t.Context(), "child-2", child.ID, "nested delegation")
	require.NoError(t, err)
	title, err := sessions.CreateTitleSession(t.Context(), parent.ID)
	require.NoError(t, err)

	// A message on the grandchild proves the cascade reaches child rows
	// of a session that was only deleted transitively.
	_, err = queries.CreateMessage(t.Context(), db.CreateMessageParams{
		ID: "msg-1", SessionID: grandchild.ID, Role: "user", Parts: "[]",
	})
	require.NoError(t, err)

	// An unrelated session must survive.
	bystander, err := sessions.Create(t.Context(), "bystander")
	require.NoError(t, err)

	require.NoError(t, sessions.Delete(t.Context(), parent.ID))

	for _, id := range []string{parent.ID, child.ID, grandchild.ID, title.ID} {
		_, err = sessions.Get(t.Context(), id)
		require.Error(t, err, "session %s should have been deleted with the tree", id)
	}

	remaining, err := queries.CountSessionMessages(t.Context(), grandchild.ID)
	require.NoError(t, err)
	require.Zero(t, remaining, "messages should cascade from a transitively deleted session")

	survivor, err := sessions.Get(t.Context(), bystander.ID)
	require.NoError(t, err)
	require.Equal(t, bystander.ID, survivor.ID)
}

// recordingSink is a [TelemetrySink] that counts the lifecycle reports it
// receives, so tests can assert the service reports through the sink
// rather than reaching into the telemetry package itself.
type recordingSink struct {
	created int
	deleted int
}

func (s *recordingSink) SessionCreated() { s.created++ }
func (s *recordingSink) SessionDeleted() { s.deleted++ }

// TestCreateAndDeleteReportThroughTelemetrySink covers that the service
// reports session creation and deletion through the wired sink, and that
// the sink-less service (the zero sink every test build uses) is nil-safe:
// no sink means no report, not a panic.
func TestCreateAndDeleteReportThroughTelemetrySink(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := db.New(conn)

	sink := &recordingSink{}
	sessions := NewService(q, conn, dataDir, WithTelemetry(sink))

	created, err := sessions.Create(t.Context(), "reported")
	require.NoError(t, err)
	require.Equal(t, 1, sink.created, "Create must report through the sink")
	require.Zero(t, sink.deleted)

	// Child creations are not lifecycle reports of their own.
	_, err = sessions.CreateTaskSession(t.Context(), "child-1", created.ID, "delegation")
	require.NoError(t, err)
	require.Equal(t, 1, sink.created)

	require.NoError(t, sessions.Delete(t.Context(), created.ID))
	require.Equal(t, 1, sink.deleted, "Delete must report through the sink")

	// A service built without a sink must be nil-safe.
	silent := NewService(q, conn, dataDir)
	_, err = silent.Create(t.Context(), "silent")
	require.NoError(t, err)
}

func TestGetReportsMissingSessionAsErrNotFound(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn, dataDir)

	_, err = sessions.Get(t.Context(), "does-not-exist")
	require.ErrorIs(t, err, ErrNotFound)
	require.NotErrorIs(t, err, sql.ErrNoRows,
		"the bare driver error reaches the user verbatim in the status line")
	require.Contains(t, err.Error(), "does-not-exist", "the id being looked up must survive into the message")
}

// A session's pinned model has to survive the round trip through the
// store, and Save must not quietly drop it: Save writes a whole Session
// back, and the model is not among the columns UpdateSession sets, so a
// fetch-modify-save cycle is exactly where a pin would go missing.
func TestSetModelSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn, dataDir)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	require.True(t, created.Model.IsZero(), "a fresh session pins no model")

	pinned := ModelRef{Provider: "anthropic", Model: "claude-opus-5"}
	require.NoError(t, sessions.SetModel(t.Context(), created.ID, pinned))

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, pinned, fetched.Model)

	fetched.Title = "renamed by an unrelated edit"
	saved, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.Equal(t, pinned, saved.Model, "an unrelated save must not drop the pin")

	again, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, pinned, again.Model)

	// Re-pinning replaces rather than accumulates, and the zero ref clears.
	moved := ModelRef{Provider: "openai", Model: "gpt-5"}
	require.NoError(t, sessions.SetModel(t.Context(), created.ID, moved))
	again, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, moved, again.Model)

	require.NoError(t, sessions.SetModel(t.Context(), created.ID, ModelRef{}))
	again, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, again.Model.IsZero(), "the zero ref clears the pin")
}

// setUpdatedAt backdates/forward-dates a session's updated_at directly,
// bypassing the service so ordering in GetLast tests doesn't depend on
// wall-clock gaps between Create calls landing in different seconds. The
// "preserve explicit updated_at" trigger (see
// 20260811000001_preserve_explicit_session_updated_at.sql) exists for
// exactly this: writers that set the column explicitly keep their value.
func setUpdatedAt(t *testing.T, conn *sql.DB, id string, updatedAt int64) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(), `UPDATE sessions SET updated_at = ? WHERE id = ?`, updatedAt, id)
	require.NoError(t, err)
}

// TestGetLastReturnsNewestTopLevelSession is the [Service.GetLast]
// counterpart to workspace.ResolveSession's old client-side scan: among
// several candidate sessions it must return the one with the highest
// updated_at, matching the scan's `s.UpdatedAt > last.UpdatedAt` rule,
// regardless of creation or listing order.
func TestGetLastReturnsNewestTopLevelSession(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn, dataDir)

	oldest, err := sessions.Create(t.Context(), "oldest")
	require.NoError(t, err)
	setUpdatedAt(t, conn, oldest.ID, 10)

	newest, err := sessions.Create(t.Context(), "newest")
	require.NoError(t, err)
	setUpdatedAt(t, conn, newest.ID, 100)

	middle, err := sessions.Create(t.Context(), "middle")
	require.NoError(t, err)
	setUpdatedAt(t, conn, middle.ID, 50)

	last, err := sessions.GetLast(t.Context())
	require.NoError(t, err)
	require.Equal(t, newest.ID, last.ID)
}

// TestGetLastExcludesChildSessions covers the filtering the old scan did
// client-side: agent-tool sub-sessions and title-generation sessions carry
// a non-null parent_session_id and must never win, even when they are the
// most recently updated row in the table.
func TestGetLastExcludesChildSessions(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn, dataDir)

	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	setUpdatedAt(t, conn, parent.ID, 10)

	childID := sessions.CreateAgentToolSessionID("msg-1", "tc-1")
	_, err = sessions.CreateTaskSession(t.Context(), childID, parent.ID, "sub-agent delegation")
	require.NoError(t, err)
	setUpdatedAt(t, conn, childID, 100)

	titleSess, err := sessions.CreateTitleSession(t.Context(), parent.ID)
	require.NoError(t, err)
	setUpdatedAt(t, conn, titleSess.ID, 90)

	last, err := sessions.GetLast(t.Context())
	require.NoError(t, err)
	require.Equal(t, parent.ID, last.ID, "child/title sessions must not win even when most recently updated")
}

// TestGetLastReportsErrorWhenNoSessions covers the empty case: the old
// scan returned "no sessions found to continue" for either an empty
// ListSessions result or a list of only ineligible sessions.
// workspace.ResolveSession maps any GetLast error to that same message,
// so here it's enough to assert GetLast itself errors on an empty store.
func TestGetLastReportsErrorWhenNoSessions(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn, dataDir)

	_, err = sessions.GetLast(t.Context())
	require.Error(t, err)
}

// TestGetLastScopesToProjectPath covers that GetLast, like List, only
// considers sessions in this service's own project: sessions now live in
// a single shared database, so a newer session in another project must
// not be returned as "the last session" here.
func TestGetLastScopesToProjectPath(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	here := NewService(db.New(conn), conn, "/project/here")
	elsewhere := NewService(db.New(conn), conn, "/project/elsewhere")

	local, err := here.Create(t.Context(), "local")
	require.NoError(t, err)
	setUpdatedAt(t, conn, local.ID, 10)

	other, err := elsewhere.Create(t.Context(), "other")
	require.NoError(t, err)
	setUpdatedAt(t, conn, other.ID, 100)

	last, err := here.GetLast(t.Context())
	require.NoError(t, err)
	require.Equal(t, local.ID, last.ID)
}
