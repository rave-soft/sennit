package grpcws

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/workspace"
)

// This file white-box tests dispatch/applyLocked/attachSubscriber/
// detachSubscriber directly, on a bare *Client with no server behind it --
// the pieces of CLIENT-SERVER.md's PR 1.4b that are easiest to get wrong
// (the stale-Version guard, replay-then-don't-replay-again) are cheapest
// to pin down at this level, without a bufconn round trip.

// TestClient_ApplyLocked_StaleClientStateIsIgnored checks the idempotency
// guarantee workspace.ClientState's own doc comment describes: a Version
// not strictly greater than the cached one changes nothing, whether it's
// equal (a duplicate delivery) or older (a reordered replay).
func TestClient_ApplyLocked_StaleClientStateIsIgnored(t *testing.T) {
	c := &Client{}
	c.state = workspace.ClientState{Version: 5, WorkingDir: "/five"}

	c.dispatch(pubsub.Event[workspace.ClientState]{Type: pubsub.UpdatedEvent, Payload: workspace.ClientState{Version: 5, WorkingDir: "/stale-same-version"}})
	require.Equal(t, "/five", c.cachedState().WorkingDir, "an equal Version must be ignored")

	c.dispatch(pubsub.Event[workspace.ClientState]{Type: pubsub.UpdatedEvent, Payload: workspace.ClientState{Version: 4, WorkingDir: "/older"}})
	require.Equal(t, "/five", c.cachedState().WorkingDir, "an older Version must be ignored")

	c.dispatch(pubsub.Event[workspace.ClientState]{Type: pubsub.UpdatedEvent, Payload: workspace.ClientState{Version: 6, WorkingDir: "/six"}})
	require.Equal(t, "/six", c.cachedState().WorkingDir, "a strictly newer Version must be applied")
}

// TestClient_AttachSubscriber_ReplaysPendingThenNotAfterResolved checks
// the request-replay half of PR 1.4b's build step 2: a subscriber
// attaching while a permission request is pending sees it immediately, but
// a subscriber attaching after that request's PermissionNotification does
// not see it at all -- it has already been resolved.
func TestClient_AttachSubscriber_ReplaysPendingThenNotAfterResolved(t *testing.T) {
	c := &Client{}
	perm := permission.PermissionRequest{ID: "perm-1", SessionID: "sess-1", ToolCallID: "call-1"}
	c.dispatch(pubsub.Event[permission.PermissionRequest]{Type: pubsub.CreatedEvent, Payload: perm})

	var got []any
	sub := &clientSub{send: func(v any) { got = append(got, v) }}
	c.attachSubscriber(sub)
	require.Len(t, got, 1)
	e, ok := got[0].(pubsub.Event[permission.PermissionRequest])
	require.True(t, ok)
	require.Equal(t, perm, e.Payload)
	c.detachSubscriber(sub)

	c.dispatch(pubsub.Event[permission.PermissionNotification]{Type: pubsub.CreatedEvent, Payload: permission.PermissionNotification{ToolCallID: "call-1", Granted: true}})

	var got2 []any
	sub2 := &clientSub{send: func(v any) { got2 = append(got2, v) }}
	c.attachSubscriber(sub2)
	require.Empty(t, got2, "a resolved request must not be replayed to a newly attached subscriber")
}

// TestClient_Dispatch_FansOutToAllSubscribersIndependently checks that
// every attached subscriber gets every dispatched event, and that
// detaching one has no effect on any other -- SubscribeWith's stop func
// must only ever affect its own subscription.
func TestClient_Dispatch_FansOutToAllSubscribersIndependently(t *testing.T) {
	c := &Client{}
	var gotA, gotB []any
	subA := &clientSub{send: func(v any) { gotA = append(gotA, v) }}
	subB := &clientSub{send: func(v any) { gotB = append(gotB, v) }}
	c.attachSubscriber(subA)
	c.attachSubscriber(subB)

	c.dispatch("event-1")
	require.Equal(t, []any{"event-1"}, gotA)
	require.Equal(t, []any{"event-1"}, gotB)

	c.detachSubscriber(subA)
	c.dispatch("event-2")
	require.Equal(t, []any{"event-1"}, gotA, "a detached subscriber must not receive further events")
	require.Equal(t, []any{"event-1", "event-2"}, gotB)
}
