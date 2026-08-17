package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// An embedded thread UI that already knows its session opens in the chat
// frame, not on the landing screen.
//
// The router switches to this UI the moment AttachThread returns, which is
// well before the session's messages have been read. Landing first meant
// clicking a thread painted a full logo-and-status screen for the length
// of the load — a visible flash on the way in.
func TestNewEmbedded_WithSessionOpensInChat(t *testing.T) {
	ws := &cmdDrivingWorkspace{
		agentReady:          true,
		sessionsBySessionID: map[string]session.Session{"sess": {ID: "sess", Title: "T"}},
	}

	ui := New(common.DefaultCommon(context.Background(), ws), "sess", false, WithEmbedded())

	require.Equal(t, uiChat, ui.state, "a thread being opened must not flash the landing screen")
	require.False(t, ui.hasSession(), "the session has not loaded yet; the frame just waits for it")
}

// With no session to open, landing is still the right destination: there
// is nothing on the way for a chat frame to be waiting for.
func TestNewEmbedded_WithoutSessionOpensOnLanding(t *testing.T) {
	ws := &cmdDrivingWorkspace{agentReady: true}

	ui := New(common.DefaultCommon(context.Background(), ws), "", false, WithEmbedded())

	require.Equal(t, uiLanding, ui.state)
}
