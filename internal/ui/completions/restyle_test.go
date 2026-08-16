package completions

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

// TestSetStyles_RestylesHeldItems pins the reason SetStyles walks the items
// it already holds: each item copies the styles it was built with, so a
// theme switch while the popup is open would otherwise leave the rows in
// the old palette while the frame around them changed.
func TestSetStyles_RestylesHeldItems(t *testing.T) {
	t.Parallel()

	blue := lipgloss.NewStyle().Foreground(lipgloss.Color("#0000ff"))
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))

	c := New(PopupStyles{Normal: blue, Focused: blue, Match: blue, Muted: blue})
	c.SetItems([]FileCompletionValue{{Path: "main.go"}}, nil)

	item, ok := c.allItems[0].(*CompletionItem)
	require.True(t, ok)
	before := item.Render(40)

	c.SetStyles(PopupStyles{Normal: red, Focused: red, Match: red, Muted: red})

	require.NotEqual(t, before, item.Render(40), "item kept the styles it was built with")
}
