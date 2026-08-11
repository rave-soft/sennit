package model

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// newChildSessionPanelTestUI builds a UI in uiChat with a two-level-deep
// nav stack (developer-junior -> task), mirroring what enterChildSession
// leaves behind after two descents, and a real layout so u.layout.editor
// is a real rectangle sized for the panel. The top-level frame has 3
// siblings (this is the 3rd), the current frame has none, matching the
// target example: "developer-junior (3/3) › task (...)".
func newChildSessionPanelTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = agentSessionWorkspace{}
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.session = &session.Session{ID: "grandchild-session", PromptTokens: 800, CompletionTokens: 200}
	u.navStack = []sessionNavFrame{
		{
			parentSessionID: "main-session", parentTitle: "main",
			label: "fix the login bug", agentName: "developer-junior",
			siblings: make([]childSessionRef, 3), siblingIndex: 2,
		},
		{
			parentSessionID: "child-session", parentTitle: "developer-junior",
			label:     "Read the entire file and summarize its contents",
			agentName: "task", model: "claude-sonnet-5", effort: "medium",
		},
	}
	// Wide enough that the full activity parenthetical fits in row 1
	// without invoking the width-pressure fallbacks (those are covered on
	// their own by the TestChildSessionHeaderText_* tests below).
	u.width = 220
	u.updateLayoutAndSize()
	return u
}

// TestChildSessionLevelName covers the per-level breadcrumb text: the
// agent name, a "(n/m)" sibling counter only when there's more than one
// sibling, and a fallback to the prompt-snippet label when agentName
// wasn't resolved (e.g. a cycled-to sibling never rendered in this chat).
func TestChildSessionLevelName(t *testing.T) {
	t.Parallel()

	require.Equal(t, "task", childSessionLevelName(sessionNavFrame{agentName: "task"}))
	require.Equal(t, "developer-junior (3/3)", childSessionLevelName(sessionNavFrame{
		agentName: "developer-junior", siblings: make([]childSessionRef, 3), siblingIndex: 2,
	}))
	require.Equal(t, "task", childSessionLevelName(sessionNavFrame{
		agentName: "task", siblings: make([]childSessionRef, 1),
	}), "a single sibling must not show a counter")
	require.Equal(t, "some prompt snippet", childSessionLevelName(sessionNavFrame{label: "some prompt snippet"}),
		"must fall back to the label when agentName is unresolved")
}

// TestChildSessionPanelReplacesEditor: generateLayout must give the editor
// area a fixed childSessionPanelHeight while viewing a child session,
// instead of the textarea-driven height it uses otherwise.
func TestChildSessionPanelReplacesEditor(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.updateLayoutAndSize()
	require.NotEqual(t, childSessionPanelHeight, u.layout.editor.Dy(),
		"outside child-session view the editor must size to the textarea, not the panel")

	u.navStack = []sessionNavFrame{{parentSessionID: "parent", parentTitle: "main", agentName: "agent1"}}
	u.updateLayoutAndSize()

	require.Equal(t, childSessionPanelHeight, u.layout.editor.Dy(),
		"viewing a child session must give the editor area the panel's fixed height")
}

// TestChildSessionPanelClickExitsChildSession: a click anywhere in the
// editor area (now occupied by the panel) must pop the nav stack.
func TestChildSessionPanelClickExitsChildSession(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	require.NotZero(t, u.layout.editor)

	_, cmd := u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.editor.Min.X,
		Y:      u.layout.editor.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Len(t, u.navStack, 1, "clicking the panel must pop exactly one nav frame")
	require.NotNil(t, cmd, "must return the loadSession cmd for the previous level")
}

// TestChildSessionPanelButtonHover: moving the pointer over the panel's
// "back" button sets childPanelHover, and moving away clears it.
func TestChildSessionPanelButtonHover(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	require.NotZero(t, u.childPanelButtonRect, "drawing the panel must compute the button's click/hover rect")

	u.Update(tea.MouseMotionMsg(tea.Mouse{
		X: u.childPanelButtonRect.Min.X,
		Y: u.childPanelButtonRect.Min.Y,
	}))
	require.True(t, u.childPanelHover, "hovering the button must set childPanelHover")

	u.Update(tea.MouseMotionMsg(tea.Mouse{X: 0, Y: 0}))
	require.False(t, u.childPanelHover, "moving off the button must clear childPanelHover")
}

// TestDrawChildSessionPanel_ShowsBreadcrumbActivityModelEffortTokens covers
// the panel's content end to end: the ancestor's name+counter, the current
// level's bold name, its activity parenthetical (prompt snippet, step
// count, last tool while running), the model/effort line, and the split
// token usage. "main" must never appear — the back button already means
// "go up".
func TestDrawChildSessionPanel_ShowsBreadcrumbActivityModelEffortTokens(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.chat.AppendMessages(
		chat.NewToolMessageItem(u.com.Styles, "m1", message.ToolCall{ID: "tc-1", Name: "view", Input: `{}`, Finished: true}, nil, false, nil),
		chat.NewToolMessageItem(u.com.Styles, "m1", message.ToolCall{ID: "tc-2", Name: "view", Input: `{}`, Finished: true}, nil, false, nil),
		chat.NewToolMessageItem(u.com.Styles, "m1",
			message.ToolCall{ID: "tc-3", Name: "grep", Input: `{"pattern":"login"}`, Finished: true}, nil, false, nil),
	)
	u.wsCache.agentBusyCache.set(true)

	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	out := ansi.Strip(scr.Render())

	require.NotContains(t, out, "main", `"main" must never appear in the breadcrumb`)
	require.Contains(t, out, "developer-junior (3/3)", "ancestor level: name + sibling counter")
	require.Contains(t, out, "› task", "current level separated from its ancestor")
	require.Contains(t, out, "Read the entire file", "current level's prompt snippet")
	require.Contains(t, out, "step 3", "current level's step count, from the loaded child chat")
	require.Contains(t, out, `grep "login"`, "current level's last tool call, while running")
	require.Contains(t, out, "back (ctrl+up")
	require.Contains(t, out, "claude-sonnet-5")
	require.Contains(t, out, "effort medium")
	require.Contains(t, out, "800", "prompt token count must be shown")
	require.Contains(t, out, "200", "completion token count must be shown")
}

// TestChildSessionCurrentActivity_OmitsLastToolWhenNotRunning: the last
// tool call is only interesting while the delegation is actively running —
// once idle, only the snippet and step count remain.
func TestChildSessionCurrentActivity_OmitsLastToolWhenNotRunning(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.chat.AppendMessages(
		chat.NewToolMessageItem(u.com.Styles, "m1",
			message.ToolCall{ID: "tc-1", Name: "grep", Input: `{"pattern":"login"}`, Finished: true}, nil, false, nil),
	)
	u.wsCache.agentBusyCache.set(false)

	activity := u.childSessionCurrentActivity()
	require.Contains(t, activity, "step 1")
	require.NotContains(t, activity, "grep", "an idle delegation's last tool call is not shown")
}

// TestChildSessionHeaderText_DropsActivityBeforeAncestors covers the
// agreed sacrifice order (requirement 5): under width pressure, the
// activity parenthetical is dropped first, while the full ancestor
// breadcrumb is kept as long as it still fits.
func TestChildSessionHeaderText_DropsActivityBeforeAncestors(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)

	full := u.childSessionHeaderText(u.com.Styles, 200)
	require.Contains(t, ansi.Strip(full), "Read the entire file", "plenty of width: activity parenthetical shown")

	// Narrow enough to drop the parenthetical but not the ancestor name.
	narrower := u.childSessionHeaderText(u.com.Styles, ansi.StringWidth(ansi.Strip(full))-5)
	out := ansi.Strip(narrower)
	require.NotContains(t, out, "Read the entire file", "activity parenthetical must be dropped first")
	require.Contains(t, out, "developer-junior (3/3)", "ancestor breadcrumb must still be shown")
	require.Contains(t, out, "task")
}

// TestChildSessionHeaderText_CollapsesMiddleLevels covers a 3+ level deep
// stack: once even the plain (no-activity) breadcrumb doesn't fit, the
// middle levels collapse into a single "…", keeping the root and the
// current (bold) level.
func TestChildSessionHeaderText_CollapsesMiddleLevels(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack = append([]sessionNavFrame{
		{parentSessionID: "root", parentTitle: "main", agentName: "root-agent-with-a-long-name"},
	}, u.navStack...)
	u.navStack[len(u.navStack)-1].label = "" // no activity to simplify this test

	root := childSessionLevelName(u.navStack[0])
	middle := childSessionLevelName(u.navStack[1])
	last := childSessionLevelName(u.navStack[2])
	full := root + " › " + middle + " › " + last

	// Narrower than the full plain breadcrumb, but wide enough for
	// "root › … › last".
	collapsed := root + " › … › " + last
	avail := ansi.StringWidth(collapsed) + 2
	require.Less(t, avail, ansi.StringWidth(full), "test width must be too narrow for the uncollapsed breadcrumb")

	out := ansi.Strip(u.childSessionHeaderText(u.com.Styles, avail))
	require.Contains(t, out, "root-agent-with-a-long-name")
	require.Contains(t, out, "…", "the middle level must collapse to an ellipsis")
	require.Contains(t, out, "task")
	require.NotContains(t, out, "developer-junior", "the collapsed middle level's real name must not appear")
}

// TestChildSessionHeaderText_DropsAncestorsKeepsCurrentName covers the
// next sacrifice tier: too narrow even for the collapsed breadcrumb, so
// only the current level's own name remains.
func TestChildSessionHeaderText_DropsAncestorsKeepsCurrentName(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack[len(u.navStack)-1].label = ""

	out := ansi.Strip(u.childSessionHeaderText(u.com.Styles, len("task")+1))
	require.Equal(t, "task", out)
}

// TestChildSessionHeaderText_HardTruncatesCurrentName is the last resort:
// even the bare current name doesn't fit, so it gets hard-truncated with
// an ellipsis rather than overflowing.
func TestChildSessionHeaderText_HardTruncatesCurrentName(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack[len(u.navStack)-1].agentName = "a-very-long-subagent-name"
	u.navStack[len(u.navStack)-1].label = ""

	out := ansi.Strip(u.childSessionHeaderText(u.com.Styles, 6))
	require.LessOrEqual(t, ansi.StringWidth(out), 6)
	require.Contains(t, out, "…")
}

// TestDrawChildSessionPanel_NoModelEffortShowsDefaultModel covers the
// "inherits the app's default" case (agentic_fetch, or an agent tool with
// no override): row 2 must say something rather than render blank.
func TestDrawChildSessionPanel_NoModelEffortShowsDefaultModel(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack[len(u.navStack)-1].model = ""
	u.navStack[len(u.navStack)-1].effort = ""

	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "default model")
}

// TestDrawChildSessionPanel_RunningShowsTickingElapsed covers the "still
// running" case: with no frozen duration but the child session busy, row 3
// must show a live elapsed time computed from delegationStart, not a
// static/frozen value.
func TestDrawChildSessionPanel_RunningShowsTickingElapsed(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack[len(u.navStack)-1].delegationStart = time.Now().Add(-45 * time.Second)
	u.navStack[len(u.navStack)-1].delegationDuration = 0
	u.wsCache.agentBusyCache.set(true)

	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "45s elapsed")
}

// TestDrawChildSessionPanel_DoneShowsFrozenDuration covers the finished
// case: a non-zero delegationDuration must win over any live elapsed
// computation, and render without the "elapsed" suffix (it's a final
// total, not a ticking value).
func TestDrawChildSessionPanel_DoneShowsFrozenDuration(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack[len(u.navStack)-1].delegationStart = time.Now().Add(-10 * time.Minute)
	u.navStack[len(u.navStack)-1].delegationDuration = 83 * time.Second
	u.wsCache.agentBusyCache.set(false)

	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "1m23s")
	require.NotContains(t, out, "elapsed", "a finished delegation's duration is a final total, not a ticking value")
}

// TestDrawChildSessionPanel_UnknownDurationOmitsTime covers a delegation
// reconstructed from history with a genuinely unknown runtime (see
// AgentToolMessageItem's duration field doc): neither a frozen duration
// nor a busy child session, so no time is shown at all — never a
// misleading "0s".
func TestDrawChildSessionPanel_UnknownDurationOmitsTime(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack[len(u.navStack)-1].delegationStart = time.Time{}
	u.navStack[len(u.navStack)-1].delegationDuration = 0
	u.wsCache.agentBusyCache.set(false)

	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "800", "tokens must still render")
	require.NotContains(t, out, "elapsed")
	require.NotContains(t, out, "0s")
}

// TestChildSessionPanelHeight_TwoRowAreaOmitsTokens: with only two rows
// available the panel must still render the breadcrumb and model line
// without panicking, and must not draw the (missing) third row.
func TestChildSessionPanelHeight_TwoRowAreaOmitsTokens(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	area := u.layout.editor
	area.Max.Y = area.Min.Y + 2

	scr := uv.NewScreenBuffer(u.width, u.height)
	require.NotPanics(t, func() {
		u.drawChildSessionPanel(scr, area)
	})
	out := ansi.Strip(scr.Render())
	require.Contains(t, out, "claude-sonnet-5")
	require.NotContains(t, out, "800", "the token row is out of bounds and must not render")
}
