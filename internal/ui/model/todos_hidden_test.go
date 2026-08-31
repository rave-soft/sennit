package model

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// newTestTodosToolItem builds a finished todos tool call/result pair with a
// small mixed-status list, the same shape a real turn produces.
func newTestTodosToolItem(t *testing.T, u *UI) chat.ToolMessageItem {
	t.Helper()

	meta := tools.TodosResponseMetadata{
		IsNew:     true,
		Completed: 0,
		Total:     2,
		Todos: []session.Todo{
			{Content: "write the plan", Status: session.TodoStatusInProgress, ActiveForm: "Writing the plan"},
			{Content: "ship it", Status: session.TodoStatusPending},
		},
	}
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)

	toolCall := message.ToolCall{ID: "tc-todos", Name: tools.TodosToolName, Input: `{"todos":[]}`, Finished: true}
	result := &message.ToolResult{ToolCallID: toolCall.ID, Metadata: string(metaJSON)}

	return chat.NewToolMessageItem(u.com.Styles, "m-todos", toolCall, result, false, nil)
}

// TestChatSetTodosHidden_HidesAndRestoresTranscriptRow covers the
// panel/chat handoff: while the session panel is showing a session's live
// todos (any todo incomplete), the chat transcript's own todos tool call
// must draw nothing at all, so the list is neither duplicated on screen
// nor announced by a one-line stub between the messages that carry the
// work. Once every todo is completed - the panel disappears, see
// TestSessionPanelPlan_PanelHidesOnceAllTodosCompleted - the transcript
// must go back to showing the full list, since it's now the only place
// left recording it.
func TestChatSetTodosHidden_HidesAndRestoresTranscriptRow(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.chat.SetSize(80, 40)
	item := newTestTodosToolItem(t, u)
	u.chat.SetMessages(item)

	u.chat.SetTodosHidden(true)
	// Empty, not merely list-free: a hidden row must not leave the
	// prefixed blank line prefixLines would make of an empty body, and
	// must not leave the header stub the compact path used to render.
	require.Empty(t, item.Render(80), "the panel is showing this list: the transcript row must draw nothing")

	u.chat.SetTodosHidden(false)
	full := item.Render(80)
	require.Contains(t, full, "To-Do")
	require.Contains(t, full, "Writing the plan", "once the panel is gone the transcript must show the full list again")
	require.Contains(t, full, "ship it")
}

// TestUpdate_SessionEvent_SyncsTodosHiddenWithPanelVisibility is an
// end-to-end check that the pubsub session-update handler actually wires
// SetTodosHidden to hasIncompleteTodos, not just that the method itself
// works in isolation.
func TestUpdate_SessionEvent_SyncsTodosHiddenWithPanelVisibility(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.chat.SetSize(80, 40)
	item := newTestTodosToolItem(t, u)
	u.chat.SetMessages(item)
	u.sess.current.Todos = []session.Todo{
		{Content: "write the plan", Status: session.TodoStatusInProgress},
		{Content: "ship it", Status: session.TodoStatusPending},
	}

	u.Update(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: *u.sess.current})
	require.Empty(t, item.Render(80), "panel is showing incomplete todos: transcript row must draw nothing")

	completed := *u.sess.current
	completed.Todos = []session.Todo{
		{Content: "write the plan", Status: session.TodoStatusCompleted},
		{Content: "ship it", Status: session.TodoStatusCompleted},
	}
	u.Update(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: completed})
	require.Contains(t, item.Render(80), "ship it", "panel disappeared: transcript must show the full list again")
}

// TestUpdate_SessionEvent_NewTodosListForcesExpand covers the "default
// expand on a brand new list" rule: a 0 -> N todos transition on the
// current session must force m.panel.expanded = true unconditionally —
// distinct from autoExpandTodosIfReasonable's gentler, terminal-height-
// gated, once-per-session nicety. A later update to the same (now
// non-empty) list must NOT re-force it open — a user's manual collapse in
// between must stick until the next 0 -> N transition.
func TestUpdate_SessionEvent_NewTodosListForcesExpand(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.lay.height = 10 // Below autoExpandTodosIfReasonable's height gate, on purpose.
	require.False(t, u.panel.expanded)

	u.Update(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: session.Session{
		ID: u.sess.current.ID,
		Todos: []session.Todo{
			{Content: "write the plan", Status: session.TodoStatusPending},
		},
	}})
	require.True(t, u.panel.expanded, "a brand new list must force the panel open even on a short terminal")

	// A manual collapse must stick across further updates to the same list.
	u.panel.expanded = false
	u.Update(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: session.Session{
		ID: u.sess.current.ID,
		Todos: []session.Todo{
			{Content: "write the plan", Status: session.TodoStatusPending},
			{Content: "ship it", Status: session.TodoStatusPending},
		},
	}})
	require.False(t, u.panel.expanded, "growing an already-non-empty list must not re-force it open")
}
