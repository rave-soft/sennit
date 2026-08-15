package model

import (
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/ui/completions"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/ui/util"
)

// handleKeyPressMsg is the top-level key event router. It handles the
// global guards (quit, dialog routing, inline editor routing, cancel), then
// dispatches to the state/focus-specific handlers below.
func (m *UI) handleKeyPressMsg(msg tea.KeyPressMsg) tea.Cmd {
	var cmds []tea.Cmd

	if key.Matches(msg, m.keyMap.Quit) && !m.dialog.ContainsDialog(dialog.QuitID) {
		// Always handle quit keys first
		m.openQuitDialog()

		return tea.Batch(cmds...)
	}

	// Route all messages to dialog if one is open.
	if m.dialog.HasDialogs() {
		return m.handleDialogMsg(msg)
	}

	// Route keys to active inline editor if one is showing.
	if m.activeInline != nil && m.focus == uiFocusEditor {
		if done, cmd := m.activeInline.HandleKey(msg); done {
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
		if cmd := m.openModelsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return cmds, true
	case key.Matches(msg, m.keyMap.Sessions):
		if cmd := m.openSessionsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return cmds, true
	case key.Matches(msg, m.keyMap.Threads):
		if !m.com.Workspace.SupportsThreads() {
			cmds = append(cmds, util.ReportInfo("This workspace doesn't support threads."))
			return cmds, true
		}
		cmds = append(cmds, util.CmdHandler(showThreadsDashboardMsg{}))
		return cmds, true
	case key.Matches(msg, m.keyMap.Chat.Details) && m.isCompact:
		m.detailsOpen = !m.detailsOpen
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
	if m.editor.completionsOpen {
		if msg, ok := m.editor.completions.Update(msg); ok {
			switch msg := msg.(type) {
			case completions.SelectionMsg[completions.FileCompletionValue]:
				cmds = append(cmds, m.insertFileCompletion(msg.Value.Path))
				if !msg.KeepOpen {
					m.editor.closeCompletions()
				}
			case completions.SelectionMsg[completions.ResourceCompletionValue]:
				cmds = append(cmds, m.insertMCPResourceCompletion(msg.Value))
				if !msg.KeepOpen {
					m.editor.closeCompletions()
				}
			case completions.SelectionMsg[completions.CommandCompletionValue]:
				if msg.InsertOnly {
					// Tab: fill in the command name so the user can
					// type arguments, without running it.
					m.editor.insertCompletionText("/" + msg.Value.Title)
				} else {
					// Enter: run the command immediately and clear
					// the editor, same as picking it from the
					// Commands palette.
					m.editor.textarea.Reset()
					if action, ok := msg.Value.Action.(dialog.Action); ok {
						cmds = append(cmds, m.applyDialogAction(action))
					}
				}
				m.editor.closeCompletions()
			case completions.ClosedMsg:
				m.editor.closeCompletions()
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
		if !m.currentModelSupportsImages() {
			break
		}
		if cmd := m.openFilesDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case key.Matches(msg, m.keyMap.Editor.PasteImage):
		if !m.currentModelSupportsImages() {
			break
		}
		cmds = append(cmds, m.pasteImageFromClipboard)

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
			m.openQuitDialog()
			return cmds, nil, true
		}

		if m.editor.bangMode && value != "" {
			m.editor.bangMode = false
			m.setEditorPrompt(m.yoloModeCached())
			m.randomizePlaceholders()
			m.editor.historyReset()
			return cmds, tea.Batch(m.runShellCommand(value)), true
		}

		attachments := m.editor.attachments.List()
		m.editor.attachments.Reset()
		if len(value) == 0 && len(attachments) == 0 {
			return cmds, nil, true
		}

		m.randomizePlaceholders()
		m.editor.historyReset()

		return cmds, tea.Batch(m.sendMessage(value, attachments...), m.loadPromptHistory()), true
	case key.Matches(msg, m.keyMap.Chat.NewSession):
		if !m.hasSession() {
			break
		}
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
			break
		}
		if cmd := m.newSession(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case key.Matches(msg, m.keyMap.Editor.OpenEditor):
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is working, please wait..."))
			break
		}
		editorValue := m.editor.textarea.Value()
		if m.editor.bangMode {
			editorValue = "!" + editorValue
		}
		cmds = append(cmds, m.openEditor(editorValue))
	case key.Matches(msg, m.keyMap.Editor.Newline):
		prevHeight := m.editor.textarea.Height()
		m.editor.textarea.InsertRune('\n')
		m.editor.closeCompletions()
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
			m.editor.promptHistory.index = -1
			m.editor.promptHistory.draft = ""
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
	if m.editor.bangMode && m.editor.bangWasEmpty && msg.Code == tea.KeyBackspace {
		m.editor.bangMode = false
		m.editor.bangWasEmpty = false
		m.setEditorPrompt(m.yoloModeCached())
		return cmds
	}

	// Check for @ trigger before passing to textarea.
	curValue := m.editor.textarea.Value()
	curIdx := len(curValue)

	// Trigger completions on @. Suppressed in bang mode: "@" is
	// just a character in a shell command (e.g. "git log @{u}"),
	// not a file-mention trigger.
	if msg.String() == "@" && !m.editor.completionsOpen && !m.editor.bangMode {
		// Only show if beginning of prompt or after whitespace.
		if curIdx == 0 || (curIdx > 0 && isWhitespace(curValue[curIdx-1])) {
			m.editor.completionsOpen = true
			m.editor.completionsMode = completionsModeFile
			m.editor.completionsQuery = ""
			m.editor.completionsStartIndex = curIdx
			m.editor.completionsPositionStart = m.completionsPosition()
			depth, limit := m.com.Config().Options.TUI.Completions.Limits()
			m.editor.completions.SetMaxWidth(m.completionsMaxWidth())
			cmds = append(cmds, m.editor.completions.Open(depth, limit, m.loadMCPResourceCompletions))
		}
	}

	// Trigger command completions on "/" at the very start of an
	// otherwise-empty editor, mirroring opencode/Claude Code: a
	// "/" mid-message is just a character. Suppressed in bang
	// mode: a shell command is very plausibly an absolute path
	// like "/usr/bin/env", not a command trigger.
	if msg.String() == "/" && !m.editor.completionsOpen && !m.editor.bangMode && curValue == "" {
		m.editor.completionsOpen = true
		m.editor.completionsMode = completionsModeCommand
		m.editor.completionsQuery = ""
		m.editor.completionsStartIndex = curIdx
		m.editor.completionsPositionStart = m.completionsPosition()
		m.editor.completions.SetMaxWidth(m.completionsMaxWidth())
		m.editor.completions.OpenCommands(m.commandCompletionItems())
	}

	// remove the details if they are open when user starts typing
	if m.detailsOpen {
		m.detailsOpen = false
		m.updateLayoutAndSize()
	}

	prevHeight := m.editor.textarea.Height()
	cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))

	// Bang mode: enter when "!" is typed at the start of the
	// prompt, optionally preceded by whitespace (either on an
	// empty/whitespace-only prompt or prepended to existing text).
	// Exit on backspace clearing the last character.
	newVal := m.editor.textarea.Value()
	trimmedNew := strings.TrimLeftFunc(newVal, unicode.IsSpace)
	trimmedCur := strings.TrimLeftFunc(curValue, unicode.IsSpace)
	if !m.editor.bangMode && strings.HasPrefix(trimmedNew, "!") && !strings.HasPrefix(trimmedCur, "!") {
		m.editor.bangMode = true
		m.editor.bangWasEmpty = len(strings.TrimSpace(curValue)) == 0
		// Strip leading whitespace and the "!" from the textarea
		// while preserving the cursor position relative to the
		// command text.
		col := m.editor.textarea.Column()
		line := m.editor.textarea.Line()
		stripped := trimmedNew[1:]
		m.editor.textarea.SetValue(stripped)
		m.editor.textarea.SetCursorColumn(max(0, col-(len(newVal)-len(stripped))))
		_ = line // cursor line doesn't change; prefix removed
		m.setEditorPrompt(m.yoloModeCached())
	} else if m.editor.bangMode && newVal == "" && curValue != "" {
		// Just cleared last character; mark empty, stay in bang mode.
		m.editor.bangWasEmpty = true
	} else if m.editor.bangMode && newVal != "" {
		m.editor.bangWasEmpty = false
	}

	// Any text modification becomes the current draft.
	m.editor.updateHistoryDraft(curValue)

	// After updating textarea, check if we need to filter completions.
	// Skip filtering on the initial @ or / keystroke: for @ the
	// items are still loading async, and for / the query is
	// empty anyway (OpenCommands already populated the list).
	if m.editor.completionsOpen && msg.String() != "@" && msg.String() != "/" {
		newValue := m.editor.textarea.Value()
		newIdx := len(newValue)

		// Close completions if cursor moved before start.
		if newIdx <= m.editor.completionsStartIndex {
			m.editor.closeCompletions()
		} else if msg.String() == "space" {
			// Close on space.
			m.editor.closeCompletions()
		} else {
			// Extract current word and filter.
			triggerChar := "@"
			if m.editor.completionsMode == completionsModeCommand {
				triggerChar = "/"
			}
			word := m.editor.textareaWord()
			if strings.HasPrefix(word, triggerChar) {
				m.editor.completionsQuery = word[1:]
				m.editor.completions.Filter(m.editor.completionsQuery)
			} else if m.editor.completionsOpen {
				m.editor.closeCompletions()
			}
		}
	}
	return cmds
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
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
			break
		}
		m.focus = uiFocusEditor
		if cmd := m.newSession(); cmd != nil {
			cmds = append(cmds, cmd)
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
		if cmd := m.chat.ScrollByAndAnimate(-1); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if !m.chat.SelectedItemInView() {
			m.chat.SelectPrev()
			if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case key.Matches(msg, m.keyMap.Chat.Down):
		if cmd := m.chat.ScrollByAndAnimate(1); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if !m.chat.SelectedItemInView() {
			m.chat.SelectNext()
			if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
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
