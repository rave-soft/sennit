package model

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// uiOwned: dispatched by execEditorCmd once the external $EDITOR exits.
// Routed by active screen instead, a thread's own external-edit draft
// could be applied to the main screen's textarea, or vice versa.
type openEditorMsg struct {
	uiOwned

	Text string
}

// shellResultMsg carries a bang command's completion. uiOwned: dispatched by
// runShellCommandInternal and, like shellStreamMsg's pending item, scoped to
// the *UI that started the run. Routed by active screen instead, a result
// that lands while the dashboard is up (Threads is not gated on busy, so
// ctrl+e is always reachable mid-run) was dropped — the pending ShellItem
// spun forever, bangCancel stayed set (blocking isAgentBusy()'s callers
// until a double-esc), and pendingSendActive stayed true so every later
// sendMessage queued behind it with nothing left to drain the queue.
type shellResultMsg struct {
	uiOwned

	PendingID  string // ID of the pending ShellItem to update.
	Command    string
	Output     string
	ExitCode   int
	Err        error
	Canceled   bool
	sessionID  string
	generation uint64
}

// shellStreamMsg carries incremental output from a streaming shell command.
//
// uiOwned: dispatched by runShellCommandInternal's stream-draining cmd and
// re-armed by updateShell itself. Routed by active screen instead, output
// for a bang command running in a thread could stream into the main
// screen's chat, or the drain loop could stop feeding the wrong screen
// entirely once misrouted once.
type shellStreamMsg struct {
	uiOwned

	PendingID string
	Chunk     string
	streamCh  <-chan string // unexported; used to continue draining
}

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
				return shellStreamMsg{uiOwned: uiOwned{owner: m}, PendingID: pid, Chunk: chunk, streamCh: ch}
			})
		}
	case shellResultMsg:
		if (m.sess.loadExpectedID != "" && msg.sessionID != m.sess.loadExpectedID) || msg.generation != m.sess.loadGen {
			// The result belongs to a session the user has since left, so
			// none of the chat updates below apply. The command itself is
			// over either way, and bangCancel is what isAgentBusy reads:
			// left set, the editor stayed "busy" for the rest of the
			// session with nothing running behind it.
			m.editor.pendingSend.finishActive()
			m.editor.bang.cancelRunning()
			break
		}
		m.editor.pendingSend.finishActive()
		// Clear the bang cancel func — command is done.
		m.editor.bang.cancelRunning()
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
		cmds = append(cmds, m.sess.loadPromptHistory(m.com, m))
		if m.editor.pendingSend.hasQueued() {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{uiOwned: uiOwned{owner: m}} })
		}
	}
	return cmds, false
}

// runShellCommand executes a shell command server-side without triggering
// the LLM. The result is displayed as a tool-style item in the chat.
func (m *UI) runShellCommand(command string) tea.Cmd {
	if m.sess.viewingChildSession() {
		return util.ReportWarn("viewing subagent session · " + m.exitChildSessionShortcut() + " to return")
	}
	if m.sess.current != nil {
		m.editor.pendingSend.enqueue(sendQueueItem{
			content:        command,
			sessionID:      m.sess.current.ID,
			loadGeneration: m.sess.loadGen,
			bang:           true,
		})
		return func() tea.Msg { return sendPendingQueueMsg{uiOwned: uiOwned{owner: m}} }
	}
	return m.runShellCommandInternal(command, true)
}

// runShellCommandInternal is the shared implementation for bang-mode shell
// execution. isFirstMessage indicates the command is the first user message
// in a newly created session, which triggers title generation.
func (m *UI) runShellCommandInternal(command string, isFirstMessage bool) tea.Cmd {
	var cmds []tea.Cmd
	if !m.sess.hasSession() {
		if m.editor.pendingSend.loadingNow() {
			m.editor.pendingSend.enqueue(sendQueueItem{content: command, generation: m.editor.pendingSend.generationNow(), bang: true})
			return nil
		}
		generation := m.editor.pendingSend.beginLoading()
		workspace := m.com.Workspace
		ctx := m.com.Context()
		owner := m
		cmds = append(cmds, func() tea.Msg {
			newSession, err := workspace.CreateSession(ctx, "New Session")
			if err != nil {
				return sendMessageErrorMsg{uiOwned: uiOwned{owner: owner}, Err: err, generation: generation, creating: true}
			}
			return bangSessionCreatedMsg{uiOwned: uiOwned{owner: owner}, session: newSession, command: command, isFirstMessage: isFirstMessage, generation: generation}
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

	cmds = append(cmds, func() tea.Msg {
		chunk, ok := <-streamCh
		if !ok {
			return nil
		}
		return shellStreamMsg{uiOwned: uiOwned{owner: m}, PendingID: pendingID, Chunk: chunk, streamCh: streamCh}
	})

	ctx, cancel := context.WithCancel(m.com.Context())
	m.editor.bang.setCancel(cancel)

	workspace := m.com.Workspace
	owner := m
	cmds = append(cmds, func() tea.Msg {
		resp, err := workspace.AgentRunShellCommand(ctx, sessionID, command, contentWidth, onProgress, isFirstMessage)
		close(streamCh)
		result := shellResultMsg{
			uiOwned:    uiOwned{owner: owner},
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
