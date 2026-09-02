package model

import (
	"log/slog"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// promptHistoryLoadedMsg is sent when prompt history is loaded.
//
// uiOwned: dispatched by sessionState.loadPromptHistory, from several
// places across the package. Routed by active screen instead, a thread's
// own history load could replace the main screen's prompt history (or
// vice versa) instead of applying to the sessionState that asked for it.
type promptHistoryLoadedMsg struct {
	uiOwned

	messages []string
}

// loadPromptHistory loads user messages for history navigation.
func (s *sessionState) loadPromptHistory(com *common.Common, owner *UI) tea.Cmd {
	ctx := com.Context()
	ws := com.Workspace

	// Snapshot session state up front: the closure below runs on the cmd
	// goroutine and must not read the session state off the Update loop.
	hasSession := s.current != nil
	var sessionID string
	if hasSession {
		sessionID = s.current.ID
	}

	return func() tea.Msg {
		var messages []message.Message
		var err error

		if hasSession {
			messages, err = ws.ListUserMessages(ctx, sessionID)
		} else {
			messages, err = ws.ListAllUserMessages(ctx)
		}
		if err != nil {
			slog.Error("Failed to load prompt history", "error", err)
			return promptHistoryLoadedMsg{uiOwned: uiOwned{owner: owner}, messages: nil}
		}

		// The SQL layer already excludes child-session (sub-agent) prompts;
		// the init prompt is the one machine-generated message that lands
		// in a top-level session as role=user, so it is filtered here by
		// its template prefix.
		initPrefix := ""
		if tpl, tplErr := ws.InitializePrompt(); tplErr == nil {
			initPrefix = firstTemplateLine(tpl)
		}

		texts := make([]string, 0, len(messages))
		for _, msg := range messages {
			if text := msg.Content().Text; text != "" {
				if initPrefix == "" || !strings.HasPrefix(text, initPrefix) {
					texts = append(texts, text)
				}
			}
			for _, sc := range msg.ShellCommands() {
				texts = append(texts, "!"+sc.Command)
			}
		}
		return promptHistoryLoadedMsg{uiOwned: uiOwned{owner: owner}, messages: texts}
	}
}

// handleHistoryUp handles up arrow for history navigation.
func (m *UI) handleHistoryUp(msg tea.Msg) tea.Cmd {
	prevHeight := m.editor.textarea.Height()
	// Navigate to older history entry from cursor position (0,0).
	if m.editor.textarea.Length() == 0 || m.editor.isAtEditorStart() {
		if m.historyPrev() {
			// we send this so that the textarea moves the view to the correct position
			// without this the cursor will show up in the wrong place.
			return m.updateTextareaWithPrevHeight(nil, prevHeight)
		}
	}

	// First move cursor to start before entering history.
	if m.editor.textarea.Line() == 0 {
		m.editor.textarea.CursorStart()
		return nil
	}

	// Let textarea handle normal cursor movement.
	return m.updateTextarea(msg)
}

// handleHistoryDown handles down arrow for history navigation.
func (m *UI) handleHistoryDown(msg tea.Msg) tea.Cmd {
	prevHeight := m.editor.textarea.Height()
	// Navigate to newer history entry from end of text.
	if m.editor.isAtEditorEnd() {
		if m.historyNext() {
			// we send this so that the textarea moves the view to the correct position
			// without this the cursor will show up in the wrong place.
			return m.updateTextareaWithPrevHeight(nil, prevHeight)
		}
	}

	// First move cursor to end before navigating history.
	if m.editor.textarea.Line() == max(m.editor.textarea.LineCount()-1, 0) {
		m.editor.textarea.MoveToEnd()
		return m.updateTextarea(nil)
	}

	// Let textarea handle normal cursor movement.
	return m.updateTextarea(msg)
}

// handleHistoryEscape handles escape for exiting history navigation.
func (m *UI) handleHistoryEscape(msg tea.Msg) tea.Cmd {
	prevHeight := m.editor.textarea.Height()
	// Return to current draft when browsing history.
	if draft, ok := m.editor.promptHistory.restoreDraft(); ok {
		m.editor.textarea.Reset()
		m.editor.textarea.InsertString(draft)
		m.syncBangModeFromTextarea()
		return m.updateTextareaWithPrevHeight(nil, prevHeight)
	}

	// Let textarea handle escape normally.
	return m.updateTextarea(msg)
}

// syncBangModeFromTextarea engages or disengages bang mode based on
// whether the current textarea value starts with "!". The "!" prefix
// is stripped when entering bang mode and re-added when leaving it so
// the visible text always reflects the correct state.
func (m *UI) syncBangModeFromTextarea() {
	m.editor.bang.syncFromTextarea(&m.editor.textarea)
	m.setEditorPrompt(m.yoloModeCached())
}

// historyPrev changes the text area content to the previous message in the history
// it returns false if it could not find the previous message.
func (m *UI) historyPrev() bool {
	entry, ok := m.editor.promptHistory.previous(m.editor.draftValue())
	if !ok {
		return false
	}
	m.editor.textarea.Reset()
	m.editor.textarea.InsertString(entry)
	m.editor.textarea.MoveToBegin()
	m.syncBangModeFromTextarea()
	return true
}

// historyNext changes the text area content to the next message in the history
// it returns false if it could not find the next message.
func (m *UI) historyNext() bool {
	entry, ok := m.editor.promptHistory.next()
	if !ok {
		return false
	}
	m.editor.textarea.Reset()
	m.editor.textarea.InsertString(entry)
	m.syncBangModeFromTextarea()
	return true
}

// firstTemplateLine returns the first non-empty line of a prompt template,
// used as a stable prefix to recognize (and hide from prompt history)
// messages the UI generated itself rather than the user typing them.
func firstTemplateLine(tpl string) string {
	for line := range strings.SplitSeq(tpl, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
