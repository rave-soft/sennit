package model

import (
	"context"
	"errors"
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
		if (m.sess.loadExpectedID != "" && msg.sessionID != m.sess.loadExpectedID) || msg.generation != m.sess.loadGen {
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

// runShellCommand executes a shell command server-side without triggering
// the LLM. The result is displayed as a tool-style item in the chat.
func (m *UI) runShellCommand(command string) tea.Cmd {
	if m.viewingChildSession() {
		return util.ReportWarn("viewing subagent session · " + m.exitChildSessionShortcut() + " to return")
	}
	if m.sess.current != nil {
		m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{
			content:        command,
			sessionID:      m.sess.current.ID,
			loadGeneration: m.sess.loadGen,
			bang:           true,
		})
		return func() tea.Msg { return sendPendingQueueMsg{} }
	}
	return m.runShellCommandInternal(command, true)
}

// runShellCommandInternal is the shared implementation for bang-mode shell
// execution. isFirstMessage indicates the command is the first user message
// in a newly created session, which triggers title generation.
func (m *UI) runShellCommandInternal(command string, isFirstMessage bool) tea.Cmd {
	var cmds []tea.Cmd
	if !m.hasSession() {
		if m.editor.pendingSendLoading {
			m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{content: command, generation: m.editor.pendingSendGen, bang: true})
			return nil
		}
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen++
		generation := m.editor.pendingSendGen
		workspace := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			newSession, err := workspace.CreateSession(context.Background(), "New Session")
			if err != nil {
				return sendMessageErrorMsg{Err: err, generation: generation, creating: true}
			}
			return bangSessionCreatedMsg{session: newSession, command: command, isFirstMessage: isFirstMessage, generation: generation}
		})
		return tea.Batch(cmds...)
	}

	sessionID := m.sess.current.ID
	loadGeneration := m.sess.loadGen
	contentWidth := min(m.lay.layout.main.Dx()-2, 120)

	// Append a pending shell item immediately so the user sees feedback.
	pendingItem := chat.NewPendingShellItem(m.com.Styles, command)
	m.chat.AppendMessages(pendingItem)
	if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := pendingItem.StartAnimation(); cmd != nil {
		cmds = append(cmds, cmd)
	}

	// Stream output via channel. The progress callback writes chunks
	// to streamCh; a reader cmd converts them to shellStreamMsg values.
	streamCh := make(chan string, 64)
	pendingID := pendingItem.ID()

	onProgress := func(chunk string) {
		select {
		case streamCh <- chunk:
		default:
			// Drop if UI can't keep up.
		}
	}

	// Reader cmd: drains streamCh into shellStreamMsg until closed.
	cmds = append(cmds, func() tea.Msg {
		chunk, ok := <-streamCh
		if !ok {
			return nil
		}
		return shellStreamMsg{PendingID: pendingID, Chunk: chunk, streamCh: streamCh}
	})

	ctx, cancel := context.WithCancel(context.Background())
	m.editor.bangCancel = cancel

	workspace := m.com.Workspace
	cmds = append(cmds, func() tea.Msg {
		resp, err := workspace.AgentRunShellCommand(ctx, sessionID, command, contentWidth, onProgress, isFirstMessage)
		close(streamCh)
		result := shellResultMsg{
			PendingID:  pendingID,
			Command:    command,
			Output:     resp.Output,
			sessionID:  sessionID,
			generation: loadGeneration,
		}
		if errors.Is(err, context.Canceled) {
			result.Canceled = true
			result.ExitCode = 130
			return result
		}
		if err != nil {
			result.Err = err
			result.ExitCode = 1
			return result
		}
		result.ExitCode = resp.ExitCode
		return result
	})
	return tea.Batch(cmds...)
}
