package model

import "strings"

// promptHistoryState owns navigation through prompt history and the memoized
// lookup used for its ghost suggestion. Entries are newest-first.
type promptHistoryState struct {
	messages        []string
	index           int
	draft           string
	ghostQuery      string
	ghostSuggestion string
}

// load replaces available history and exits navigation.
func (s *promptHistoryState) load(messages []string) {
	s.messages = messages
	s.reset()
	s.ghostQuery = ""
	s.ghostSuggestion = ""
}

// reset exits navigation and forgets the saved draft without changing entries.
func (s *promptHistoryState) reset() {
	s.index = -1
	s.draft = ""
}

// recordDraft exits navigation when the editor value changes.
func (s *promptHistoryState) recordDraft(oldValue, draft string) {
	if draft != oldValue {
		s.draft = draft
		s.index = -1
	}
}

// previous selects the next older entry, saving draft when navigation starts.
func (s *promptHistoryState) previous(draft string) (string, bool) {
	if len(s.messages) == 0 {
		return "", false
	}
	if s.index == -1 {
		s.draft = draft
	}
	next := s.index + 1
	if next >= len(s.messages) {
		return "", false
	}
	s.index = next
	return s.messages[next], true
}

// next selects the next newer entry, restoring the saved draft at the end.
func (s *promptHistoryState) next() (string, bool) {
	if s.index < 0 {
		return "", false
	}
	next := s.index - 1
	if next < 0 {
		s.index = -1
		return s.draft, true
	}
	s.index = next
	return s.messages[next], true
}

// restoreDraft exits navigation and returns the saved draft when active.
func (s *promptHistoryState) restoreDraft() (string, bool) {
	if s.index < 0 {
		return "", false
	}
	s.index = -1
	return s.draft, true
}

// suggestionFor returns the most recent entry that strictly extends value.
func (s *promptHistoryState) suggestionFor(value string) string {
	if value == s.ghostQuery {
		return s.ghostSuggestion
	}
	s.ghostQuery = value
	s.ghostSuggestion = ""
	if value != "" {
		for _, msg := range s.messages {
			if len(msg) > len(value) && strings.HasPrefix(msg, value) {
				s.ghostSuggestion = msg
				break
			}
		}
	}
	return s.ghostSuggestion
}
