package model

// initializeSelectionState owns the Yes/No selection in the project
// initialization prompt. Its zero value selects Yes.
type initializeSelectionState struct {
	noSelected bool
}

func (s *initializeSelectionState) yesSelected() bool {
	return !s.noSelected
}

func (s *initializeSelectionState) toggle() {
	s.noSelected = !s.noSelected
}
