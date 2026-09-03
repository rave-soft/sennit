package dialog

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// renameTrackingWorkspace records the rename the dialog performed and the
// arguments it used.
type renameTrackingWorkspace struct {
	workspace.Workspace
	renamedID    string
	renamedTitle string
	renameCalled bool
}

func (w *renameTrackingWorkspace) RenameSession(_ context.Context, sessionID, title string) error {
	w.renameCalled = true
	w.renamedID = sessionID
	w.renamedTitle = title
	return nil
}

// TestSessionRename_WritesOnlyTheTitle pins what a confirmed rename is
// allowed to persist: the title, through Workspace.RenameSession. The
// dialog's session list is a snapshot taken once when it opens, and the
// dialog never subscribes to session updates, so by the time a rename is
// confirmed that row can be stale. It used to be written back whole,
// which silently rolled back cost, todos and summary_message_id that
// other writers (a finishing turn, the todo tool, auto-summarization) had
// set while the dialog sat open. The whole-row method is gone from the
// contract now; this test guards the call the dialog makes in its place.
func TestSessionRename_WritesOnlyTheTitle(t *testing.T) {
	com := newCommandsNamesTestCommon(t)
	ws := &renameTrackingWorkspace{}
	com.Workspace = ws

	stale := session.Session{
		ID:               "sess-1",
		Title:            "Old Title",
		Cost:             12.5,
		SummaryMessageID: "summary-1",
		Todos:            []session.Todo{{Content: "do the thing", Status: session.TodoStatusPending}},
	}
	s := NewSessions(com, []session.Session{stale}, "sess-1")

	s.HandleMsg(tea.KeyPressMsg{Text: "ctrl+r"})
	require.Equal(t, sessionsModeUpdating, s.sessionsMode)

	item := s.selectedSessionItem()
	require.NotNil(t, item)
	for _, r := range "New Title" {
		s.HandleMsg(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	require.Equal(t, "New Title", item.InputValue())

	action := s.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "confirming a rename must return an async ActionCmd")
	require.NotNil(t, cmdAction.Cmd)
	cmdAction.Cmd()

	require.True(t, ws.renameCalled, "rename must go through Workspace.RenameSession")
	require.Equal(t, "sess-1", ws.renamedID)
	require.Equal(t, "New Title", ws.renamedTitle)
}
