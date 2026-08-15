package model

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	xstrings "github.com/charmbracelet/x/exp/strings"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/common"
)

// updateSystem handles the terminal/runtime and animation-tick branches of
// UI.Update: terminal capability probes (env, mode report, OSC, focus/blur),
// window resize, keyboard enhancement negotiation, chat/panel animation
// ticks, and scrollbar/warm-cache housekeeping messages. It is called from
// Update's message-type switch and shares that switch's cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateSystem(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.EnvMsg:
		// Is this Windows Terminal?
		if !m.sendProgressBar {
			m.sendProgressBar = slices.Contains(msg, "WT_SESSION")
		}
		cmds = append(cmds, common.QueryCmd(uv.Environ(msg)))
	case tea.ModeReportMsg:
		m.updateNotificationBackend()
	case uv.UnknownOscEvent:
		m.updateNotificationBackend()
	case tea.FocusMsg:
		m.notifyWindowFocused = true
	case tea.BlurMsg:
		m.notifyWindowFocused = false

	case tea.WindowSizeMsg:
		m.lay.width, m.lay.height = msg.Width, msg.Height
		// Suppress the chat's full-height scan during the resize so a drag
		// only reflows visible items; it settles (and recomputes) shortly
		// after the last resize event.
		if m.state == uiChat {
			cmds = append(cmds, m.chat.BeginResize())
		}
		m.updateLayoutAndSize()
		if m.state == uiChat && m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case tea.KeyboardEnhancementsMsg:
		m.keyenh = msg
		if msg.SupportsKeyDisambiguation() {
			if slices.Contains(m.keyMap.Models.Keys(), "ctrl+m") {
				m.keyMap.Models.SetHelp("ctrl+m", "models")
			} else if slices.Contains(m.keyMap.Models.Keys(), "super+m") {
				m.keyMap.Models.SetHelp("super+m", "models")
			}
			if slices.Contains(m.keyMap.Editor.Newline.Keys(), "shift+enter") {
				m.keyMap.Editor.Newline.SetHelp("shift+enter", "newline")
			}
		}

	case anim.StepMsg:
		if m.state == uiChat {
			if cmd := m.chat.Animate(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.chat.Follow() {
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	case scrollbarHideMsg:
		if m.state == uiChat {
			m.chat.HideScrollbar(msg.seq)
		}
	case chatWarmMsg:
		// A resize has settled; warm the message cache one batch at a time
		// so the scrollbar recompute never blocks the UI thread.
		if m.state == uiChat {
			cmd, done := m.chat.WarmStep(msg.seq)
			if cmd != nil {
				cmds = append(cmds, cmd)
			} else if done {
				// Heights are cached now, so the final layout pass (scrollbar
				// reservation) is cheap.
				m.updateLayoutAndSize()
			}
		}
	case sidebarScrollbarHideMsg:
		if msg.seq == m.sidebar.scrollbarSeq {
			m.sidebar.hideScrollbar()
		}
	case spinner.TickMsg:
		if m.dialog.HasDialogs() {
			// route to dialog
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		// Stop the tick loop when nothing live is left (or the chat screen
		// isn't showing); syncPanelSpinner re-arms it on the next relevant
		// event. Letting the loop die and be restarted beats ticking
		// forever behind an idle screen.
		if m.panel.panelIsSpinning && (m.state != uiChat || !m.panelSpinnerWanted()) {
			m.panel.panelIsSpinning = false
		}
		if m.panel.panelIsSpinning {
			var cmd tea.Cmd
			m.panel.panelSpinner, cmd = m.panel.panelSpinner.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case uv.KittyGraphicsEvent:
		if !bytes.HasPrefix(msg.Payload, []byte("OK")) {
			slog.Warn("Unexpected Kitty graphics response",
				"response", string(msg.Payload),
				"options", msg.Options)
		}
	}
	return cmds, false
}

// updateTerminalVersion handles tea.TerminalVersionMsg. It always returns m,
// nil, bypassing Update's common tail exactly as the original inline case
// did.
func (m *UI) updateTerminalVersion(msg tea.TerminalVersionMsg) (tea.Model, tea.Cmd) {
	termVersion := strings.ToLower(msg.Name)
	// Only enable progress bar for the following terminals.
	if !m.sendProgressBar {
		m.sendProgressBar = xstrings.ContainsAnyOf(termVersion, "ghostty", "iterm2", "rio")
	}
	return m, nil
}
