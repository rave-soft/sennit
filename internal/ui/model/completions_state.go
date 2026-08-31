package model

import (
	"image"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/completions"
)

// completionsLifecycleState owns the editor completion popup and the state
// that makes a popup meaningful: its source, replacement start, filter query,
// and screen anchor. UI remains the sole Bubble Tea model; it supplies the
// context-specific completion items and routes the popup's imperative API.
type completionsLifecycleState struct {
	popup      *completions.Completions
	open       bool
	mode       completionsMode
	startIndex int
	query      string
	anchor     image.Point
}

// completionsMode selects what the completions popup is currently offering:
// "@" file/resource mentions, or "/" commands.
type completionsMode int

const (
	completionsModeFile completionsMode = iota
	completionsModeCommand
)

func (s *completionsLifecycleState) openFiles(startIndex int, anchor image.Point, maxWidth, depth, limit int, loadResources func() []completions.ResourceCompletionValue) tea.Cmd {
	s.open = true
	s.mode = completionsModeFile
	s.query = ""
	s.startIndex = startIndex
	s.anchor = anchor
	s.popup.SetMaxWidth(maxWidth)
	return s.popup.Open(depth, limit, loadResources)
}

func (s *completionsLifecycleState) openCommands(startIndex int, anchor image.Point, maxWidth int, items []completions.CommandCompletionValue) {
	s.open = true
	s.mode = completionsModeCommand
	s.query = ""
	s.startIndex = startIndex
	s.anchor = anchor
	s.popup.SetMaxWidth(maxWidth)
	s.popup.OpenCommands(items)
}

// close resets every lifecycle value, including the anchor, so a later popup
// can never be positioned or filtered using stale state.
func (s *completionsLifecycleState) close() {
	s.open = false
	s.mode = completionsModeFile
	s.query = ""
	s.startIndex = 0
	s.anchor = image.Point{}
	if s.popup != nil {
		s.popup.Close()
	}
}

func (s *completionsLifecycleState) setItems(files []completions.FileCompletionValue, resources []completions.ResourceCompletionValue) {
	if s.open && s.popup != nil {
		s.popup.SetItems(files, resources)
	}
}

// updateQuery reconciles a changed textarea with the active completion span.
// cursorOffset is a byte offset, matching startIndex and preserving UTF-8 and
// mid-buffer completion anchoring.
func (s *completionsLifecycleState) updateQuery(cursorOffset int, word string, insertedSpace bool) {
	if !s.open {
		return
	}
	if cursorOffset <= s.startIndex || insertedSpace {
		s.close()
		return
	}
	trigger := "@"
	if s.mode == completionsModeCommand {
		trigger = "/"
	}
	if !strings.HasPrefix(word, trigger) {
		s.close()
		return
	}
	s.query = word[len(trigger):]
	s.popup.Filter(s.query)
}

// replace swaps the trigger word for text and appends the existing completion
// separator. Invalid byte boundaries are rejected rather than slicing the
// textarea value incorrectly.
func (s *completionsLifecycleState) replace(input *textarea.Model, text string) bool {
	value := input.Value()
	if s.startIndex < 0 || s.startIndex > len(value) {
		return false
	}
	word := input.Word()
	endIndex := min(s.startIndex+len(word), len(value))
	if !utf8Boundary(value, s.startIndex) || !utf8Boundary(value, endIndex) {
		return false
	}
	input.SetValue(value[:s.startIndex] + text + value[endIndex:])
	input.MoveToEnd()
	input.InsertRune(' ')
	return true
}

func utf8Boundary(value string, index int) bool {
	return index == 0 || index == len(value) || (index > 0 && index < len(value) && (value[index]&0xc0) != 0x80)
}
