package model

import (
	"context"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

func newCmdDrivenGoldenUI(ws *cmdDrivingWorkspace) *UI {
	m := New(common.DefaultCommon(context.Background(), ws), "", false)
	m.state = uiChat
	m.focus = uiFocusEditor
	m.lay.width = 140
	m.lay.height = 45
	m.sess.current = &session.Session{ID: "s1"}
	m.readyPlaceholder = "Ready!"
	m.workingPlaceholder = "Working!"
	m.editor.textarea.Placeholder = m.readyPlaceholder
	warmCmdDrivenCaches(m)
	return m
}

func renderCmdDrivenUI(m *UI) []byte {
	canvas := uv.NewScreenBuffer(m.lay.width, m.lay.height)
	m.Draw(canvas, canvas.Bounds())
	return []byte(canvas.Render())
}

// renderDashboardScreen snapshots the threads dashboard screen straight
// from a uv.ScreenBuffer, mirroring Root.dashboardView's buffer draw (the
// test needs the raw buffer, not the normalized View string).
func renderDashboardScreen(r *Root) []byte {
	canvas := uv.NewScreenBuffer(r.width, r.height)
	r.dashboard.Draw(canvas, canvas.Bounds())
	if r.dashboardDialog.HasDialogs() {
		r.dashboardDialog.Draw(canvas, canvas.Bounds())
	}
	return []byte(canvas.Render())
}

func runRootCmdTree(r *Root, cmd tea.Cmd) *Root {
	if cmd == nil {
		return r
	}
	stack := []tea.Cmd{cmd}
	for len(stack) > 0 {
		cmd := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		msg := cmd()
		if cmds, ok := isCommandSliceWrapper(msg); ok {
			for _, cmd := range slices.Backward(cmds) {
				stack = append(stack, cmd)
			}
			continue
		}
		model, next := r.Update(msg)
		r = model.(*Root)
		if next != nil && !selfPerpetuatingTick(msg) {
			stack = append(stack, next)
		}
	}
	return r
}

func TestCmdDrivingGolden(t *testing.T) {
	t.Run("open_models", func(t *testing.T) {
		ws := &cmdDrivingWorkspace{agentReady: true}
		m := newCmdDrivenGoldenUI(ws)

		_, cmd := m.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'l'})
		runCmdTree(m, cmd, nil)

		require.True(t, m.dialog.ContainsDialog(dialog.ModelsID))
		golden.RequireEqual(t, renderCmdDrivenUI(m))
	})

	t.Run("send_message", func(t *testing.T) {
		ws := &cmdDrivingWorkspace{agentReady: true}
		m := newCmdDrivenGoldenUI(ws)
		m.editor.textarea.SetValue("ship the golden tests")

		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m.workingPlaceholder = "Working!"
		var captured bool
		runCmdTree(m, cmd, func(msg tea.Msg, next tea.Cmd) {
			if _, ok := msg.(agentRunSubmittedMsg); !ok {
				return
			}
			require.NotNil(t, next)
			require.True(t, m.isAgentBusy())
			golden.RequireEqual(t, renderCmdDrivenUI(m))
			captured = true
		})

		require.True(t, captured)
		require.Equal(t, "ship the golden tests", ws.agentRunPrompt)
	})

	t.Run("permission_flow", func(t *testing.T) {
		ws := &cmdDrivingWorkspace{agentReady: true}
		m := newCmdDrivenGoldenUI(ws)
		perm := permission.PermissionRequest{ID: "permission-golden", ToolCallID: "tool-golden", ToolName: "bash"}

		_, cmd := m.Update(pubsub.Event[permission.PermissionRequest]{Type: pubsub.CreatedEvent, Payload: perm})
		runCmdTree(m, cmd, nil)

		require.True(t, m.dialog.ContainsDialog(dialog.PermissionsID))
		golden.RequireEqual(t, renderCmdDrivenUI(m))

		m.dialog.CloseDialog(dialog.PermissionsID)
		m.dialog.OpenDialog(dialog.NewPermissions(m.com, perm))
		_, cmd = m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
		runCmdTree(m, cmd, nil)
		require.Positive(t, ws.permGrantCalls)
		require.False(t, m.dialog.ContainsDialog(dialog.PermissionsID))
	})

	t.Run("load_session", func(t *testing.T) {
		// Session state transition: loading a session populates the chat
		// and the header from workspace results, which the updateSession
		// router slice of S1 will own.
		ws := &cmdDrivingWorkspace{
			agentReady: true,
			sessionsBySessionID: map[string]session.Session{
				"s-loaded": {ID: "s-loaded", Title: "Loaded Session"},
			},
			messagesBySessionID: map[string][]message.Message{
				"s-loaded": {
					{ID: "m1", Role: message.User, SessionID: "s-loaded", Parts: []message.ContentPart{
						message.TextContent{Text: "What does the status line show?"},
					}},
					{ID: "m2", Role: message.Assistant, SessionID: "s-loaded", Parts: []message.ContentPart{
						message.TextContent{Text: "Focus, agent state, and keybindings."},
					}},
				},
			},
		}
		m := newCmdDrivenGoldenUI(ws)

		_, cmd := m.Update(requestSessionLoad{sessionID: "s-loaded"})
		runCmdTree(m, cmd, nil)

		require.Equal(t, "s-loaded", m.sess.current.ID)
		require.Equal(t, "Loaded Session", m.sess.current.Title)
		require.Equal(t, 2, m.chat.Len())
		golden.RequireEqual(t, renderCmdDrivenUI(m))
	})

	t.Run("threads_panel", func(t *testing.T) {
		ws := &cmdDrivingWorkspace{
			agentReady:      true,
			supportsThreads: true,
			threads: []proto.Thread{{
				ID:     "thread-golden",
				Name:   "Golden coverage",
				Status: "running",
				Goal:   "render the threads panel",
			}},
		}
		m := newCmdDrivenGoldenUI(ws)
		m.lay.width, m.lay.height = 100, 24
		r := &Root{
			com:             m.com,
			main:            m,
			dashboardDialog: dialog.NewOverlay(),
			width:           100,
			height:          24,
			active:          screenMain,
		}

		model, cmd := r.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'e'})
		r = model.(*Root)
		r = runRootCmdTree(r, cmd)

		require.Equal(t, screenDashboard, r.active)
		require.Positive(t, ws.listThreadsCalls)
		golden.RequireEqual(t, renderDashboardScreen(r))
	})
}
