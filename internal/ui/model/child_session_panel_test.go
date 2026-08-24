package model

import (
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// newChildSessionPanelTestUI builds a UI in uiChat with a two-level-deep
// nav stack (developer-junior -> task), mirroring what enterChildSession
// leaves behind after two descents, and a real layout so u.lay.layout.editor
// is a real rectangle sized for the panel. The top-level frame has 3
// siblings (this is the 3rd), the current frame has none, matching the
// target example: "developer-junior (3/3) › task (...)".
func newChildSessionPanelTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = agentSessionWorkspace{}
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.sess.current = &session.Session{ID: "grandchild-session", PromptTokens: 800, CompletionTokens: 200}
	u.sess.navStack = []sessionNavFrame{
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
	u.lay.width = 220
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
	require.NotEqual(t, childSessionPanelHeight, u.lay.layout.editor.Dy(),
		"outside child-session view the editor must size to the textarea, not the panel")

	u.sess.navStack = []sessionNavFrame{{parentSessionID: "parent", parentTitle: "main", agentName: "agent1"}}
	u.updateLayoutAndSize()

	require.Equal(t, childSessionPanelHeight, u.lay.layout.editor.Dy(),
		"viewing a child session must give the editor area the panel's fixed height")
}

// setChildSessionBusy makes the workspace stub report the delegation the
// top nav frame points at as generating, which is what the panel's
// elapsed line gates on — the workspace-wide agentBusyCache would also be
// true for an unrelated running agent.
func setChildSessionBusy(u *UI, busy bool) {
	frame := &u.sess.navStack[len(u.sess.navStack)-1]
	frame.childSessionID = u.sess.current.ID
	u.com.Workspace = agentSessionWorkspace{busySessions: map[string]bool{frame.childSessionID: busy}}
}

// TestDrawChildSessionPanel_ShowsModelEffortTokensAndNoNavigation covers
// the panel's content end to end — the model/effort line and the split
// token usage — and pins the division of labour: where you are and how to
// leave belong to the breadcrumb bar, so no crumb, separator, or back
// button may appear down here.
func TestDrawChildSessionPanel_ShowsModelEffortTokensAndNoNavigation(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.wsCache.agentBusyCache.set(true)

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "claude-sonnet-5")
	require.Contains(t, out, "effort medium")
	require.Contains(t, out, "800", "prompt token count must be shown")
	require.Contains(t, out, "200", "completion token count must be shown")

	require.NotContains(t, out, "developer-junior", "the breadcrumb belongs to the bar, not the panel")
	require.NotContains(t, out, "\u203a", "no breadcrumb separator in the panel")
	require.NotContains(t, out, "Back", "the back button belongs to the bar, not the panel")
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

// TestDrawChildSessionPanel_NoModelEffortShowsDefaultModel covers the
// "inherits the app's default" case (agentic_fetch, or an agent tool with
// no override): row 2 must say something rather than render blank.
func TestDrawChildSessionPanel_NoModelEffortShowsDefaultModel(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack[len(u.sess.navStack)-1].model = ""
	u.sess.navStack[len(u.sess.navStack)-1].effort = ""

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
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
	u.sess.navStack[len(u.sess.navStack)-1].delegationStart = time.Now().Add(-45 * time.Second)
	u.sess.navStack[len(u.sess.navStack)-1].delegationDuration = 0
	setChildSessionBusy(u, true)

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "45s elapsed")
}

// TestFreezeFinishedChildDelegation_FreezesOnFinish: a delegation that
// finishes while it is being viewed must keep its final runtime on screen.
// The elapsed reading is gated on the delegation being busy, so without
// this the number simply disappeared the moment it stopped — the one
// moment the user is waiting for it.
func TestFreezeFinishedChildDelegation_FreezesOnFinish(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	top := len(u.sess.navStack) - 1
	u.sess.navStack[top].delegationStart = time.Now().Add(-45 * time.Second)
	u.sess.navStack[top].delegationDuration = 0

	setChildSessionBusy(u, true)
	u.freezeFinishedChildDelegation()
	require.Zero(t, u.sess.navStack[top].delegationDuration, "a running delegation has nothing to freeze yet")

	setChildSessionBusy(u, false)
	u.freezeFinishedChildDelegation()
	require.InDelta(t, (45 * time.Second).Seconds(),
		u.sess.navStack[top].delegationDuration.Seconds(), 1)

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
	out := ansi.Strip(scr.Render())
	require.Contains(t, out, "45s")
	require.NotContains(t, out, "elapsed", "a finished delegation's runtime is a final total")
}

// TestFreezeFinishedChildDelegation_IgnoresAlreadyFinished: drilling into
// a delegation that was already done must not start timing it. Its
// runtime is whatever history recorded, and "unknown" is the honest
// answer when history recorded nothing — never "how long this session has
// been open".
func TestFreezeFinishedChildDelegation_IgnoresAlreadyFinished(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	top := len(u.sess.navStack) - 1
	u.sess.navStack[top].delegationStart = time.Now().Add(-3 * time.Hour)
	u.sess.navStack[top].delegationDuration = 0
	setChildSessionBusy(u, false)

	u.freezeFinishedChildDelegation()
	require.Zero(t, u.sess.navStack[top].delegationDuration)
}

// TestDrawChildSessionPanel_DoneShowsFrozenDuration covers the finished
// case: a non-zero delegationDuration must win over any live elapsed
// computation, and render without the "elapsed" suffix (it's a final
// total, not a ticking value).
func TestDrawChildSessionPanel_DoneShowsFrozenDuration(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack[len(u.sess.navStack)-1].delegationStart = time.Now().Add(-10 * time.Minute)
	u.sess.navStack[len(u.sess.navStack)-1].delegationDuration = 83 * time.Second
	u.wsCache.agentBusyCache.set(false)

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
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
	u.sess.navStack[len(u.sess.navStack)-1].delegationStart = time.Time{}
	u.sess.navStack[len(u.sess.navStack)-1].delegationDuration = 0
	u.wsCache.agentBusyCache.set(false)

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "800", "tokens must still render")
	require.NotContains(t, out, "elapsed")
	require.NotContains(t, out, "0s")
}

// TestChildSessionPanelHeight_OneRowAreaOmitsTokens: with only one row
// available the panel must still render the model line without panicking,
// and must not draw the (missing) second row.
func TestChildSessionPanelHeight_OneRowAreaOmitsTokens(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	area := u.lay.layout.editor
	area.Max.Y = area.Min.Y + 1

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	require.NotPanics(t, func() {
		u.drawChildSessionPanel(scr, area)
	})
	out := ansi.Strip(scr.Render())
	require.Contains(t, out, "claude-sonnet-5")
	require.NotContains(t, out, "800", "the token row is out of bounds and must not render")
}
