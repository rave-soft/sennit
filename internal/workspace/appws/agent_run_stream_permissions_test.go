package appws

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// immediateCoordinator is a minimal agent.Coordinator whose Run returns
// straight away with an empty result, for tests that only care about
// what AgentRunStream does before/around the turn, not the turn itself.
type immediateCoordinator struct {
	agent.Coordinator
}

func (c *immediateCoordinator) UpdateModels(context.Context) error { return nil }

func (c *immediateCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{}, nil
}

// SetLiveSession is inert: AgentRunStream reports the run's session
// identity through it (see App.ReportCurrentSession) but this test
// doesn't care which session herdr thinks is live.
func (c *immediateCoordinator) SetLiveSession(string) {}

// recordingAutoApprovePermissions is a permission.Service double that
// only tracks AutoApproveSession calls; embedding the (nil) interface
// means anything AgentRunStream is not supposed to reach for panics
// loudly rather than silently doing nothing.
type recordingAutoApprovePermissions struct {
	permission.Service
	autoApprovedSessions []string
}

func (p *recordingAutoApprovePermissions) AutoApproveSession(sessionID string) {
	p.autoApprovedSessions = append(p.autoApprovedSessions, sessionID)
}

// TestAppWorkspace_AgentRunStream_AutoApprovePermissionsOptIn proves the
// permission bypass is opt-in rather than unconditional: AgentRunStream
// must call Permissions().AutoApproveSession only when the caller asks
// for it via AgentRunOptions.AutoApprovePermissions, never as a side
// effect of streaming a turn.
func TestAppWorkspace_AgentRunStream_AutoApprovePermissionsOptIn(t *testing.T) {
	sessions, messages := newRealSessionAgentEnv(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.MCP = mcp.NewRegistry() // AgentRunStream calls WaitForInit unconditionally; NewForTest leaves it nil.
	a.SetSessionsForTest(sessions)
	a.SetMessagesForTest(messages)
	a.SetAgentCoordinatorForTest(&immediateCoordinator{})

	perms := &recordingAutoApprovePermissions{}
	a.SetPermissionsForTest(perms)

	sess, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	out, err := aw.AgentRunStream(t.Context(), sess.ID, "hello", workspace.AgentRunOptions{})
	require.NoError(t, err)
	for range out {
	}
	require.Empty(t, perms.autoApprovedSessions, "AgentRunStream must not auto-approve permissions unless asked to")

	out, err = aw.AgentRunStream(t.Context(), sess.ID, "hello", workspace.AgentRunOptions{AutoApprovePermissions: true})
	require.NoError(t, err)
	for range out {
	}
	require.Equal(t, []string{sess.ID}, perms.autoApprovedSessions, "AgentRunStream must auto-approve permissions when asked to")
}
