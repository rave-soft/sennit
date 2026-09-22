package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestRunTurn_FinishedMarksChildSession pins that TypeAgentFinished says
// whether the session nests under a parent, so the UI can keep the
// busy->idle edge for a delegated task or thread without raising a
// desktop notification for it.
func TestRunTurn_FinishedMarksChildSession(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &failingStreamModel{}
	// Skip the model's one scripted failure: every turn here succeeds.
	model.calls.Store(1)

	notifyBroker := pubsub.NewBroker[notify.Notification]()
	t.Cleanup(notifyBroker.Shutdown)
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 100000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Notify:   notifyBroker,
	}).(*sessionAgent)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), "tool-call", parent.ID, "child")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	notifications := notifyBroker.Subscribe(ctx)

	for _, tc := range []struct {
		sessionID string
		child     bool
	}{
		{sessionID: parent.ID, child: false},
		{sessionID: child.ID, child: true},
	} {
		_, err := sa.Run(t.Context(), SessionAgentCall{SessionID: tc.sessionID, Prompt: "go"})
		require.NoError(t, err)
		finished := awaitNotification(t, ctx, notifications, notify.TypeAgentFinished)
		require.Equal(t, tc.sessionID, finished.SessionID)
		require.Equal(t, tc.child, finished.ChildSession)
	}
}
