package model

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/util"
)

// updateShell handles the inline-editor and shell-command branches of
// UI.Update: applying an externally edited draft, and streaming/completing
// a bang (`!`) shell command's output. It is called from Update's
// message-type switch and shares that switch's cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateShell(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case openEditorMsg:
		prevHeight := m.editor.textarea.Height()
		m.editor.textarea.SetValue(msg.Text)
		m.editor.textarea.MoveToEnd()
		m.syncBangModeFromTextarea()
		cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))
	case shellStreamMsg:
		if item := m.chat.MessageItem(msg.PendingID); item != nil {
			if shellItem, ok := item.(*chat.ShellItem); ok {
				shellItem.AppendOutput(msg.Chunk)
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		// Continue draining the stream channel.
		if msg.streamCh != nil {
			ch := msg.streamCh
			pid := msg.PendingID
			cmds = append(cmds, func() tea.Msg {
				chunk, ok := <-ch
				if !ok {
					return nil
				}
				return shellStreamMsg{PendingID: pid, Chunk: chunk, streamCh: ch}
			})
		}
	case shellResultMsg:
		if (m.sessionLoadExpectedID != "" && msg.sessionID != m.sessionLoadExpectedID) || msg.generation != m.sessionLoadGen {
			break
		}
		m.editor.pendingSendActive = false
		// Clear the bang cancel func — command is done.
		if m.editor.bangCancel != nil {
			m.editor.bangCancel()
			m.editor.bangCancel = nil
		}
		// Complete the pending shell item if it exists, otherwise create a new one.
		completed := false
		if msg.PendingID != "" {
			if item := m.chat.MessageItem(msg.PendingID); item != nil {
				if shellItem, ok := item.(*chat.ShellItem); ok {
					shellItem.Complete(msg.Output, msg.ExitCode)
					if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
					completed = true
				}
			}
		}
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(fmt.Errorf("shell command failed: %w", msg.Err)))
		}
		if !completed {
			item := chat.NewShellItem(m.com.Styles, msg.Command, msg.Output, msg.ExitCode)
			m.chat.AppendMessages(item)
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		cmds = append(cmds, m.loadPromptHistory())
		if len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}
	}
	return cmds, false
}
