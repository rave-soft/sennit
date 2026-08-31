package model

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// openExternalEditorGuarded appends the command that opens the external
// editor with the textarea's current value (bang-prefixed in bang mode),
// or a busy warning instead when the agent is working. started reports
// which happened, for callers that only take a further action (closing a
// dialog) on success. Shared by the Editor.OpenEditor key binding and the
// commands palette's ActionExternalEditor.
func (m *UI) openExternalEditorGuarded(cmds []tea.Cmd) (out []tea.Cmd, started bool) {
	if m.isAgentBusy() {
		return append(cmds, util.ReportWarn("Agent is working, please wait...")), false
	}
	editorValue := m.editor.bang.draftValue(m.editor.textarea.Value())
	return append(cmds, m.editor.openEditor(editorValue)), true
}

// openThreadsDashboardGuarded appends the command that opens the threads
// dashboard, or an info message instead when the workspace doesn't support
// threads at all. Shared by the Threads key binding and the commands
// palette's ActionOpenThreadsDashboard.
func openThreadsDashboardGuarded(com *common.Common, cmds []tea.Cmd) []tea.Cmd {
	if !com.Workspace.SupportsThreads() {
		return append(cmds, util.ReportInfo("This workspace doesn't support threads."))
	}
	return append(cmds, util.CmdHandler(showThreadsDashboardMsg{}))
}

// scrollChatUpAndKeepSelectionVisible scrolls the chat up one line,
// animated, and — when that scroll left the selected item outside the
// viewport — moves the selection to the previous item and scrolls it back
// into view. Shared by the Chat.Up key binding and the mouse hover-scroll
// zone at the top of the chat viewport (mouse.go).
func (w *widgets) scrollChatUpAndKeepSelectionVisible(cmds []tea.Cmd) []tea.Cmd {
	if cmd := w.chat.ScrollByAndAnimate(-1); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if !w.chat.SelectedItemInView() {
		w.chat.SelectPrev()
		if cmd := w.chat.ScrollToSelectedAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// scrollChatDownAndKeepSelectionVisible mirrors
// scrollChatUpAndKeepSelectionVisible for the Chat.Down key binding and the
// mouse hover-scroll zone at the bottom of the chat viewport.
func (w *widgets) scrollChatDownAndKeepSelectionVisible(cmds []tea.Cmd) []tea.Cmd {
	if cmd := w.chat.ScrollByAndAnimate(1); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if !w.chat.SelectedItemInView() {
		w.chat.SelectNext()
		if cmd := w.chat.ScrollToSelectedAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// handleKeyPressMsg is the top-level key event router. It handles the
// global guards (quit, dialog routing, inline editor routing, cancel), then
// dispatches to the state/focus-specific handlers below.
func (m *UI) handleKeyPressMsg(msg tea.KeyPressMsg) tea.Cmd {
	var cmds []tea.Cmd

	if key.Matches(msg, m.keyMap.Quit) && !m.dialog.ContainsDialog(dialog.QuitID) {
		// Always handle quit keys first
		m.openQuitDialog(m.com)

		return tea.Batch(cmds...)
	}

	// Route all messages to dialog if one is open.
	if m.dialog.HasDialogs() {
		return m.handleDialogMsg(msg)
	}

	// Route keys to active inline editor if one is showing.
	if m.activeInline != nil && m.focus == uiFocusEditor {
		if done, cmd := m.activeInline.HandleKey(msg); done {
			// cmd may carry the submit/cancel side effect (e.g. the
			// question form's workspace call) — it must still run even
			// though the editor itself is going away.
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			m.activeInline = nil
			m.editor.textarea.Focus()
			m.updateLayoutAndSize()
		} else {
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.activeInline.HeightChanged() {
				m.updateLayoutAndSize()
			}
		}
		return tea.Batch(cmds...)
	}

	// Handle cancel key when agent is busy.
	if key.Matches(msg, m.keyMap.Chat.Cancel) {
		if m.isAgentBusy() {
			if cmd := m.cancelAgent(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return tea.Batch(cmds...)
		}
	}

	switch m.state {
	case uiOnboarding:
		return tea.Batch(cmds...)
	case uiInitialize:
		cmds = append(cmds, m.updateInitializeView(msg)...)
		return tea.Batch(cmds...)
	case uiChat, uiLanding:
		switch m.focus {
		case uiFocusEditor:
			var finalCmd tea.Cmd
			var early bool
			cmds, finalCmd, early = m.handleEditorKeyPress(msg, cmds)
			if early {
				return finalCmd
			}
		case uiFocusMain:
			cmds = m.handleMainKeyPress(msg, cmds)
		default:
			cmds, _ = m.handleGlobalKeys(msg, cmds)
		}
	default:
		cmds, _ = m.handleGlobalKeys(msg, cmds)
	}

	return tea.Sequence(cmds...)
}

// handleGlobalKeys handles key bindings that apply regardless of focus or
// state: help, the command/model/session dialogs, threads, details, pills,
// suspend, and yolo mode. It returns the (possibly extended) cmds slice and
// whether the key was consumed.
func (m *UI) handleGlobalKeys(msg tea.KeyPressMsg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keyMap.Help):
		m.status.ToggleHelp()
		m.updateLayoutAndSize()
		return cmds, true
	case key.Matches(msg, m.keyMap.Commands):
		if cmd := m.openCommandsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return cmds, true
	case key.Matches(msg, m.keyMap.Models):
		if cmd := m.openModelsDialog(m.com); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return cmds, true
	case key.Matches(msg, m.keyMap.Sessions):
		if cmd := m.openSessionsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return cmds, true
	case key.Matches(msg, m.keyMap.Threads):
		cmds = openThreadsDashboardGuarded(m.com, cmds)
		return cmds, true
	case key.Matches(msg, m.keyMap.Chat.Details) && m.lay.isCompact:
		m.lay.detailsOpen = !m.lay.detailsOpen
		m.updateLayoutAndSize()
		return cmds, true
	case key.Matches(msg, m.keyMap.Chat.TogglePills):
		if m.state == uiChat && m.hasSession() {
			if cmd := m.toggleTodosExpanded(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return cmds, true
		}
	case key.Matches(msg, m.keyMap.Suspend):
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait..."))
			return cmds, true
		}
		cmds = append(cmds, tea.Suspend)
		return cmds, true
	case key.Matches(msg, m.keyMap.ToggleYolo):
		cmds = append(cmds, m.toggleYoloMode())
		return cmds, true
	}
	return cmds, false
}

// handleEditorKeyPress handles key events while the editor is focused. It
// first resolves the Escape double-tap sequence and routes to completions
// and attachments if either is capturing input, then falls through to
// handleEditorBindingKeyPress for the editor's own key bindings.
//
// The third return value reports whether a branch below took one of
// handleKeyPressMsg's early-return paths; when true, the caller must return
// the second value immediately, bypassing the rest of handleKeyPressMsg's
// tail (the outer switches and the final tea.Sequence(cmds...)) exactly as
// the original inline case did. When false, the caller continues normally
// with the returned cmds.
func (m *UI) handleEditorKeyPress(msg tea.KeyPressMsg, cmds []tea.Cmd) ([]tea.Cmd, tea.Cmd, bool) {
	// Double-Esc clears the draft outright (see the Editor.Escape
	// case below): any key other than Escape breaks the sequence.
	if !key.Matches(msg, m.keyMap.Editor.Escape) {
		m.editor.lastKeyWasEsc = false
	}

	// Handle completions if open.
	if m.editor.completions.open {
		if msg, ok := m.editor.completions.popup.Update(msg); ok {
			switch msg := msg.(type) {
			case completions.SelectionMsg[completions.FileCompletionValue]:
				cmds = append(cmds, m.insertFileCompletion(msg.Value.Path))
				if !msg.KeepOpen {
					m.editor.completions.close()
				}
			case completions.SelectionMsg[completions.ResourceCompletionValue]:
				cmds = append(cmds, m.insertMCPResourceCompletion(msg.Value))
				if !msg.KeepOpen {
					m.editor.completions.close()
				}
			case completions.SelectionMsg[completions.CommandCompletionValue]:
				if msg.InsertOnly {
					// Tab: fill in the command name so the user can
					// type arguments, without running it.
					m.editor.completions.replace(&m.editor.textarea, "/"+msg.Value.Title)
				} else {
					// Enter: run the command immediately and clear
					// the editor, same as picking it from the
					// Commands palette.
					m.editor.textarea.Reset()
					if action, ok := msg.Value.Action.(dialog.Action); ok {
						cmds = append(cmds, m.applyDialogAction(action))
					}
				}
				m.editor.completions.close()
			case completions.ClosedMsg:
				m.editor.completions.close()
			}
			return cmds, tea.Batch(cmds...), true
		}
	}

	if ok := m.editor.attachments.Update(msg); ok {
		return cmds, tea.Batch(cmds...), true
	}

	return m.handleEditorBindingKeyPress(msg, cmds)
}

// handleEditorBindingKeyPress handles the editor's own key bindings (send,
// new session, open editor, newline, history, tab-completion of the ghost
// prediction, escape) and, in its default case, falls back to global keys
// and then to the textarea itself. See handleEditorKeyPress for the
// early-exit contract of the third return value.
func (m *UI) handleEditorBindingKeyPress(msg tea.KeyPressMsg, cmds []tea.Cmd) ([]tea.Cmd, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keyMap.Editor.AddImage):
		if !currentModelSupportsImages(m.com) {
			break
		}
		if cmd := m.openFilesDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case key.Matches(msg, m.keyMap.Editor.PasteImage):
		if !currentModelSupportsImages(m.com) {
			break
		}
		cmds = append(cmds, m.pasteImageFromClipboardCmd())

	case key.Matches(msg, m.keyMap.Editor.SendMessage):
		prevHeight := m.editor.textarea.Height()
		value := m.editor.textarea.Value()
		if before, ok := strings.CutSuffix(value, "\\"); ok {
			// If the last character is a backslash, remove it and add a newline.
			m.editor.textarea.SetValue(before)
			if cmd := m.handleTextareaHeightChange(prevHeight); cmd != nil {
				cmds = append(cmds, cmd)
			}
			break
		}

		// Otherwise, send the message
		m.editor.textarea.Reset()
		if cmd := m.handleTextareaHeightChange(prevHeight); cmd != nil {
			cmds = append(cmds, cmd)
		}

		value = strings.TrimSpace(value)
		if value == "exit" || value == "quit" {
			m.openQuitDialog(m.com)
			return cmds, nil, true
		}

		if m.editor.bang.isActive() && value != "" {
			m.editor.bang.exit()
			m.setEditorPrompt(m.yoloModeCached())
			m.editor.randomizePlaceholders()
			m.editor.historyReset()
			return cmds, tea.Batch(m.runShellCommand(value)), true
		}

		attachments := m.editor.attachments.List()
		m.editor.attachments.Reset()
		if len(value) == 0 && len(attachments) == 0 {
			return cmds, nil, true
		}

		m.editor.randomizePlaceholders()
		m.editor.historyReset()

		return cmds, tea.Batch(m.sendMessage(value, attachments...), m.sess.loadPromptHistory(m.com)), true
	case key.Matches(msg, m.keyMap.Chat.NewSession):
		if !m.hasSession() {
			break
		}
		cmds, _ = m.startNewSessionGuarded(cmds)
	case key.Matches(msg, m.keyMap.Editor.OpenEditor):
		cmds, _ = m.openExternalEditorGuarded(cmds)
	case key.Matches(msg, m.keyMap.Editor.Newline):
		prevHeight := m.editor.textarea.Height()
		m.editor.textarea.InsertRune('\n')
		m.editor.completions.close()
		cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))
	case key.Matches(msg, m.keyMap.Editor.HistoryPrev):
		cmd := m.handleHistoryUp(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Editor.HistoryNext):
		cmd := m.handleHistoryDown(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Editor.ScrollPageUp):
		if cmd := m.scrollChatPage(-1); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Editor.ScrollPageDown):
		if cmd := m.scrollChatPage(1); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Tab):
		// Tab accepts the inline history prediction, if one is
		// showing. It's otherwise a no-op here (deliberately: it
		// no longer toggles focus, and must not fall through to
		// the default branch below, which would hand it to the
		// raw textarea and insert a literal tab character).
		if tail := m.activeGhostTail(); tail != "" {
			prevHeight := m.editor.textarea.Height()
			m.editor.textarea.InsertString(tail)
			m.editor.textarea.MoveToEnd()
			if cmd := m.updateTextareaWithPrevHeight(nil, prevHeight); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case key.Matches(msg, m.keyMap.Editor.Escape):
		consecutive := m.editor.lastKeyWasEsc
		m.editor.lastKeyWasEsc = true
		if !consecutive {
			// First Esc: its own job (exit history nav to the draft,
			// or whatever the textarea itself does with Escape).
			if cmd := m.handleHistoryEscape(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			// Hide any active ghost prediction until the input
			// changes again. Applied after handleHistoryEscape so
			// it hides relative to the value Esc left behind (e.g.
			// the restored draft), not the pre-Esc value.
			m.editor.ghostHiddenFor = m.editor.textarea.Value()
		} else {
			// Second Esc right after the first: the first one
			// already did whatever it could (exited history nav,
			// etc.), so this one wipes the draft outright instead
			// of leaving stale text sitting in the editor.
			prevHeight := m.editor.textarea.Height()
			m.editor.historyReset()
			m.editor.textarea.Reset()
			m.syncBangModeFromTextarea()
			if cmd := m.updateTextareaWithPrevHeight(nil, prevHeight); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	default:
		cmds = m.handleEditorTextInput(msg, cmds)
	}
	return cmds, nil, false
}

// handleEditorTextInput handles the default fall-through case of
// handleEditorBindingKeyPress's switch: global keys, bang-mode entry/exit,
// the "@" and "/" completion triggers, and finally passing the key through
// to the textarea itself.
func (m *UI) handleEditorTextInput(msg tea.KeyPressMsg, cmds []tea.Cmd) []tea.Cmd {
	var ok bool
	cmds, ok = m.handleGlobalKeys(msg, cmds)
	if ok {
		// Handle global keys first before passing to textarea.
		return cmds
	}

	// Bang mode: backspace on already-empty prompt exits.
	if msg.Code == tea.KeyBackspace && m.editor.bang.exitOnEmptyBackspace() {
		m.setEditorPrompt(m.yoloModeCached())
		return cmds
	}

	// Check for @ trigger before passing to textarea.
	curValue := m.editor.textarea.Value()
	// The cursor position, not len(curValue): typing "@" mid-buffer must
	// check the character actually preceding the cursor and anchor the
	// completion at that spot, not at the end of the text.
	curIdx := m.editor.textareaCursorOffset()

	// Trigger completions on @. Suppressed in bang mode: "@" is
	// just a character in a shell command (e.g. "git log @{u}"),
	// not a file-mention trigger.
	if msg.String() == "@" && !m.editor.completions.open && !m.editor.bang.isActive() {
		// Only show if beginning of prompt or after whitespace.
		if curIdx == 0 || (curIdx > 0 && isWhitespace(curValue[curIdx-1])) {
			depth, limit := m.com.Config().Options.TUI.Completions.Limits()
			cmds = append(cmds, m.editor.completions.openFiles(
				curIdx, m.completionsPosition(), m.completionsMaxWidth(), depth, limit,
				func() []completions.ResourceCompletionValue { return loadMCPResourceCompletions(m.com) },
			))
		}
	}

	// Trigger command completions on "/" at the very start of an
	// otherwise-empty editor, mirroring opencode/Claude Code: a
	// "/" mid-message is just a character. Suppressed in bang
	// mode: a shell command is very plausibly an absolute path
	// like "/usr/bin/env", not a command trigger.
	if msg.String() == "/" && !m.editor.completions.open && !m.editor.bang.isActive() && curValue == "" {
		m.editor.completions.openCommands(
			curIdx, m.completionsPosition(), m.completionsMaxWidth(), m.commandCompletionItems(),
		)
	}

	// remove the details if they are open when user starts typing
	if m.lay.detailsOpen {
		m.lay.detailsOpen = false
		m.updateLayoutAndSize()
	}

	prevHeight := m.editor.textarea.Height()
	cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))

	newVal := m.editor.textarea.Value()
	if m.editor.bang.enterFromLeadingPrefix(&m.editor.textarea, curValue, m.editor.textarea.Column()) {
		m.setEditorPrompt(m.yoloModeCached())
	} else {
		m.editor.bang.updateEmpty(curValue, newVal)
	}

	// Any text modification becomes the current draft.
	m.editor.updateHistoryDraft(curValue)

	// After updating textarea, check if we need to filter completions.
	// Skip filtering on the initial @ or / keystroke: for @ the
	// items are still loading async, and for / the query is
	// empty anyway (OpenCommands already populated the list).
	if m.editor.completions.open && msg.String() != "@" && msg.String() != "/" {
		m.editor.completions.updateQuery(
			m.editor.textareaCursorOffset(), m.editor.textareaWord(), msg.String() == "space",
		)
	}
	return cmds
}

// scrollChatPage scrolls the conversation by one screenful in the given
// direction (-1 up, 1 down) and keeps the chat's selection inside the new
// viewport. It's the editor-focused counterpart of the Chat.PageUp/PageDown
// branches in handleMainKeyPress, so the page keys work while typing.
func (m *UI) scrollChatPage(dir int) tea.Cmd {
	if m.state != uiChat || !m.hasSession() {
		return nil
	}
	cmd := m.chat.ScrollByAndAnimate(dir * m.chat.Height())
	if dir < 0 {
		m.chat.SelectFirstInView()
	} else {
		m.chat.SelectLastInView()
	}
	return cmd
}

// handleMainKeyPress handles key events while the chat list is focused:
// navigation, selection, and child-session traversal. Unmatched keys fall
// through to the chat's own key handler and, failing that, to global keys.
func (m *UI) handleMainKeyPress(msg tea.KeyPressMsg, cmds []tea.Cmd) []tea.Cmd {
	switch {
	case key.Matches(msg, m.keyMap.Chat.NewSession):
		if !m.hasSession() {
			break
		}
		var started bool
		if cmds, started = m.startNewSessionGuarded(cmds); started {
			m.focus = uiFocusEditor
		}
	case key.Matches(msg, m.keyMap.Chat.Expand):
		m.chat.ToggleExpandedSelectedItem()
	case key.Matches(msg, m.keyMap.Chat.EnterChildSession) && m.state == uiChat && m.hasSession():
		if messageID, toolCallID, ok := m.chat.SelectedNestedToolContainer(); ok {
			if cmd := m.enterChildSession(messageID, toolCallID); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case key.Matches(msg, m.keyMap.Chat.ExitChildSession) && m.state == uiChat && m.hasSession():
		if cmd := m.exitChildSession(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Chat.PrevChildSession) && m.state == uiChat && m.hasSession():
		if cmd := m.cycleChildSession(-1); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Chat.NextChildSession) && m.state == uiChat && m.hasSession():
		if cmd := m.cycleChildSession(1); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Chat.Up):
		cmds = m.scrollChatUpAndKeepSelectionVisible(cmds)
	case key.Matches(msg, m.keyMap.Chat.Down):
		cmds = m.scrollChatDownAndKeepSelectionVisible(cmds)
	case key.Matches(msg, m.keyMap.Chat.UpOneItem):
		m.chat.SelectPrev()
		if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Chat.DownOneItem):
		m.chat.SelectNext()
		if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Chat.HalfPageUp):
		if cmd := m.chat.ScrollByAndAnimate(-m.chat.Height() / 2); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectFirstInView()
	case key.Matches(msg, m.keyMap.Chat.HalfPageDown):
		if cmd := m.chat.ScrollByAndAnimate(m.chat.Height() / 2); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLastInView()
	case key.Matches(msg, m.keyMap.Chat.PageUp):
		if cmd := m.chat.ScrollByAndAnimate(-m.chat.Height()); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectFirstInView()
	case key.Matches(msg, m.keyMap.Chat.PageDown):
		if cmd := m.chat.ScrollByAndAnimate(m.chat.Height()); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLastInView()
	case key.Matches(msg, m.keyMap.Chat.Home):
		if cmd := m.chat.ScrollToTopAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectFirst()
	case key.Matches(msg, m.keyMap.Chat.End):
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLast()
	default:
		if ok, cmd := m.chat.HandleKeyMsg(msg); ok {
			cmds = append(cmds, cmd)
		} else {
			cmds, _ = m.handleGlobalKeys(msg, cmds)
		}
	}
	return cmds
}

// isWhitespace returns true if the byte is a whitespace character.
func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
