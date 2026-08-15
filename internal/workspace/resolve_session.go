package workspace

import (
	"context"
	"fmt"

	"github.com/rave-soft/braid/internal/session"
)

// ResolveSession resolves which session a non-interactive run should
// use. If continueSessionID is set it must name an existing top-level
// session (not a child session, not an agent-tool session). If
// useLast is set it returns the most recently updated top-level
// session. Otherwise it creates a new session titled title.
//
// This is shared across Workspace implementations (via cmd/run.go) so
// `braid run` behaves identically regardless of which one is in use; it
// is a free function rather than an interface method because it only
// needs the operations Workspace already exposes.
func ResolveSession(ctx context.Context, ws Workspace, continueSessionID string, useLast bool, title string) (session.Session, error) {
	switch {
	case continueSessionID != "":
		if _, _, ok := ws.ParseAgentToolSessionID(continueSessionID); ok {
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
		sessions, err := ws.ListSessions(ctx)
		if err != nil || len(sessions) == 0 {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		last := sessions[0]
		for _, s := range sessions[1:] {
			if s.UpdatedAt > last.UpdatedAt && s.ParentSessionID == "" {
				last = s
			}
		}
		return last, nil

	default:
		return ws.CreateSession(ctx, title)
	}
}
