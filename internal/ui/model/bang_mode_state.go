package model

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
)

// bangModeState owns the editor lifecycle for bang (!) shell mode. It only
// mutates state supplied by the UI loop; shell execution remains in UI cmds.
type bangModeState struct {
	active   bool
	wasEmpty bool
	cancel   context.CancelFunc
}

func (b *bangModeState) isActive() bool {
	return b.active
}

func (b *bangModeState) isEmpty() bool {
	return b.wasEmpty
}

func (b *bangModeState) enter(wasEmpty bool) {
	b.active = true
	b.wasEmpty = wasEmpty
}

func (b *bangModeState) exit() {
	b.active = false
	b.wasEmpty = false
}

// exitOnEmptyBackspace leaves bang mode only after the prompt was already
// empty before the Backspace key was received.
func (b *bangModeState) exitOnEmptyBackspace() bool {
	if !b.active || !b.wasEmpty {
		return false
	}
	b.exit()
	return true
}

// updateEmpty records a bang prompt's empty/non-empty transition after an
// edit has changed its textarea value.
func (b *bangModeState) updateEmpty(previous, current string) {
	if !b.active {
		return
	}
	if current == "" && previous != "" {
		b.wasEmpty = true
	} else if current != "" {
		b.wasEmpty = false
	}
}

// enterFromLeadingPrefix engages bang mode for a newly introduced leading
// bang prefix, removes the prefix and leading whitespace, and preserves the
// cursor's rune-column relative to the command text.
func (b *bangModeState) enterFromLeadingPrefix(textarea *textarea.Model, previous string, cursorColumn int) bool {
	if b.active {
		return false
	}
	current := textarea.Value()
	trimmedCurrent := strings.TrimLeftFunc(current, unicode.IsSpace)
	trimmedPrevious := strings.TrimLeftFunc(previous, unicode.IsSpace)
	if !strings.HasPrefix(trimmedCurrent, "!") || strings.HasPrefix(trimmedPrevious, "!") {
		return false
	}

	stripped := trimmedCurrent[1:] // ASCII bang prefix.
	textarea.SetValue(stripped)
	column := textarea.Column()
	if previous != "" {
		column = cursorColumn
	}
	prefixRunes := utf8.RuneCountInString(current) - utf8.RuneCountInString(stripped)
	textarea.SetCursorColumn(max(0, column-prefixRunes))
	b.enter(len(strings.TrimSpace(previous)) == 0)
	b.updateEmpty(previous, stripped)
	return true
}

// syncFromTextarea restores the bang/plain lifecycle when a complete draft or
// history entry is assigned to the textarea.
func (b *bangModeState) syncFromTextarea(textarea *textarea.Model) {
	value := textarea.Value()
	if strings.HasPrefix(value, "!") {
		stripped := strings.TrimPrefix(value, "!")
		textarea.SetValue(stripped)
		textarea.MoveToBegin()
		b.enter(stripped == "")
		return
	}
	b.exit()
}

func (b *bangModeState) draftValue(value string) string {
	if b.active {
		return "!" + value
	}
	return value
}

func (b *bangModeState) setCancel(cancel context.CancelFunc) {
	b.cancel = cancel
}

func (b *bangModeState) isRunning() bool {
	return b.cancel != nil
}

// cancelRunning invokes and clears the current command cancellation function.
func (b *bangModeState) cancelRunning() bool {
	if b.cancel == nil {
		return false
	}
	b.cancel()
	b.cancel = nil
	return true
}
