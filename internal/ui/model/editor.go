package model

import (
	"context"
	"image"

	"charm.land/bubbles/v2/textarea"

	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/completions"
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

	// attachments is the file/text attachment list shown above the editor.
	attachments *attachments.Attachments

	// completions is the @-mention completions popup; the fields below
	// track its open/query/anchor state as the user types.
	completions              *completions.Completions
	completionsOpen          bool
	completionsStartIndex    int
	completionsQuery         string
	completionsPositionStart image.Point // x,y where user typed '@'

	// bangMode tracks whether the editor is in bang (!) shell mode.
	bangMode     bool
	bangWasEmpty bool // true when bang prompt became empty on last keystroke

	// pendingBangCommand holds a shell command that was issued before
	// the session finished loading. The loadSessionMsg handler creates
	// the pending UI item and starts execution once the chat list is
	// stable, eliminating races between session load and shell output.
	pendingBangCommand string

	// bangCancel cancels a running bang-mode shell command. Nil when no
	// bang command is in progress. Set by runShellCommand, cleared by
	// shellResultMsg. Checked by isAgentBusy and cancelAgent so that
	// Escape works for bang commands the same way it does for agent runs.
	bangCancel context.CancelFunc

	// promptHistory holds up/down navigation state through previous
	// messages.
	promptHistory struct {
		messages []string
		index    int
		draft    string
	}
}

// closeCompletions closes the completions popup and resets state.
func (e *editorState) closeCompletions() {
	e.completionsOpen = false
	e.completionsQuery = ""
	e.completionsStartIndex = 0
	e.completions.Close()
}

// textareaWord returns the current word at the cursor position.
func (e *editorState) textareaWord() string {
	return e.textarea.Word()
}

// insertCompletionText replaces the @query in the textarea with the given text.
// Returns false if the replacement cannot be performed.
func (e *editorState) insertCompletionText(text string) bool {
	value := e.textarea.Value()
	if e.completionsStartIndex > len(value) {
		return false
	}

	word := e.textareaWord()
	endIdx := min(e.completionsStartIndex+len(word), len(value))
	newValue := value[:e.completionsStartIndex] + text + value[endIdx:]
	e.textarea.SetValue(newValue)
	e.textarea.MoveToEnd()
	e.textarea.InsertRune(' ')
	return true
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
		e.promptHistory.draft = e.textarea.Value()
		e.promptHistory.index = -1
	}
}

// historyReset resets the history, but does not clear the message
// it just sets the current draft to empty and the position in the history.
func (e *editorState) historyReset() {
	e.promptHistory.index = -1
	e.promptHistory.draft = ""
}
