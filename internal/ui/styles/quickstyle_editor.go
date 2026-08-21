package styles

import (
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// quickStyleEditor fills in TextInput and the prompt editor's textarea and
// prompt/question styles.
func quickStyleEditor(s *Styles, o quickStyleOpts, base, _, _ lipgloss.Style) {
	s.TextInput = textinput.Styles{
		Focused: textinput.StyleState{
			Text:        base,
			Placeholder: base.Foreground(o.fgMostSubtle),
			Prompt:      base.Foreground(o.accent),
			Suggestion:  base.Foreground(o.fgMostSubtle),
		},
		Blurred: textinput.StyleState{
			Text:        base.Foreground(o.fgMoreSubtle),
			Placeholder: base.Foreground(o.fgMostSubtle),
			Prompt:      base.Foreground(o.fgMoreSubtle),
			Suggestion:  base.Foreground(o.fgMostSubtle),
		},
		Cursor: textinput.CursorStyle{
			Color: o.secondary,
			Shape: tea.CursorBlock,
			Blink: true,
		},
	}

	s.Editor.Textarea = textarea.Styles{
		Focused: textarea.StyleState{
			Base:             base,
			Text:             base,
			LineNumber:       base.Foreground(o.fgMostSubtle),
			CursorLine:       base,
			CursorLineNumber: base.Foreground(o.fgMostSubtle),
			Placeholder:      base.Foreground(o.fgMostSubtle),
			Prompt:           base.Foreground(o.accent),
		},
		Blurred: textarea.StyleState{
			Base:             base,
			Text:             base.Foreground(o.fgMoreSubtle),
			LineNumber:       base.Foreground(o.fgMoreSubtle),
			CursorLine:       base,
			CursorLineNumber: base.Foreground(o.fgMoreSubtle),
			Placeholder:      base.Foreground(o.fgMostSubtle),
			Prompt:           base.Foreground(o.fgMoreSubtle),
		},
		Cursor: textarea.CursorStyle{
			Color: o.secondary,
			Shape: tea.CursorBlock,
			Blink: true,
		},
	}

	// Editor
	s.Editor.PromptNormalFocused = lipgloss.NewStyle().Foreground(o.successMostSubtle).SetString("::: ")
	s.Editor.PromptNormalBlurred = s.Editor.PromptNormalFocused.Foreground(o.fgMoreSubtle)
	s.Editor.PromptYoloIconFocused = lipgloss.NewStyle().MarginRight(1).Foreground(o.fgMostSubtle).Background(o.busy).Bold(true).SetString(" Y ")
	s.Editor.PromptYoloIconBlurred = s.Editor.PromptYoloIconFocused.Foreground(o.bgBase).Background(o.fgMoreSubtle)
	s.Editor.PromptYoloDotsFocused = lipgloss.NewStyle().MarginRight(1).Foreground(o.warningSubtle).SetString(":::")
	s.Editor.PromptYoloDotsBlurred = s.Editor.PromptYoloDotsFocused.Foreground(o.fgMoreSubtle)
	s.Editor.PromptBangIconFocused = lipgloss.NewStyle().MarginRight(1).Foreground(o.onPrimary).Background(o.primary).Bold(true).SetString(" ! ")
	s.Editor.PromptBangIconBlurred = s.Editor.PromptBangIconFocused.Foreground(o.bgBase).Background(o.fgMoreSubtle)
	s.Editor.PromptBangDotsFocused = lipgloss.NewStyle().MarginRight(1).Foreground(o.primary).SetString(":::")
	s.Editor.PromptBangDotsBlurred = s.Editor.PromptBangDotsFocused.Foreground(o.fgMoreSubtle)
	s.Editor.PromptQuestionIconFocused = lipgloss.NewStyle().MarginRight(1).Foreground(o.fgBase).Background(o.primary).Bold(true).SetString(" ? ")
	s.Editor.PromptQuestionIconBlurred = s.Editor.PromptQuestionIconFocused.Foreground(o.bgBase).Background(o.fgMoreSubtle)
	s.Editor.QuestionSelected = lipgloss.NewStyle().Foreground(o.secondary).Bold(true)
	s.Editor.QuestionUnselected = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Editor.QuestionBody = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Editor.QuestionConfirm = lipgloss.NewStyle().Foreground(o.primary).Bold(true)
	s.Editor.QuestionNote = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Editor.QuestionCursorBar = lipgloss.NewStyle().Foreground(o.secondary)
	s.Editor.QuestionRadioOn = lipgloss.NewStyle().Foreground(o.secondary).SetString(RadioOn)
	s.Editor.QuestionRadioOff = lipgloss.NewStyle().Foreground(o.fgSubtle).SetString(RadioOff)
	s.Editor.QuestionCheckOn = lipgloss.NewStyle().Foreground(o.secondary).SetString(RadioOn)
	s.Editor.QuestionCheckOff = lipgloss.NewStyle().Foreground(o.fgSubtle).SetString(RadioOff)
}
