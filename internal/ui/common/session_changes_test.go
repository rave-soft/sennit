package common

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

type attachedSessionChangesWorkspace struct {
	workspace.Workspace
	inner workspace.SessionChangePreparer
}

// KnownProviders: no test here renders a provider list.
func (w attachedSessionChangesWorkspace) KnownProviders() []catwalk.Provider { return nil }

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w attachedSessionChangesWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w attachedSessionChangesWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w attachedSessionChangesWorkspace) BuiltinSkills() []*skills.Skill {
	return skills.DiscoverBuiltin()
}

func (w *attachedSessionChangesWorkspace) Config() *workspace.FrontendConfig {
	return &workspace.FrontendConfig{}
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

// TestDefaultCommonPreservesAttachedSessionChangePreparer and
// TestDefaultCommonAttachedSessionChangesUnavailable used to pin
// DefaultCommon's own type assertion (ws.(workspace.SessionChangePreparer))
// that populated a separate Common.SessionChanges field. PR 0.7c's review
// found that assertion could not survive wsrpc.Loopback or a real gRPC
// client (same class as commands.go's WorktreeState finding), so
// PrepareSessionChanges is now a guaranteed member of workspace.Workspace
// itself (FileServices) and DefaultCommon no longer probes for it -- a
// caller reaches it directly through com.Workspace. What these tests
// actually need to prove -- that Common carries whatever
// PrepareSessionChanges implementation the wrapped workspace has, forward
// and unavailable cases alike -- is exercised directly below instead.
func TestDefaultCommonWorkspacePreparesSessionChanges(t *testing.T) {
	preparer := &recordingSessionChangePreparer{}
	attached := &attachedSessionChangesWorkspace{inner: preparer}

	com := DefaultCommon(t.Context(), attached)
	files, err := com.Workspace.PrepareSessionChanges(t.Context(), "thread-session")
	require.NoError(t, err)
	require.Equal(t, []workspace.SessionFile{{FirstVersion: history.File{Path: "thread-session"}}}, files)
	require.Equal(t, []string{"thread-session"}, preparer.sessions)
}

func TestDefaultCommonWorkspaceSessionChangesUnavailable(t *testing.T) {
	com := DefaultCommon(t.Context(), &attachedSessionChangesWorkspace{})

	_, err := com.Workspace.PrepareSessionChanges(t.Context(), "thread-session")
	require.EqualError(t, err, "session change preparer is unavailable")
}
