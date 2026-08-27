package dialog

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

func newMCPAuthTestCommon() *common.Common {
	s := styles.SennitDark()
	return &common.Common{
		Styles: &s,
		Ctx:    context.Background(),
	}
}

// startedCtx drives startAuth's returned Action to completion and returns
// the context it handed to ActionMCPAuthStarted. startAuth batches the
// spinner tick alongside the "started" message (see MCPAuth.startAuth), so
// this unwraps a possible tea.BatchMsg to find it.
func startedCtx(t *testing.T, action Action) context.Context {
	t.Helper()
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "startAuth must return an ActionCmd, got %T", action)
	require.NotNil(t, cmdAction.Cmd)

	msg := cmdAction.Cmd()
	if started, ok := msg.(ActionMCPAuthStarted); ok {
		return started.Ctx
	}
	batch, ok := msg.(tea.BatchMsg)
	require.True(t, ok, "expected ActionMCPAuthStarted or a batch containing it, got %T", msg)
	for _, cmd := range batch {
		if cmd == nil {
			continue
		}
		if started, ok := cmd().(ActionMCPAuthStarted); ok {
			return started.Ctx
		}
	}
	t.Fatal("ActionMCPAuthStarted not found in batch")
	return nil
}

// TestMCPAuth_CompleteCancelsAuthContext covers the context leak: the
// per-attempt context startAuth creates from com.Context() must be
// cancelled once the flow reports success, not just forgotten. Without the
// fix, m.cancelAuth is set to nil directly and the context (and whatever
// goroutine selects on it) stays alive until the session exits.
func TestMCPAuth_CompleteCancelsAuthContext(t *testing.T) {
	com := newMCPAuthTestCommon()
	pending := []workspace.MCPPendingAuthServer{{Name: "server1", URL: "http://example.com"}}
	m, _ := NewMCPAuth(com, pending, func(string) string { return "" })

	ctx := startedCtx(t, m.startAuth())
	require.NoError(t, ctx.Err(), "context must still be live while the attempt is in flight")

	m.HandleMsg(ActionMCPAuthComplete{Name: "server1"})

	require.Error(t, ctx.Err(), "the auth context must be cancelled once the attempt completes")
	require.Nil(t, m.cancelAuth, "cancelAuth must be cleared after use")
}

// TestMCPAuth_ErrorCancelsAuthContext is the same leak on the failure path.
func TestMCPAuth_ErrorCancelsAuthContext(t *testing.T) {
	com := newMCPAuthTestCommon()
	pending := []workspace.MCPPendingAuthServer{{Name: "server1", URL: "http://example.com"}}
	m, _ := NewMCPAuth(com, pending, func(string) string { return "" })

	ctx := startedCtx(t, m.startAuth())
	require.NoError(t, ctx.Err())

	m.HandleMsg(ActionMCPAuthErrored{Name: "server1", Error: context.DeadlineExceeded})

	require.Error(t, ctx.Err(), "the auth context must be cancelled once the attempt errors")
	require.Nil(t, m.cancelAuth, "cancelAuth must be cleared after use")
}

// TestMCPAuth_CloseCancelsAuthContext covers the pre-existing Close path,
// guarding against a regression there while the other two sites are fixed.
func TestMCPAuth_CloseCancelsAuthContext(t *testing.T) {
	com := newMCPAuthTestCommon()
	pending := []workspace.MCPPendingAuthServer{{Name: "server1", URL: "http://example.com"}}
	m, _ := NewMCPAuth(com, pending, func(string) string { return "" })

	ctx := startedCtx(t, m.startAuth())
	require.NoError(t, ctx.Err())

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.IsType(t, ActionClose{}, action)

	require.Error(t, ctx.Err(), "closing the dialog must cancel the in-flight auth context")
}
