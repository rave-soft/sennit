package workspace

import (
	"context"
	"fmt"

	"github.com/rave-soft/sennit/internal/session"
)

type sessionResolver interface {
	CreateSession(ctx context.Context, title string) (session.Session, error)
	GetSession(ctx context.Context, sessionID string) (session.Session, error)
	GetLastSession(ctx context.Context) (session.Session, error)
}

// ResolveSession resolves which session a non-interactive run should
// use. If continueSessionID is set it must name an existing top-level
// session (not a child session, not an agent-tool session). If
// useLast is set it returns the most recently updated top-level
// session. Otherwise it creates a new session titled title.
//
// This is shared across Workspace implementations (via cmd/run.go) so
// `sennit run` behaves identically regardless of which one is in use; it
// is a free function rather than an interface method because it only
// needs sessionResolver's three operations.
func ResolveSession(ctx context.Context, ws sessionResolver, continueSessionID string, useLast bool, title string) (session.Session, error) {
	switch {
	case continueSessionID != "":
		if _, _, ok := session.ParseAgentToolSessionID(continueSessionID); ok {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := ws.GetSession(ctx, continueSessionID)
		if err != nil {
			return session.Session{}, fmt.Errorf("session not found: %s", continueSessionID)
		}
		if sess.ParentSessionID != "" {
			return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
		}
		return sess, nil

	case useLast:
		// GetLastSession is scoped in SQL to top-level sessions only (see
		// its query), which already excludes agent-tool sub-sessions:
		// those always carry a parent_session_id. Any error - including
		// "no rows", when the project has no sessions - collapses to the
		// same message the empty-list case always reported.
		sess, err := ws.GetLastSession(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
		return ws.CreateSession(ctx, title)
	}
}
