package workspace

import (
	"errors"
	"testing"

	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestAgentNotificationTranslationMatchesLocalAndRemote(t *testing.T) {
	t.Parallel()

	want := pubsub.Event[AgentNotification]{
		Type: pubsub.UpdatedEvent,
		Payload: AgentNotification{
			SessionID:    "session-1",
			SessionTitle: "Session",
			Type:         AgentNotificationAWSSSOAuth,
			ProviderID:   "bedrock",
			RunID:        "run-1",
			Message:      "refreshing credentials",
			AWSSOCommand: "aws sso login",
			AWSSOURL:     "https://example.com/device",
		},
	}

	local := (&AppWorkspace{}).translateEvent(pubsub.Event[notify.Notification]{
		Type: pubsub.UpdatedEvent,
		Payload: notify.Notification{
			SessionID:    want.Payload.SessionID,
			SessionTitle: want.Payload.SessionTitle,
			Type:         notify.Type(want.Payload.Type),
			ProviderID:   want.Payload.ProviderID,
			RunID:        want.Payload.RunID,
			Message:      want.Payload.Message,
			AWSSOCommand: want.Payload.AWSSOCommand,
			AWSSOURL:     want.Payload.AWSSOURL,
		},
	})

	remote := NewClientWorkspace(nil, proto.Workspace{}).translateEvent(pubsub.Event[proto.AgentEvent]{
		Type: pubsub.UpdatedEvent,
		Payload: proto.AgentEvent{
			SessionID:    want.Payload.SessionID,
			SessionTitle: want.Payload.SessionTitle,
			Type:         proto.AgentEventType(want.Payload.Type),
			ProviderID:   want.Payload.ProviderID,
			RunID:        want.Payload.RunID,
			Error:        errors.New(want.Payload.Message),
			AWSSOCommand: want.Payload.AWSSOCommand,
			AWSSOURL:     want.Payload.AWSSOURL,
		},
	})

	require.Equal(t, want, local)
	require.Equal(t, want, remote)
}
