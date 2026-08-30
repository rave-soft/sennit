package common

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

type attachedSessionChangesWorkspace struct {
	workspace.Workspace
	inner workspace.SessionChangePreparer
}

// KnownProviders: no test here renders a provider list.
func (w attachedSessionChangesWorkspace) KnownProviders() []catwalk.Provider { return nil }

func (w *attachedSessionChangesWorkspace) Config() *config.Config {
	return &config.Config{}
}

func (w *attachedSessionChangesWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]workspace.SessionFile, error) {
	if w.inner == nil {
		return nil, errors.New("session change preparer is unavailable")
	}
	return w.inner.PrepareSessionChanges(ctx, sessionID)
}

type recordingSessionChangePreparer struct {
	sessions []string
}

func (p *recordingSessionChangePreparer) PrepareSessionChanges(_ context.Context, sessionID string) ([]workspace.SessionFile, error) {
	p.sessions = append(p.sessions, sessionID)
	return []workspace.SessionFile{{FirstVersion: history.File{Path: sessionID}}}, nil
}

func TestDefaultCommonPreservesAttachedSessionChangePreparer(t *testing.T) {
	preparer := &recordingSessionChangePreparer{}
	attached := &attachedSessionChangesWorkspace{inner: preparer}

	com := DefaultCommon(t.Context(), attached)
	require.NotNil(t, com.SessionChanges)
	files, err := com.SessionChanges.PrepareSessionChanges(t.Context(), "thread-session")
	require.NoError(t, err)
	require.Equal(t, []workspace.SessionFile{{FirstVersion: history.File{Path: "thread-session"}}}, files)
	require.Equal(t, []string{"thread-session"}, preparer.sessions)
}

func TestDefaultCommonAttachedSessionChangesUnavailable(t *testing.T) {
	com := DefaultCommon(t.Context(), &attachedSessionChangesWorkspace{})

	require.NotNil(t, com.SessionChanges)
	_, err := com.SessionChanges.PrepareSessionChanges(t.Context(), "thread-session")
	require.EqualError(t, err, "session change preparer is unavailable")
}
