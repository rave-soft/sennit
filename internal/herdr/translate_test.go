package herdr

import (
	"testing"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Domain type translation.

func TestTranslateDomainAssistantMessage(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[message.Message]{
		Payload: message.Message{Role: message.Assistant, SessionID: "s1"},
	}
	assert.Equal(t, AssistantMessage{SessionID: "s1"}, Translate(ev))
}

func TestTranslateDomainSummaryMessage(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[message.Message]{
		Payload: message.Message{
			Role:             message.Assistant,
			SessionID:        "s1",
			IsSummaryMessage: true,
		},
	}
	assert.Equal(t, Summarizing{}, Translate(ev))
}

func TestTranslateDomainNonAssistantIgnored(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[message.Message]{
		Payload: message.Message{Role: message.System},
	}
	assert.Nil(t, Translate(ev))
}

func TestTranslateDomainRunComplete(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[notify.RunComplete]{
		Payload: notify.RunComplete{SessionID: "s1"},
	}
	assert.Equal(t, RunComplete{SessionID: "s1"}, Translate(ev))
}

func TestTranslateDomainPermissionRequest(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[permission.PermissionRequest]{
		Payload: permission.PermissionRequest{ToolName: "bash"},
	}
	assert.Equal(t, PermissionRequested{}, Translate(ev))
}

func TestTranslateDomainPermissionNotification(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[permission.PermissionNotification]{
		Payload: permission.PermissionNotification{Granted: true},
	}
	assert.Equal(t, PermissionResolved{}, Translate(ev))
}

// Unknown types.

func TestTranslateUnknownReturnsNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, Translate("not an event"))
}

// Frontend event translation (CLIENT-SERVER.md, PR 1.5): the events a
// daemon-mode client actually receives through Workspace.Subscribe,
// rather than the local pub/sub types BridgeLocal subscribes to
// in-process.

func TestTranslateFrontendTurnStarted(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[workspace.AgentNotification]{
		Payload: workspace.AgentNotification{SessionID: "s1", Type: workspace.AgentNotificationTurnStarted},
	}
	assert.Equal(t, AssistantMessage{SessionID: "s1"}, TranslateFrontend(ev))
}

func TestTranslateFrontendFinished(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[workspace.AgentNotification]{
		Payload: workspace.AgentNotification{SessionID: "s1", Type: workspace.AgentNotificationFinished},
	}
	assert.Equal(t, RunComplete{SessionID: "s1"}, TranslateFrontend(ev))
}

func TestTranslateFrontendOtherNotificationIgnored(t *testing.T) {
	t.Parallel()
	ev := pubsub.Event[workspace.AgentNotification]{
		Payload: workspace.AgentNotification{Type: workspace.AgentNotificationError},
	}
	assert.Nil(t, TranslateFrontend(ev))
}

func TestTranslateFrontendDelegatesSharedTypes(t *testing.T) {
	t.Parallel()
	msgEv := pubsub.Event[message.Message]{
		Payload: message.Message{Role: message.Assistant, SessionID: "s1"},
	}
	assert.Equal(t, AssistantMessage{SessionID: "s1"}, TranslateFrontend(msgEv))

	reqEv := pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ToolName: "bash"}}
	assert.Equal(t, PermissionRequested{}, TranslateFrontend(reqEv))

	notifEv := pubsub.Event[permission.PermissionNotification]{Payload: permission.PermissionNotification{Granted: true}}
	assert.Equal(t, PermissionResolved{}, TranslateFrontend(notifEv))
}

// TestTranslateFrontendSequence_MatchesInProcessTransitions drives the
// same lifecycle -- a turn starting, a permission request, its
// resolution, and the turn finishing -- through TranslateFrontend and
// through the in-process Translate, on separate Clients with a fake
// (non-socket) sender, and asserts both report the identical state
// sequence. Dropping the permission mapping from TranslateFrontend (or
// from Translate) is exactly the kind of regression this pins: either
// side skipping stateBlocked would desync it from the other, since
// production runs whichever one applies to how Sennit is wired for that
// pane, and both must mean the same thing to herdr.
func TestTranslateFrontendSequence_MatchesInProcessTransitions(t *testing.T) {
	t.Parallel()

	frontendEvents := []any{
		pubsub.Event[workspace.AgentNotification]{
			Payload: workspace.AgentNotification{SessionID: "s1", Type: workspace.AgentNotificationTurnStarted},
		},
		pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ToolName: "bash"}},
		pubsub.Event[permission.PermissionNotification]{Payload: permission.PermissionNotification{Granted: true}},
		pubsub.Event[workspace.AgentNotification]{
			Payload: workspace.AgentNotification{SessionID: "s1", Type: workspace.AgentNotificationFinished},
		},
	}
	domainEvents := []any{
		pubsub.Event[message.Message]{Payload: message.Message{Role: message.Assistant, SessionID: "s1"}},
		pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ToolName: "bash"}},
		pubsub.Event[permission.PermissionNotification]{Payload: permission.PermissionNotification{Granted: true}},
		pubsub.Event[notify.RunComplete]{Payload: notify.RunComplete{SessionID: "s1"}},
	}

	frontendClient := newTestClient()
	for _, ev := range frontendEvents {
		hev := TranslateFrontend(ev)
		require.NotNil(t, hev)
		frontendClient.HandleEvent(hev)
	}

	domainClient := newTestClient()
	for _, ev := range domainEvents {
		hev := Translate(ev)
		require.NotNil(t, hev)
		domainClient.HandleEvent(hev)
	}

	assert.Equal(t, reportedStates(domainClient), reportedStates(frontendClient))
	assert.Equal(t, []string{stateWorking, stateBlocked, stateWorking, stateIdle}, reportedStates(frontendClient))
}
