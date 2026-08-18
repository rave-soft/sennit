package workspace

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// threadEventSubscriber mirrors the interface the TUI router type-asserts
// for when it attaches to a thread (see internal/ui/model's Root). It is
// restated here because that assertion is the whole contract: it lives in
// another package, it is silent when it fails, and what it costs when it
// fails is every live event on the thread's screen.
type threadEventSubscriber interface {
	SubscribeWith(send func(tea.Msg)) func()
}

// The wrapper AttachThread returns must still offer the subscription.
//
// It embeds the Workspace interface, which does not carry SubscribeWith —
// that is a concrete AppWorkspace method — so nothing is promoted and the
// wrapper silently stopped satisfying this. The attach still succeeded and
// the thread's screen simply never received another event: its chat froze
// while its agent worked, and only leaving and re-entering showed what had
// happened.
func TestAttachedThreadWorkspace_StillOffersSubscribeWith(t *testing.T) {
	var ws Workspace = &attachedThreadWorkspace{Workspace: &subscribeStubWorkspace{}}

	sub, ok := ws.(threadEventSubscriber)
	require.True(t, ok,
		"the attached-thread wrapper must satisfy the subscriber interface the router asserts for")

	stop := sub.SubscribeWith(func(tea.Msg) {})
	require.NotNil(t, stop)
	stop()
}

// A wrapped workspace that cannot subscribe (a read-only one, say) must
// degrade to a no-op rather than panic: the caller always calls stop.
func TestAttachedThreadWorkspace_SubscribeWithoutSupportIsANoop(t *testing.T) {
	ws := &attachedThreadWorkspace{Workspace: &plainStubWorkspace{}}

	stop := ws.SubscribeWith(func(tea.Msg) {})
	require.NotNil(t, stop)
	stop()
}

// subscribeStubWorkspace is a Workspace that can subscribe, standing in
// for the thread's own AppWorkspace.
type subscribeStubWorkspace struct {
	Workspace
	stopped bool
}

func (s *subscribeStubWorkspace) SubscribeWith(func(tea.Msg)) func() {
	return func() { s.stopped = true }
}

// plainStubWorkspace is a Workspace with no subscription of its own.
type plainStubWorkspace struct{ Workspace }

// The thread's own session must not drag the thread back onto the model it
// used to run on: it runs whatever its parent runs, handed down at spawn.
func TestAttachedThreadWorkspace_IgnoresSessionModelPinForItsOwnSession(t *testing.T) {
	inner := &applyModelStubWorkspace{}
	ws := &attachedThreadWorkspace{Workspace: inner, sessionID: "thread-session"}

	switched, err := ws.ApplySessionModel(t.Context(), "thread-session")
	require.NoError(t, err)
	require.False(t, switched)
	require.Empty(t, inner.applied, "the thread's own session must not reach the wrapped workspace")
}

// Every other session reachable from the thread's screen is not the
// delegation, so it keeps the ordinary behavior.
func TestAttachedThreadWorkspace_AppliesSessionModelForOtherSessions(t *testing.T) {
	inner := &applyModelStubWorkspace{switched: true}
	ws := &attachedThreadWorkspace{Workspace: inner, sessionID: "thread-session"}

	switched, err := ws.ApplySessionModel(t.Context(), "other-session")
	require.NoError(t, err)
	require.True(t, switched)
	require.Equal(t, []string{"other-session"}, inner.applied)
}

// applyModelStubWorkspace records the session IDs ApplySessionModel was
// called with.
type applyModelStubWorkspace struct {
	Workspace
	applied  []string
	switched bool
}

func (s *applyModelStubWorkspace) ApplySessionModel(_ context.Context, sessionID string) (bool, error) {
	s.applied = append(s.applied, sessionID)
	return s.switched, nil
}
