package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
)

// liveDelegation builds one running task row parented to s1 — sessionUI's
// session — the shape ListTasks returns for a delegation in flight.
func liveDelegation(id, sessionID string) proto.Thread {
	return proto.Thread{
		ID:              id,
		Name:            "task-" + id,
		Goal:            "look into the flaky test",
		Kind:            string(proto.ThreadKindTask),
		SessionID:       sessionID,
		ParentSessionID: "s1",
		Status:          string(proto.ThreadStatusRunning),
		CreatedAt:       time.Now().Unix(),
	}
}

// TestSessionDelegations_ScopedToParentAndLive proves the panel's source
// filter: this session's delegations only, live ones only, oldest first.
func TestSessionDelegations_ScopedToParentAndLive(t *testing.T) {
	t.Parallel()

	newer := liveDelegation("d2", "m$$c2")
	newer.CreatedAt += 10
	older := liveDelegation("d1", "m$$c1")

	finished := liveDelegation("d3", "m$$c3")
	finished.Status = string(proto.ThreadStatusCompleted)

	otherSession := liveDelegation("d4", "m$$c4")
	otherSession.ParentSessionID = "s2"

	got := sessionDelegations([]proto.Thread{newer, finished, otherSession, older}, "s1")

	require.Len(t, got, 2, "only this session's live delegations")
	require.Equal(t, "d1", got[0].ID, "oldest first")
	require.Equal(t, "d2", got[1].ID)

	require.Empty(t, sessionDelegations([]proto.Thread{older}, ""),
		"a session with no id of its own owns nothing")
}

// TestSessionDelegations_IdleIsStillLive pins the one status that is easy to
// mistake for finished: an idle delegation has not produced its result yet,
// and dropping its block would say it had.
func TestSessionDelegations_IdleIsStillLive(t *testing.T) {
	t.Parallel()

	idle := liveDelegation("d1", "m$$c1")
	idle.Status = string(proto.ThreadStatusIdle)

	require.Len(t, sessionDelegations([]proto.Thread{idle}, "s1"), 1)
}

// TestAgentListCache_KeepsOnlyTasks proves the event filter that keeps this
// cache and threadListCache from swallowing each other's rows: both kinds
// ride one pubsub stream. Threads (and payloads predating the Kind field)
// must not land here.
func TestAgentListCache_KeepsOnlyTasks(t *testing.T) {
	t.Parallel()

	var c agentListCache
	c.applyEvent(pubsub.Event[proto.Thread]{Type: pubsub.CreatedEvent, Payload: liveDelegation("d1", "m$$c1")})
	require.Len(t, c.cache.value, 1)

	c.applyEvent(pubsub.Event[proto.Thread]{Type: pubsub.CreatedEvent, Payload: proto.Thread{
		ID: "t1", Kind: string(proto.ThreadKindThread), Status: "running",
	}})
	c.applyEvent(pubsub.Event[proto.Thread]{Type: pubsub.CreatedEvent, Payload: proto.Thread{
		ID: "legacy", Status: "running",
	}})
	require.Len(t, c.cache.value, 1, "a thread — and a payload with no kind at all — belongs to the other cache")

	running := liveDelegation("d1", "m$$c1")
	running.Status = string(proto.ThreadStatusCompleted)
	c.applyEvent(pubsub.Event[proto.Thread]{Type: pubsub.UpdatedEvent, Payload: running})
	require.Equal(t, string(proto.ThreadStatusCompleted), c.cache.value[0].Status, "an update rewrites the row in place")

	c.applyEvent(pubsub.Event[proto.Thread]{Type: pubsub.DeletedEvent, Payload: proto.Thread{ID: "d1"}})
	require.Empty(t, c.cache.value, "a removal is never kind-filtered — it would strand the row")
}

// TestSessionPanelPlan_AgentsRows covers the agents section's natural row
// budget: two rows per live delegation plus its header, and nothing at all
// for a session that has delegated nothing.
func TestSessionPanelPlan_AgentsRows(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	require.Zero(t, u.sessionPanelPlan(100).agentsRows, "no delegations at all")

	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "m$$c1"), liveDelegation("d2", "m$$c2")}
	plan := u.sessionPanelPlan(100)
	require.Equal(t, 4, plan.agentsRows)
	require.Equal(t, 1, plan.agentsHeaderRows)
	require.Equal(t, 2, plan.agentsLive)
	require.True(t, plan.agentsExpanded)
}

// TestSessionPanelPlan_AgentsCollapsedKeepsHeader pins what collapsing the
// section means: the blocks go, the header — the only thing left to click to
// get them back, and the only remaining sign that anything is running —
// stays, still reporting the count.
func TestSessionPanelPlan_AgentsCollapsedKeepsHeader(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "m$$c1")}
	u.panel.agentsCollapsed = true

	plan := u.sessionPanelPlan(100)
	require.Zero(t, plan.agentsRows)
	require.Equal(t, 1, plan.agentsHeaderRows)
	require.Equal(t, 1, plan.agentsLive)
	require.False(t, plan.agentsExpanded)
}

// TestDrawSessionPanel_AgentsSection proves the section paints: its header,
// and one block per live delegation carrying the delegation's goal.
func TestDrawSessionPanel_AgentsSection(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "m$$c1")}
	u.updateLayoutAndSize()

	plan := u.sessionPanelPlan(u.lay.layout.panel.Dy())
	scr := uv.NewScreenBuffer(u.lay.width, plan.totalRows)
	area := uv.Rectangle{Max: uv.Position{X: u.lay.width, Y: plan.totalRows}}
	u.drawSessionPanel(scr, area)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "agents 1")
	require.Contains(t, out, "look into the flaky test")
}

// agentsPanelWorkspace implements the two id helpers the agents section
// calls synchronously — resolving a block's name and turning its child
// session id back into the tool call that started it — on top of the nil
// workspace.Workspace embed. Every other method panics if called, so the
// click it serves must return before Update reaches its workspace-probing
// tail (it does: a handled panel click returns immediately).
type agentsPanelWorkspace struct {
	workspace.Workspace
}

func (agentsPanelWorkspace) SupportsThreads() bool { return false }

func (agentsPanelWorkspace) SupportsTasks() bool { return false }

func (agentsPanelWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return messageID + "$$" + toolCallID
}

func (agentsPanelWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	messageID, toolCallID, ok := strings.Cut(sessionID, "$$")
	return messageID, toolCallID, ok
}

// TestMouseClick_AgentBlockOpensItsTranscript is the whole point of the
// section: a delegation runs somewhere you cannot see, and its block is the
// way in. The child session is named for the tool call that started it (see
// delegationSessionID in internal/agent), which is what lets the click reuse
// the transcript's own drill-in.
func TestMouseClick_AgentBlockOpensItsTranscript(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.com.Workspace = agentsPanelWorkspace{}
	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "msg-7$$call-1")}
	u.updateLayoutAndSize()

	plan := u.sessionPanelPlan(u.lay.layout.panel.Dy())
	agentRects := sessionPanelRowLayout(u.lay.layout.panel, plan).agentBlocks
	require.Len(t, agentRects, 1)

	rect := agentRects[0]
	u.Update(tea.MouseClickMsg{X: rect.Min.X, Y: rect.Min.Y, Button: tea.MouseLeft})

	require.True(t, u.viewingChildSession(), "the click must push a child-session nav frame")
	require.Equal(t, "msg-7$$call-1", u.sess.navStack[len(u.sess.navStack)-1].childSessionID)
}

// TestMouseClick_AgentsHeaderCollapsesSection mirrors the threads header's
// click-to-collapse.
func TestMouseClick_AgentsHeaderCollapsesSection(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "m$$c1")}
	u.updateLayoutAndSize()

	plan := u.sessionPanelPlan(u.lay.layout.panel.Dy())
	header := sessionPanelRowLayout(u.lay.layout.panel, plan).agentsHeader
	require.NotZero(t, header)

	u.Update(tea.MouseClickMsg{X: header.Min.X, Y: header.Min.Y, Button: tea.MouseLeft})
	require.True(t, u.panel.agentsCollapsed)
}

// TestPanelSpinner_RunningDelegationKeepsItTicking proves the panel animates
// for delegated work with nothing else live: the local agent is idle, there
// are no todos and no threads, and a delegation is still running.
func TestPanelSpinner_RunningDelegationKeepsItTicking(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	require.False(t, u.panelSpinnerWanted())

	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "m$$c1")}
	require.True(t, u.panelSpinnerWanted())

	u.agentList.cache.value[0].ParentSessionID = "somebody-else"
	require.False(t, u.panelSpinnerWanted(),
		"another session's delegation is live work, but not this panel's")
}

// TestSessionPanelRowLayout_AgentsAboveThreads pins the section order: a
// delegation belongs to the conversation directly above the panel and
// reports back into it, so it leads; a thread runs off in its own worktree
// and sits below.
func TestSessionPanelRowLayout_AgentsAboveThreads(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "m$$c1")}
	u.threadList.cache.value = []proto.Thread{
		{ID: "t1", SessionID: "s-t1", Name: "fix-auth", Status: "running", CreatedAt: time.Now().Unix()},
	}
	u.updateLayoutAndSize()

	plan := u.sessionPanelPlan(u.lay.layout.panel.Dy())
	layout := sessionPanelRowLayout(u.lay.layout.panel, plan)
	require.Len(t, layout.agentBlocks, 1)
	require.Len(t, layout.threadBlocks, 1)

	require.Less(t, layout.agentsHeader.Min.Y, layout.threadsHeader.Min.Y,
		"the agents header must come first")
	require.Less(t, layout.agentBlocks[0].Min.Y, layout.threadBlocks[0].Min.Y)
}

// TestSessionPanelPlan_TodosCappedAtTenRows proves the todos section stops
// growing at maxPanelTodosRows and scrolls instead: a long list is a list to
// scroll, not a wall that pushes the chat off screen. Nothing is dropped —
// todosContentRows still holds every row.
func TestSessionPanelPlan_TodosCappedAtTenRows(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	todos := make([]session.Todo, 0, 14)
	for i := range 14 {
		todos = append(todos, session.Todo{Content: fmt.Sprintf("todo %d", i), Status: session.TodoStatusPending})
	}
	u.sess.current.Todos = todos
	u.panel.expanded = true

	// A budget far larger than the list: only the cap can be what limits it.
	plan := u.sessionPanelPlan(100)
	require.Equal(t, 14, plan.todosContentRows, "every row is still in the plan")
	require.Equal(t, maxPanelTodosRows, plan.todosViewportRows)
	require.True(t, plan.todosScrollable, "the rows past the cap must stay reachable")
}

// TestSessionPanelPlan_TodosCapBeatsTheInProgressFloor covers the one case
// where the cap and the panel's older promise disagree: with more in-progress
// todos than the section may be tall, the height cap wins and the rest are a
// scroll away.
func TestSessionPanelPlan_TodosCapBeatsTheInProgressFloor(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	todos := make([]session.Todo, 0, 12)
	for i := range 12 {
		todos = append(todos, session.Todo{Content: fmt.Sprintf("todo %d", i), Status: session.TodoStatusInProgress})
	}
	u.sess.current.Todos = todos
	u.panel.expanded = true

	require.Equal(t, maxPanelTodosRows, u.sessionPanelPlan(100).todosViewportRows)
}

// TestSessionEvent_HidesPanelledDelegationsFromTranscript is the end-to-end
// half of the handoff: a delegation goes into the panel when it starts and
// leaves it when it finishes, so the transcript must have no row for it
// while the panel does, and its block must appear the moment the panel is
// done with it.
func TestSessionEvent_HidesPanelledDelegationsFromTranscript(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.chat.SetSize(80, 40)

	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "call-1", Name: "agent", Input: `{"prompt":"look into the flaky test"}`, Finished: false},
		nil, false, nil)
	u.chat.SetMessages(item)

	u.agentList.cache.value = []proto.Thread{liveDelegation("d1", "m$$call-1")}
	u.Update(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: *u.sess.current})
	require.Empty(t, item.Render(80), "the panel has it: no row in the transcript")

	// It finished and left the panel, so the transcript is where it lands.
	u.agentList.cache.value = nil
	u.Update(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: *u.sess.current})
	require.NotEmpty(t, item.Render(80), "the panel let go: the block appears")
}
