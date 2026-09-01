package model

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// taskListingWorkspace is agentSessionWorkspace with the delegation list
// switched on: delegationHasTaskRecord only consults the list for a
// workspace that has one, so a stub that answers "no tasks here" can never
// exercise the gate.
type taskListingWorkspace struct {
	agentSessionWorkspace
}

func (taskListingWorkspace) SupportsTasks() bool { return true }

// newTaskListingUI is newChildSessionTestUI over taskListingWorkspace.
func newTaskListingUI(t *testing.T) *UI {
	t.Helper()
	com := common.DefaultCommon(context.Background(), taskListingWorkspace{})
	u := &UI{
		com:  com,
		goos: "linux",
		widgets: widgets{
			chat:   chatlist.NewChat(com, config.ScrollbarDefault),
			status: NewStatus(com, nil),
		},
	}
	u.sess.current = &session.Session{ID: "parent-session"}
	u.focus = uiFocusEditor
	return u
}

// setTaskList writes tasks through the delegation cache the way a landed
// fetch does — Timestamp included, since an unfetched list deliberately
// decides nothing.
func setTaskList(u *UI, tasks ...proto.Thread) {
	u.agentList.cache.Set(tasks)
}

// TestClickingAnUnstartedDelegationIsNotAClick is the fix for what the
// person kept hitting: a delegation the model has only just asked for is
// on screen, spinner and all, seconds before its run is launched and its
// session written. Opening it there pushed a nav frame, blurred the
// editor, and rolled all of it back on a not-found load — a click whose
// only outcome was a status-bar line. It must now do nothing at all.
func TestClickingAnUnstartedDelegationIsNotAClick(t *testing.T) {
	t.Parallel()

	u := newTaskListingUI(t)
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-new"))
	// The list has been fetched and this delegation is not in it: its run
	// has not been launched.
	setTaskList(u, proto.Thread{SessionID: "msg1$$tc-other"})

	require.Nil(t, u.enterChildSession("msg1", "tc-new"),
		"a delegation with no session behind it must not be opened")
	require.Empty(t, u.sess.navStack, "no nav frame may be pushed for it")
	require.Equal(t, uiFocusEditor, u.focus, "focus must stay where it was")
}

// TestEnteringARunningDelegationStillWorks: the task record naming the
// child session is written once that session exists, so a delegation the
// panel is already carrying opens as before — mid-run, result or not.
func TestEnteringARunningDelegationStillWorks(t *testing.T) {
	t.Parallel()

	u := newTaskListingUI(t)
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-run"))
	setTaskList(u, proto.Thread{SessionID: "msg1$$tc-run"})

	require.NotNil(t, u.enterChildSession("msg1", "tc-run"))
	require.Len(t, u.sess.navStack, 1)
}

// TestEnteringAFinishedDelegationStillWorks: a delegation whose call has
// come back ran by definition, so it opens without the task list being
// consulted at all — which matters because a finished task may well have
// dropped out of that list.
func TestEnteringAFinishedDelegationStillWorks(t *testing.T) {
	t.Parallel()

	u := newTaskListingUI(t)
	done := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-done", Name: "agent", Input: `{}`, Finished: true},
		&message.ToolResult{ToolCallID: "tc-done", Content: "all done"}, false, nil)
	done.SetMessageID("msg1")
	u.chat.AppendMessages(done)
	setTaskList(u) // nothing live

	require.NotNil(t, u.enterChildSession("msg1", "tc-done"))
	require.Len(t, u.sess.navStack, 1)
}

// TestUnfetchedTaskListOpensDelegationsAsBefore: an empty list that has
// never been fetched says nothing about any delegation. Reading it as
// "nothing has started" would make every delegation unopenable until the
// first fetch lands.
func TestUnfetchedTaskListOpensDelegationsAsBefore(t *testing.T) {
	t.Parallel()

	u := newTaskListingUI(t)
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"))
	require.True(t, u.agentList.cache.Timestamp.IsZero(), "precondition: never fetched")

	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.Len(t, u.sess.navStack, 1)
}

// TestDelegationNotInTheLoadedChatOpensAsBefore covers the panel's own
// entry point (enterDelegation) and sibling cycling: neither has a chat
// item to judge, and both point at delegations that demonstrably exist.
func TestDelegationNotInTheLoadedChatOpensAsBefore(t *testing.T) {
	t.Parallel()

	u := newTaskListingUI(t)
	setTaskList(u, proto.Thread{SessionID: "msg1$$tc-elsewhere", CreatedAt: time.Now().Unix()})

	require.NotNil(t, u.enterChildSession("msg1", "tc-absent"))
	require.Len(t, u.sess.navStack, 1)
}

// TestAnUnstartedDelegationDoesNotLookClickable: the block stopped
// opening anything, but it still lit up under the pointer and still
// reported the click as handled, so it read as a control that does
// nothing. refreshDelegationBlocks marks it, and the item refuses both.
func TestAnUnstartedDelegationDoesNotLookClickable(t *testing.T) {
	t.Parallel()

	u := newTaskListingUI(t)
	item := newAgentItem(u.com.Styles, "tc-new")
	u.chat.AppendMessages(item)
	setTaskList(u, proto.Thread{SessionID: "msg1$$tc-other"})

	u.refreshDelegationBlocks()

	require.False(t, item.HoverableAt(chat.MessageLeftPaddingTotal, 1, 80),
		"a delegation with nothing to open must not highlight under the pointer")
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, chat.MessageLeftPaddingTotal, 1),
		"and must not report the click as handled")
}

// TestAStartedDelegationStaysClickable is the other half: the moment its
// task record names a child session, the block is a way in again.
func TestAStartedDelegationStaysClickable(t *testing.T) {
	t.Parallel()

	u := newTaskListingUI(t)
	item := newAgentItem(u.com.Styles, "tc-run")
	u.chat.AppendMessages(item)
	setTaskList(u, proto.Thread{SessionID: "msg1$$tc-other"})
	u.refreshDelegationBlocks()

	setTaskList(u, proto.Thread{SessionID: "msg1$$tc-run"})
	u.refreshDelegationBlocks()

	require.True(t, item.HoverableAt(chat.MessageLeftPaddingTotal, 1, 80))
	require.True(t, item.HandleMouseClick(ansi.MouseLeft, chat.MessageLeftPaddingTotal, 1))
}
