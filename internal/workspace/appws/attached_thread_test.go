package appws

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// The wrapper AttachThread returns must still deliver events through
// SubscribeWith. It embeds the Workspace interface, which now carries
// SubscribeWith directly (no type assertion needed, and no gap for an
// implementation to silently fall through — see workspace.EventSubscriber's
// doc comment for the bug that used to cause). This asserts the forward is
// real, not just present: the send callback identity must reach the
// wrapped workspace, and stop must reach its stopped flag — a wrapper that
// swallows both (returning an unconnected no-op) would pass a weaker
// "stop is non-nil" check but not this one.
func TestAttachedThreadWorkspace_DeliversEventsThroughSubscribeWith(t *testing.T) {
	inner := &subscribeStubWorkspace{}
	var ws workspace.Workspace = &attachedThreadWorkspace{Workspace: inner}

	var received any
	stop := ws.SubscribeWith(func(m any) { received = m })
	require.NotNil(t, stop)
	require.NotNil(t, inner.send, "SubscribeWith must forward the send callback to the wrapped workspace")

	inner.send("hello")
	require.Equal(t, "hello", received, "an event sent through the wrapped workspace must reach the caller's callback")

	stop()
	require.True(t, inner.stopped, "stop must reach the wrapped workspace's own stop")
}

// subscribeStubWorkspace is a Workspace that can subscribe, standing in
// for the thread's own AppWorkspace.
type subscribeStubWorkspace struct {
	workspace.Workspace
	send    func(any)
	stopped bool
}

func (s *subscribeStubWorkspace) SubscribeWith(send func(any)) func() {
	s.send = send
	return func() { s.stopped = true }
}

func TestAttachedThreadWorkspace_PreparesSessionChanges(t *testing.T) {
	inner := &sessionChangeStubWorkspace{}
	ws := &attachedThreadWorkspace{Workspace: inner, sessionID: "thread-session"}

	files, err := ws.PrepareSessionChanges(t.Context(), "thread-session")
	require.NoError(t, err)
	require.Equal(t, []workspace.SessionFile{{FirstVersion: history.File{Path: "thread-session"}}}, files)
	require.Equal(t, []string{"thread-session"}, inner.sessions)
}

// TestAttachedThreadWorkspace_SessionChangesUnavailable used to pin the
// fallback for a wrapped workspace that did not implement the separate,
// optional workspace.SessionChangePreparer interface (plainStubWorkspace,
// embedding a nil workspace.Workspace, was exactly such a workspace).
// PR 0.7c's review folded PrepareSessionChanges into workspace.Workspace
// itself (FileServices), so every Workspace now statically implements it
// -- there is no longer a workspace this fallback could ever see, and
// plainStubWorkspace's nil embedded field would instead panic on the
// promoted call, same as any other unimplemented method on a minimal
// stub. Removed along with plainStubWorkspace, which existed only for
// this test.

type sessionChangeStubWorkspace struct {
	workspace.Workspace
	sessions []string
}

func (s *sessionChangeStubWorkspace) PrepareSessionChanges(_ context.Context, sessionID string) ([]workspace.SessionFile, error) {
	s.sessions = append(s.sessions, sessionID)
	return []workspace.SessionFile{{FirstVersion: history.File{Path: sessionID}}}, nil
}

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
	workspace.Workspace
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

	accepted, err := ws.PermissionGrant(permission.PermissionRequest{ID: "req"})
	require.NoError(t, err)
	require.False(t, accepted,
		"with no parent to fall back to, the thread's own answer stands")
	require.Equal(t, []string{"grant"}, inner.calls)
}

// The thread's own workspace is asked first: a prompt raised inside the
// thread is the common case, and a service that is not holding the request
// does nothing, so asking is cheap but not free.
func TestAttachedThreadWorkspace_AsksTheThreadFirst(t *testing.T) {
	inner := &permissionStubWorkspace{accept: true}
	ws := &attachedThreadWorkspace{Workspace: inner}

	accepted, err := ws.PermissionGrant(permission.PermissionRequest{ID: "req"})
	require.NoError(t, err)
	require.True(t, accepted)
	require.Equal(t, []string{"grant"}, inner.calls)
}

// Deny and the persistent grant travel the same path as a plain grant.
func TestAttachedThreadWorkspace_RoutesEveryPermissionAnswer(t *testing.T) {
	inner := &permissionStubWorkspace{accept: true}
	ws := &attachedThreadWorkspace{Workspace: inner}

	acceptedPersistent, err := ws.PermissionGrantPersistent(permission.PermissionRequest{ID: "req"})
	require.NoError(t, err)
	require.True(t, acceptedPersistent)
	deniedAccepted, err := ws.PermissionDeny(permission.PermissionRequest{ID: "req"})
	require.NoError(t, err)
	require.True(t, deniedAccepted)
	require.Equal(t, []string{"grant-persistent", "deny"}, inner.calls)
}

// answerPermission stops at the first acceptance and skips nil attempts
// (there is no parent to fall back to when a thread is attached without
// one).
func TestAnswerPermission_StopsAtTheFirstAcceptance(t *testing.T) {
	var ran []string
	accepted, err := answerPermission(
		nil,
		func() (bool, error) { ran = append(ran, "first"); return false, nil },
		func() (bool, error) { ran = append(ran, "second"); return true, nil },
		func() (bool, error) { ran = append(ran, "third"); return true, nil },
	)

	require.NoError(t, err)
	require.True(t, accepted)
	require.Equal(t, []string{"first", "second"}, ran)
}

func TestAnswerPermission_ReportsNoAcceptance(t *testing.T) {
	accepted, err := answerPermission(func() (bool, error) { return false, nil })
	require.NoError(t, err)
	require.False(t, accepted)

	accepted, err = answerPermission()
	require.NoError(t, err)
	require.False(t, accepted)
}

// TestAnswerPermission_JoinsErrorsWhenNoneResolves pins the combinator's
// error semantics: an attempt that itself fails (the call could not be
// carried out) neither wins nor is silently dropped — if nothing resolves
// the request, every such error is joined into the result.
func TestAnswerPermission_JoinsErrorsWhenNoneResolves(t *testing.T) {
	errFirst := errors.New("first failed")
	errSecond := errors.New("second failed")

	accepted, err := answerPermission(
		func() (bool, error) { return false, errFirst },
		func() (bool, error) { return false, nil },
		func() (bool, error) { return false, errSecond },
	)

	require.False(t, accepted)
	require.ErrorIs(t, err, errFirst)
	require.ErrorIs(t, err, errSecond)
}

// permissionStubWorkspace records which permission answer was asked of it.
type permissionStubWorkspace struct {
	workspace.Workspace
	calls  []string
	accept bool
}

func (s *permissionStubWorkspace) PermissionGrant(permission.PermissionRequest) (bool, error) {
	s.calls = append(s.calls, "grant")
	return s.accept, nil
}

func (s *permissionStubWorkspace) PermissionGrantPersistent(permission.PermissionRequest) (bool, error) {
	s.calls = append(s.calls, "grant-persistent")
	return s.accept, nil
}

func (s *permissionStubWorkspace) PermissionDeny(permission.PermissionRequest) (bool, error) {
	s.calls = append(s.calls, "deny")
	return s.accept, nil
}
