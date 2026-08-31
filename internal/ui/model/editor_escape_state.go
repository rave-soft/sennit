package model

// editorEscapeState owns editor behavior that spans consecutive Escape key
// presses and value-specific ghost-suggestion suppression.
type editorEscapeState struct {
	lastKeyWasEsc  bool
	ghostHiddenFor string
}

// escape records an Escape key press and reports whether it immediately
// follows another Escape key press.
func (s *editorEscapeState) escape() bool {
	consecutive := s.lastKeyWasEsc
	s.lastKeyWasEsc = true
	return consecutive
}

// nonEscape breaks an Escape key sequence.
func (s *editorEscapeState) nonEscape() {
	s.lastKeyWasEsc = false
}

// hideGhostFor suppresses the ghost suggestion while the textarea retains
// value.
func (s *editorEscapeState) hideGhostFor(value string) {
	s.ghostHiddenFor = value
}

func (s *editorEscapeState) hidesGhostFor(value string) bool {
	return s.ghostHiddenFor == value
}
