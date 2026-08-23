package workspace

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeSessionWorkspace is a minimal Workspace test double covering
// only the methods ResolveSession calls (GetSession, ListSessions,
// CreateSession, ParseAgentToolSessionID). It embeds the Workspace
// interface (left nil) so it satisfies the full interface at compile
// time without stubbing every method; calling anything else panics,
// which is fine since these tests only exercise ResolveSession.
type fakeSessionWorkspace struct {
	Workspace

	sessions []session.Session
	created  []session.Session
}

func (w *fakeSessionWorkspace) CreateSession(_ context.Context, title string) (session.Session, error) {
	s := session.Session{ID: "new-session-id", Title: title}
	w.created = append(w.created, s)
	return s, nil
}

func (w *fakeSessionWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	for _, s := range w.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return session.Session{}, sql.ErrNoRows
}

func (w *fakeSessionWorkspace) ListSessions(context.Context) ([]session.Session, error) {
	return w.sessions, nil
}

// GetLastSession mirrors the SQL query's scope (top-level sessions only,
// newest updated_at wins) over the fake's in-memory list, so tests can
// drive ResolveSession's useLast branch without a real database.
func (w *fakeSessionWorkspace) GetLastSession(context.Context) (session.Session, error) {
	var (
		last  session.Session
		found bool
	)
	for _, s := range w.sessions {
		if s.ParentSessionID != "" {
			continue
		}
		if !found || s.UpdatedAt > last.UpdatedAt {
			last = s
			found = true
		}
	}
	if !found {
		return session.Session{}, sql.ErrNoRows
	}
	return last, nil
}

func (w *fakeSessionWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func TestResolveSession_NewSession(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{}

	sess, err := ResolveSession(t.Context(), ws, "", false, "non-interactive")
	require.NoError(t, err)
	require.Equal(t, "new-session-id", sess.ID)
	require.Len(t, ws.created, 1)
}

func TestResolveSession_ContinueByID(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "existing-id", Title: "Old session"},
		},
	}

	sess, err := ResolveSession(t.Context(), ws, "existing-id", false, "non-interactive")
	require.NoError(t, err)
	require.Equal(t, "existing-id", sess.ID)
	require.Equal(t, "Old session", sess.Title)
	require.Empty(t, ws.created)
}

func TestResolveSession_ContinueByID_NotFound(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{}

	_, err := ResolveSession(t.Context(), ws, "nonexistent", false, "non-interactive")
	require.Error(t, err)
	require.Contains(t, err.Error(), "session not found")
}

func TestResolveSession_ContinueByID_ChildSession(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "child-id", ParentSessionID: "parent-id", Title: "Child session"},
		},
	}

	_, err := ResolveSession(t.Context(), ws, "child-id", false, "non-interactive")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot continue a child session")
}

// TestResolveSession_ContinueByID_AgentToolSession covers that `sennit run
// --session <agent-tool-session-id>` must be rejected, instead of quietly
// attaching to an internal tool-call session.
func TestResolveSession_ContinueByID_AgentToolSession(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{}

	_, err := ResolveSession(t.Context(), ws, "msg123$$tool456", false, "non-interactive")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot continue an agent tool session")
}

func TestResolveSession_Last(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "most-recent", Title: "Latest session", UpdatedAt: 100},
			{ID: "older", Title: "Older session", UpdatedAt: 50},
		},
	}

	sess, err := ResolveSession(t.Context(), ws, "", true, "non-interactive")
	require.NoError(t, err)
	require.Equal(t, "most-recent", sess.ID)
	require.Empty(t, ws.created)
}

func TestResolveSession_Last_SkipsChildSessions(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "top-level", Title: "Top level", UpdatedAt: 50},
			{ID: "child", ParentSessionID: "top-level", Title: "Child", UpdatedAt: 100},
		},
	}

	sess, err := ResolveSession(t.Context(), ws, "", true, "non-interactive")
	require.NoError(t, err)
	require.Equal(t, "top-level", sess.ID)
}

// TestResolveSession_Last_SkipsChildSessionFirst covers the regression where
// the accumulator was seeded with sessions[0] unconditionally: a child
// session with the newest UpdatedAt, listed first, must not win over an
// older top-level session.
func TestResolveSession_Last_SkipsChildSessionFirst(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "child", ParentSessionID: "top-level", Title: "Child", UpdatedAt: 100},
			{ID: "top-level", Title: "Top level", UpdatedAt: 50},
		},
	}

	sess, err := ResolveSession(t.Context(), ws, "", true, "non-interactive")
	require.NoError(t, err)
	require.Equal(t, "top-level", sess.ID)
}

// TestResolveSession_Last_SkipsAgentToolSessionFirst covers that an
// agent-tool sub-session listed first (and newest) must not be returned,
// matching the continueSessionID branch's rejection of such sessions.
// session.CreateSubAgentSession always stamps ParentSessionID, which is
// what GetLastSession's SQL scope (and this fake) actually filters on -
// an agent-tool session id with no parent, as the pre-SQL scan's belt-
// and-suspenders check also handled, cannot occur against a real store.
func TestResolveSession_Last_SkipsAgentToolSessionFirst(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "msg123$$tool456", ParentSessionID: "top-level", Title: "Agent tool session", UpdatedAt: 100},
			{ID: "top-level", Title: "Top level", UpdatedAt: 50},
		},
	}

	sess, err := ResolveSession(t.Context(), ws, "", true, "non-interactive")
	require.NoError(t, err)
	require.Equal(t, "top-level", sess.ID)
}

// TestResolveSession_Last_AllChildSessions covers that when every session is
// ineligible, ResolveSession reports the same error as an empty list rather
// than falling back to sessions[0].
func TestResolveSession_Last_AllChildSessions(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "child-1", ParentSessionID: "parent", Title: "Child 1", UpdatedAt: 100},
			{ID: "child-2", ParentSessionID: "parent", Title: "Child 2", UpdatedAt: 50},
		},
	}

	_, err := ResolveSession(t.Context(), ws, "", true, "non-interactive")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no sessions found to continue")
}

// TestResolveSession_Last_NewestFirst covers that list order doesn't matter
// when all sessions are eligible: the newest one wins whether it's listed
// first or last.
func TestResolveSession_Last_NewestFirst(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{
		sessions: []session.Session{
			{ID: "most-recent", Title: "Latest session", UpdatedAt: 100},
			{ID: "older", Title: "Older session", UpdatedAt: 50},
			{ID: "oldest", Title: "Oldest session", UpdatedAt: 10},
		},
	}

	sess, err := ResolveSession(t.Context(), ws, "", true, "non-interactive")
	require.NoError(t, err)
	require.Equal(t, "most-recent", sess.ID)
}

func TestResolveSession_Last_NoSessions(t *testing.T) {
	t.Parallel()
	ws := &fakeSessionWorkspace{}

	_, err := ResolveSession(t.Context(), ws, "", true, "non-interactive")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no sessions found")
}
