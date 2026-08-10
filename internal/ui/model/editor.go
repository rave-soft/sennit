package model

import (
	"context"
	"image"
	"strings"

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

	// completions is the @-mention / "/"-command completions popup; the
	// fields below track its open/mode/query/anchor state as the user
	// types.
	completions              *completions.Completions
	completionsOpen          bool
	completionsMode          completionsMode
	completionsStartIndex    int
	completionsQuery         string
	completionsPositionStart image.Point // x,y where user typed '@' or '/'

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

	// ghostQuery/ghostSuggestion memoize the last prefix-scan result so
	// Draw() (called every frame) doesn't rescan history when the input
	// hasn't changed since the last computation.
	ghostQuery      string
	ghostSuggestion string

	// ghostHiddenFor holds the textarea value at the moment Esc hid the
	// suggestion. The hide only applies while the value is unchanged;
	// typing anything (which changes the value) implicitly un-hides it,
	// so no separate invalidation call is needed anywhere else.
	ghostHiddenFor string

	// lastKeyWasEsc tracks whether the immediately preceding key event was
	// Escape, so a second consecutive Esc can clear the draft outright
	// (see the Editor.Escape case in UI.handleKeyPressMsg). Reset by any
	// other key.
	lastKeyWasEsc bool
}

// completionsMode selects what the completions popup is currently offering:
// "@" file/resource mentions, or "/" commands.
type completionsMode int

const (
	completionsModeFile completionsMode = iota
	completionsModeCommand
)

// closeCompletions closes the completions popup and resets state.
func (e *editorState) closeCompletions() {
	e.completionsOpen = false
	e.completionsMode = completionsModeFile
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
		e.promptHistory.draft = e.draftValue()
		e.promptHistory.index = -1
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
	e.promptHistory.index = -1
	e.promptHistory.draft = ""
}

// ghostSuggestionFor returns the most recent prompt-history entry that
// starts with value, or "" if none matches. Memoized by value: repeated
// calls with the same value (e.g. multiple Draw calls in one frame) don't
// rescan history. History is ordered newest-first (index 0 = most recent,
// see historyPrev), so the first prefix match found scanning forward is the
// one to use.
func (e *editorState) ghostSuggestionFor(value string) string {
	if value == e.ghostQuery {
		return e.ghostSuggestion
	}
	e.ghostQuery = value
	e.ghostSuggestion = ""
	if value != "" {
		for _, msg := range e.promptHistory.messages {
			if len(msg) > len(value) && strings.HasPrefix(msg, value) {
				e.ghostSuggestion = msg
				break
			}
		}
	}
	return e.ghostSuggestion
}
