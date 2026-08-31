package model

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textarea"

	"github.com/rave-soft/sennit/internal/ui/attachments"
)

// editorState holds the prompt editor's own state: the textarea, the
// attachment list, the completions popup bookkeeping, bang (!) shell-mode
// flags, and prompt history navigation.
//
// Most of the logic that touches these fields still lives on *UI (in ui.go
// and history.go) because it also reads layout, session, dialog, or
// workspace state. Only the handful of methods below operate exclusively on
// this state.
type editorState struct {
	// textarea is the prompt input widget.
	textarea textarea.Model

	// readyPlaceholder / workingPlaceholder are the randomized placeholder
	// texts shown in the textarea when idle / when the agent is busy — see
	// randomizePlaceholders (editor_input.go) and the placeholder switch at
	// the end of UI.Update.
	readyPlaceholder   string
	workingPlaceholder string

	// attachments is the file/text attachment list shown above the editor.
	attachments *attachments.Attachments

	// completions owns the @-mention / "/"-command popup lifecycle.
	completions completionsLifecycleState

	// bangMode tracks whether the editor is in bang (!) shell mode.
	bangMode     bool
	bangWasEmpty bool // true when bang prompt became empty on last keystroke

	// pendingSendState owns sends accepted by the editor until their target
	// session accepts them. See send_state.go.
	pendingSend pendingSendState

	// bangCancel cancels a running bang-mode shell command. Nil when no
	// bang command is in progress. Set by runShellCommand, cleared by
	// shellResultMsg. Checked by isAgentBusy and cancelAgent so that
	// Escape works for bang commands the same way it does for agent runs.
	bangCancel context.CancelFunc

	// history owns prompt navigation and history-derived ghost suggestions.
	promptHistory promptHistoryState

	// ghostHiddenFor holds the textarea value at the moment Esc hid the
	// suggestion. The hide only applies while the value is unchanged.
	ghostHiddenFor string

	// lastKeyWasEsc tracks whether the immediately preceding key event was
	// Escape, so a second consecutive Esc can clear the draft outright
	// (see the Editor.Escape case in UI.handleKeyPressMsg). Reset by any
	// other key.
	lastKeyWasEsc bool
}

// textareaWord returns the current word at the cursor position.
func (e *editorState) textareaWord() string {
	return e.textarea.Word()
}

// textareaCursorOffset returns the byte offset of the cursor within
// Value(). Line() gives the row and Column() a rune index into that row,
// so the row is re-sliced by rune to translate that into a byte count —
// using len() on the row directly would double-count any multi-byte rune
// before the cursor.
func (e *editorState) textareaCursorOffset() int {
	value := e.textarea.Value()
	lines := strings.Split(value, "\n")
	line := min(max(e.textarea.Line(), 0), len(lines)-1)
	if line < 0 {
		return 0
	}
	offset := 0
	for i := range line {
		offset += len(lines[i]) + 1 // +1 for the newline joining rows.
	}
	row := []rune(lines[line])
	col := min(e.textarea.Column(), len(row))
	return offset + len(string(row[:col]))
}

// isAtEditorStart returns true if we are at the 0 line and 0 col in the textarea.
func (e *editorState) isAtEditorStart() bool {
	return e.textarea.Line() == 0 && e.textarea.LineInfo().ColumnOffset == 0
}

// isAtEditorEnd returns true if we are in the last line and the last column in the textarea.
func (e *editorState) isAtEditorEnd() bool {
	lineCount := e.textarea.LineCount()
	if lineCount == 0 {
		return true
	}
	if e.textarea.Line() != lineCount-1 {
		return false
	}
	info := e.textarea.LineInfo()
	return info.CharOffset >= info.CharWidth-1 || info.CharWidth == 0
}

// updateHistoryDraft updates history state when text is modified.
func (e *editorState) updateHistoryDraft(oldValue string) {
	if e.textarea.Value() != oldValue {
		e.promptHistory.recordDraft(oldValue, e.draftValue())
	}
}

// draftValue returns the textarea's value as it should be captured into
// promptHistory.draft: with a leading "!" restored if bang mode is active,
// matching how bang commands are stored in promptHistory.messages (see
// loadPromptHistory) and decoded back by syncBangModeFromTextarea. Without
// this, browsing away from an in-progress bang command and back (Up then
// Esc) would silently drop out of bang mode — the textarea's raw Value()
// never carries the "!" once it's been stripped on entry.
func (e *editorState) draftValue() string {
	if e.bangMode {
		return "!" + e.textarea.Value()
	}
	return e.textarea.Value()
}

// historyReset resets the history, but does not clear the message
// it just sets the current draft to empty and the position in the history.
func (e *editorState) historyReset() {
	e.promptHistory.reset()
}

// ghostSuggestionFor returns the most recent prompt-history entry that
// starts with value, or "" if none matches. Memoized by value: repeated
// calls with the same value (e.g. multiple Draw calls in one frame) don't
// rescan history. History is ordered newest-first (index 0 = most recent,
// see historyPrev), so the first prefix match found scanning forward is the
// one to use.
func (e *editorState) ghostSuggestionFor(value string) string {
	return e.promptHistory.suggestionFor(value)
}
