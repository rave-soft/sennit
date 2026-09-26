package model

// Tests for PR 1.4c (CLIENT-SERVER.md): the UI's handling of
// pubsub.Event[workspace.ConnectionEvent]. See update_connection.go.

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
)

// connectionEvent builds the pubsub.Event[workspace.ConnectionEvent] shape
// grpcws.connectionEvent produces (client_manual.go), so a test drives
// exactly what the real client delivers.
func connectionEvent(state workspace.ConnectionState) pubsub.Event[workspace.ConnectionEvent] {
	return pubsub.Event[workspace.ConnectionEvent]{Type: pubsub.UpdatedEvent, Payload: workspace.ConnectionEvent{State: state}}
}

func renderHeaderLine(u *UI) string {
	canvas := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.Draw(canvas, canvas.Bounds())
	return ansi.Strip(canvas.Render())
}

// TestConnectionLostShowsHeaderIndicatorRecoveredClears pins the
// "persistent, unobtrusive indicator" requirement: Lost must show
// something that stays up (unlike Status's TTL banner), and Recovered
// clears it.
func TestConnectionLostShowsHeaderIndicatorRecoveredClears(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	require.False(t, u.conn.lost)
	require.NotContains(t, renderHeaderLine(u), "reconnecting")

	u.Update(connectionEvent(workspace.ConnectionLost))
	require.True(t, u.conn.lost)
	require.Contains(t, renderHeaderLine(u), "reconnecting",
		"a lost connection must show a persistent indicator in the header")

	u.Update(connectionEvent(workspace.ConnectionRecovered))
	require.False(t, u.conn.lost)
	require.NotContains(t, renderHeaderLine(u), "reconnecting",
		"Recovered must clear the indicator")
}

// TestConnectionResyncClearsIndicatorAndReloadsState drives a full Resync
// through a guarded cmdDrivingWorkspace: the update-goroutine guard
// (wsguard_test.go) panics if any reload here runs synchronously in
// Update rather than inside the returned tea.Cmd tree, which is what
// backs the "UI guard stays green" acceptance criterion. It also checks
// every cache CLIENT-SERVER.md's PR 1.4c build step lists: the session
// reload, and the delegation/task list invalidation (both must happen
// synchronously in Update, since Invalidate itself is not IO), then runs
// the returned commands and checks the session and MCP state actually
// got refetched.
func TestConnectionResyncClearsIndicatorAndReloadsState(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	ws.sessionsBySessionID = map[string]session.Session{"s1": {ID: "s1"}}
	ws.messagesBySessionID = map[string][]message.Message{"s1": {}}
	m := newCmdDrivenUI(t, ws)
	warmCmdDrivenCaches(m)
	m.threadList.Cache.Set(nil)
	m.agentList.cache.Set(nil)
	m.conn.lost = true

	_, cmd := m.Update(connectionEvent(workspace.ConnectionResync))

	require.False(t, m.conn.lost, "Resync must clear the lost indicator")
	require.True(t, m.threadList.Cache.Timestamp.IsZero(),
		"Resync must invalidate the all-delegation list cache")
	require.True(t, m.agentList.cache.Timestamp.IsZero(),
		"Resync must invalidate the per-session task list cache")

	runCmdTree(m, cmd, nil)

	require.Positive(t, ws.getSessionCalls, "Resync must reload the current session")
	require.Positive(t, ws.agentUpdateModelCalls, "Resync must refresh MCP/agent-model state")
}

// TestConnectionResyncReopensSessionsDialogIfOpen covers the sessions list:
// unlike the TTL caches, there is no live-refresh path for an already-open
// sessions dialog, so Resync has to close and reopen it to pull a fresh
// list.
func TestConnectionResyncReopensSessionsDialogIfOpen(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	ws.sessionsBySessionID = map[string]session.Session{"s1": {ID: "s1"}}
	ws.messagesBySessionID = map[string][]message.Message{"s1": {}}
	ws.listSessions = []session.Session{{ID: "s1"}}
	m := newCmdDrivenUI(t, ws)
	warmCmdDrivenCaches(m)

	runCmdTree(m, m.openSessionsDialog(), nil)
	require.True(t, m.dialog.ContainsDialog(dialog.SessionsID))

	_, cmd := m.Update(connectionEvent(workspace.ConnectionResync))
	// openSessionsDialog only opens the dialog once its fetch lands, so
	// closing it here (done synchronously in Update, not IO) is expected;
	// running the returned commands should bring it back.
	runCmdTree(m, cmd, nil)

	require.True(t, m.dialog.ContainsDialog(dialog.SessionsID),
		"a re-opened sessions dialog must land once the Resync-triggered fetch completes")
}

// TestConnectionResyncDedupesPendingPermission covers the redelivery this
// task calls out: a Resync replays every pending permission request as an
// ordinary CreatedEvent (see grpcws.Client.resync / deliverPending), so one
// already shown here must not open a second dialog or reset the answer
// lifecycle a person may already be acting on.
func TestConnectionResyncDedupesPendingPermission(t *testing.T) {
	t.Parallel()

	u := newTestUIForOpeningPermissions(t)
	perm := permission.PermissionRequest{ID: "perm-1", ToolCallID: "tool-call-1", ToolName: "bash"}

	require.Nil(t, u.openPermissionsDialog(perm))
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))
	firstGen := u.permissionResponse.generation
	firstDialog := u.dialog.Dialog(dialog.PermissionsID)

	// Resync re-delivers the same pending request; openPermissionsDialog
	// (dialogs.go) is what applyPromptRequest (update_prompts.go) calls for
	// it, exactly as it would for the first delivery.
	require.Nil(t, u.openPermissionsDialog(perm))

	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))
	require.Equal(t, firstGen, u.permissionResponse.generation,
		"a re-delivered request already open must not start a new answer lifecycle")
	require.Same(t, firstDialog, u.dialog.Dialog(dialog.PermissionsID),
		"a re-delivered request already open must not replace the dialog instance")
}

// TestMergeReloadedMessageItemsKeepsNewerLiveUpdate is the reload-regression
// case: a live Updated event can land before a session reload's own fetch
// resolves (the two race across a Resync — see updateConnection's
// resyncCmds), and the reload must not clobber it with the stale copy it
// fetched. UpdatedAt is what decides it, since a freshly built item's
// list.Versioned counter starts at zero regardless of how many times the
// live item it is replacing was mutated (see Timestamped's doc comment).
func TestMergeReloadedMessageItemsKeepsNewerLiveUpdate(t *testing.T) {
	t.Parallel()

	m := newBusyUI(&countingWorkspace{ready: true})
	base := message.Message{
		ID: "a1", Role: message.Assistant, SessionID: "s1",
		UpdatedAt: 100,
		Parts:     []message.ContentPart{message.TextContent{Text: "old"}},
	}
	m.chat.SetMessages(chat.NewAssistantMessageItem(m.com.Styles, &base))

	newer := base
	newer.UpdatedAt = 200
	newer.Parts = []message.ContentPart{message.TextContent{Text: "new"}}
	m.applyMessageEvent(pubsub.Event[message.Message]{Type: pubsub.UpdatedEvent, Payload: newer}, nil)

	item, ok := m.chat.MessageItem("a1").(*chat.AssistantMessageItem)
	require.True(t, ok)
	require.Equal(t, int64(200), item.UpdatedAt(), "the live update must have applied")

	// A reload lands afterward, built from a fetch that started before the
	// live update above — its copy of "a1" is the stale one.
	stale := base
	staleItem := []chat.MessageItem{chat.NewAssistantMessageItem(m.com.Styles, &stale)}
	m.applySessionMessageItems(staleItem, 0)

	item, ok = m.chat.MessageItem("a1").(*chat.AssistantMessageItem)
	require.True(t, ok)
	require.Equal(t, int64(200), item.UpdatedAt(),
		"a stale reloaded item must not clobber a newer live update")
	require.True(t, strings.Contains(item.RawRender(80), "new"),
		"the newer content must still be what's rendered")

	// The mirror case: a reload strictly newer than what's displayed must
	// still apply -- the guard only protects against going backwards.
	newest := base
	newest.UpdatedAt = 300
	newest.Parts = []message.ContentPart{message.TextContent{Text: "newest"}}
	newestItem := []chat.MessageItem{chat.NewAssistantMessageItem(m.com.Styles, &newest)}
	m.applySessionMessageItems(newestItem, 0)

	item, ok = m.chat.MessageItem("a1").(*chat.AssistantMessageItem)
	require.True(t, ok)
	require.Equal(t, int64(300), item.UpdatedAt(),
		"a reload strictly newer than the displayed item must still apply")
}

// finishedAssistantMessage returns a message.Message with a Finish part
// (text non-empty, so AssistantMessageItem.Finished() is not also held
// false by isSpinning's own thinking/tool-call checks).
func finishedAssistantMessage(text string) message.Message {
	return message.Message{
		ID: "a1", Role: message.Assistant, SessionID: "s1", UpdatedAt: 100,
		Parts: []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	}
}

// unfinishedAssistantMessage is finishedAssistantMessage without the
// Finish part -- still streaming.
func unfinishedAssistantMessage(text string) message.Message {
	return message.Message{
		ID: "a1", Role: message.Assistant, SessionID: "s1", UpdatedAt: 100,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}
}

// TestMergeReloadedMessageItemsTieBreak covers the coordinator's round-1
// finding: UpdatedAt is second granularity, but a streaming assistant
// message updates many times within the same second. A Resync reload can
// therefore tie with a live update on UpdatedAt while one side is
// meaningfully ahead -- most concretely, the live stream delivering the
// turn's own Finish part in the same second a slightly-earlier-started
// reload fetch resolves without it. Finished breaks the tie (see
// Timestamped's doc comment); an equal Finished state falls back to
// preferring the existing (live) item.
func TestMergeReloadedMessageItemsTieBreak(t *testing.T) {
	t.Parallel()

	t.Run("finished live beats unfinished reload", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		live := finishedAssistantMessage("done")
		m.chat.SetMessages(chat.NewAssistantMessageItem(m.com.Styles, &live))

		reloaded := unfinishedAssistantMessage("still typing")
		m.applySessionMessageItems([]chat.MessageItem{chat.NewAssistantMessageItem(m.com.Styles, &reloaded)}, 0)

		item, ok := m.chat.MessageItem("a1").(*chat.AssistantMessageItem)
		require.True(t, ok)
		require.True(t, item.Finished(), "the finished live item must win the tie")
		require.True(t, strings.Contains(item.RawRender(80), "done"))
	})

	t.Run("finished reload beats unfinished live", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		live := unfinishedAssistantMessage("still typing")
		m.chat.SetMessages(chat.NewAssistantMessageItem(m.com.Styles, &live))

		reloaded := finishedAssistantMessage("done")
		m.applySessionMessageItems([]chat.MessageItem{chat.NewAssistantMessageItem(m.com.Styles, &reloaded)}, 0)

		item, ok := m.chat.MessageItem("a1").(*chat.AssistantMessageItem)
		require.True(t, ok)
		require.True(t, item.Finished(), "the finished reload must win the tie")
		require.True(t, strings.Contains(item.RawRender(80), "done"))
	})

	t.Run("both unfinished keeps live", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		live := unfinishedAssistantMessage("live text")
		m.chat.SetMessages(chat.NewAssistantMessageItem(m.com.Styles, &live))

		reloaded := unfinishedAssistantMessage("reloaded text")
		m.applySessionMessageItems([]chat.MessageItem{chat.NewAssistantMessageItem(m.com.Styles, &reloaded)}, 0)

		item, ok := m.chat.MessageItem("a1").(*chat.AssistantMessageItem)
		require.True(t, ok)
		require.False(t, item.Finished())
		require.True(t, strings.Contains(item.RawRender(80), "live text"),
			"with neither side finished, the existing live item must win the tie")
	})
}
