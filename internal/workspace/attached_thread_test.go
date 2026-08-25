package workspace

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

// threadEventSubscriber mirrors the interface the TUI router type-asserts
// for when it attaches to a thread (see internal/ui/model's Root). It is
// restated here because that assertion is the whole contract: it lives in
// another package, it is silent when it fails, and what it costs when it
// fails is every live event on the thread's screen.
type threadEventSubscriber interface {
	SubscribeWith(send func(any)) func()
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

	stop := sub.SubscribeWith(func(any) {})
	require.NotNil(t, stop)
	stop()
}

// A wrapped workspace that cannot subscribe (a read-only one, say) must
// degrade to a no-op rather than panic: the caller always calls stop.
func TestAttachedThreadWorkspace_SubscribeWithoutSupportIsANoop(t *testing.T) {
	ws := &attachedThreadWorkspace{Workspace: &plainStubWorkspace{}}

	stop := ws.SubscribeWith(func(any) {})
	require.NotNil(t, stop)
	stop()
}

// subscribeStubWorkspace is a Workspace that can subscribe, standing in
// for the thread's own AppWorkspace.
type subscribeStubWorkspace struct {
	Workspace
	stopped bool
}

func (s *subscribeStubWorkspace) SubscribeWith(func(any)) func() {
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

// While the user is drilled into a thread, every event is routed to that
// thread's UI -- including a prompt raised by the parent workspace behind
// it. Answering reached only the thread's own permission service, which is
// not holding that request, so the prompt could never be answered or
// dismissed.
func TestAttachedThreadWorkspace_FallsBackToTheParentForPermissions(t *testing.T) {
	inner := &permissionStubWorkspace{}
	ws := &attachedThreadWorkspace{Workspace: inner}

	require.False(t, ws.PermissionGrant(permission.PermissionRequest{ID: "req"}),
		"with no parent to fall back to, the thread's own answer stands")
	require.Equal(t, []string{"grant"}, inner.calls)
}

// The thread's own workspace is asked first: a prompt raised inside the
// thread is the common case, and a service that is not holding the request
// does nothing, so asking is cheap but not free.
func TestAttachedThreadWorkspace_AsksTheThreadFirst(t *testing.T) {
	inner := &permissionStubWorkspace{accept: true}
	ws := &attachedThreadWorkspace{Workspace: inner}

	require.True(t, ws.PermissionGrant(permission.PermissionRequest{ID: "req"}))
	require.Equal(t, []string{"grant"}, inner.calls)
}

// Deny and the persistent grant travel the same path as a plain grant.
func TestAttachedThreadWorkspace_RoutesEveryPermissionAnswer(t *testing.T) {
	inner := &permissionStubWorkspace{accept: true}
	ws := &attachedThreadWorkspace{Workspace: inner}

	require.True(t, ws.PermissionGrantPersistent(permission.PermissionRequest{ID: "req"}))
	require.True(t, ws.PermissionDeny(permission.PermissionRequest{ID: "req"}))
	require.Equal(t, []string{"grant-persistent", "deny"}, inner.calls)
}

// answerPermission stops at the first acceptance and skips nil attempts
// (there is no parent to fall back to when a thread is attached without
// one).
func TestAnswerPermission_StopsAtTheFirstAcceptance(t *testing.T) {
	var ran []string
	accepted := answerPermission(
		nil,
		func() bool { ran = append(ran, "first"); return false },
		func() bool { ran = append(ran, "second"); return true },
		func() bool { ran = append(ran, "third"); return true },
	)

	require.True(t, accepted)
	require.Equal(t, []string{"first", "second"}, ran)
}

func TestAnswerPermission_ReportsNoAcceptance(t *testing.T) {
	require.False(t, answerPermission(func() bool { return false }))
	require.False(t, answerPermission())
}

// permissionStubWorkspace records which permission answer was asked of it.
type permissionStubWorkspace struct {
	Workspace
	calls  []string
	accept bool
}

func (s *permissionStubWorkspace) PermissionGrant(permission.PermissionRequest) bool {
	s.calls = append(s.calls, "grant")
	return s.accept
}

func (s *permissionStubWorkspace) PermissionGrantPersistent(permission.PermissionRequest) bool {
	s.calls = append(s.calls, "grant-persistent")
	return s.accept
}

func (s *permissionStubWorkspace) PermissionDeny(permission.PermissionRequest) bool {
	s.calls = append(s.calls, "deny")
	return s.accept
}
