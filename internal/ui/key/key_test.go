package key

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func TestMatchesUsesBaseCodeForRussianLayout(t *testing.T) {
	t.Parallel()

	binding := NewBinding(WithKeys("ctrl+p"))
	msg := tea.KeyPressMsg(tea.Key{
		Code:     'з',
		Text:     "з",
		Mod:      tea.ModCtrl,
		BaseCode: 'p',
	})

	require.True(t, Matches(msg, binding))
}

func TestMatchesUsesBaseCodeForShiftedShortcut(t *testing.T) {
	t.Parallel()

	binding := NewBinding(WithKeys("G"))
	msg := tea.KeyPressMsg(tea.Key{
		Code:     'П',
		Text:     "П",
		Mod:      tea.ModShift,
		BaseCode: 'g',
	})

	require.True(t, Matches(msg, binding))
}

func TestMatchesStringUsesBaseCodeForRussianLayout(t *testing.T) {
	t.Parallel()

	msg := tea.KeyPressMsg(tea.Key{Code: 'с', Text: "с", BaseCode: 'c'})

	require.True(t, MatchesString(msg, "c"))
}

func TestMatchesUsesBaseCodeWithCapsLock(t *testing.T) {
	t.Parallel()

	binding := NewBinding(WithKeys("G"))
	msg := tea.KeyPressMsg(tea.Key{
		Code:     'П',
		Text:     "П",
		Mod:      tea.ModCapsLock,
		BaseCode: 'g',
	})

	require.True(t, Matches(msg, binding))
}

func TestMatchesUsesLowercaseWithShiftAndCapsLock(t *testing.T) {
	t.Parallel()

	binding := NewBinding(WithKeys("g"))
	msg := tea.KeyPressMsg(tea.Key{
		Code:     'п',
		Text:     "п",
		Mod:      tea.ModShift | tea.ModCapsLock,
		BaseCode: 'g',
	})

	require.True(t, Matches(msg, binding))
}

func TestMatchesDoesNotInferLayoutWithoutBaseCode(t *testing.T) {
	t.Parallel()

	binding := NewBinding(WithKeys("ctrl+p"))
	msg := tea.KeyPressMsg(tea.Key{Code: 'з', Text: "з", Mod: tea.ModCtrl})

	require.False(t, Matches(msg, binding))
}
