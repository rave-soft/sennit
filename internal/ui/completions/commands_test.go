package completions

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

func TestOpenCommandsAndFilter(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new", Aliases: []string{"new session", "clear"}},
		{ID: "summarize", Title: "compact", Aliases: []string{"summarize", "summarize session"}},
		{ID: "switch_model", Title: "models", Aliases: []string{"switch model"}},
	})

	require.True(t, c.IsOpen())
	require.True(t, c.HasItems())
	require.Len(t, c.filtered, 3)

	// Filtering by an alias still surfaces the short title.
	c.Filter("clear")
	require.NotEmpty(t, c.filtered)
	first, ok := c.filtered[0].(*CompletionItem)
	require.True(t, ok)
	require.Equal(t, "new", first.Text())
}

func TestCommandsEnterExecutesSelection(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "summarize", Title: "compact", Action: "summarize-action"},
	})

	msg, ok := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, ok)

	sel, ok := msg.(SelectionMsg[CommandCompletionValue])
	require.True(t, ok)
	require.False(t, sel.InsertOnly)
	require.Equal(t, "compact", sel.Value.Title)
	require.Equal(t, "summarize-action", sel.Value.Action)
	require.False(t, c.IsOpen(), "Enter closes the popup")
}

func TestCommandsTabInsertsNameOnly(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "summarize", Title: "compact", Action: "summarize-action"},
	})

	msg, ok := c.Update(tea.KeyPressMsg{Code: tea.KeyTab, Text: "tab"})
	require.True(t, ok)

	sel, ok := msg.(SelectionMsg[CommandCompletionValue])
	require.True(t, ok)
	require.True(t, sel.InsertOnly)
	require.Equal(t, "compact", sel.Value.Title)
}

func TestOpenCommandsSizesWidthToLongestTitleAndDescription(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new", Description: "start a new session"},
		{ID: "compact", Title: "compact", Description: "x"},
	})

	// Width must fit the longest "title (description)" pair (the "new"
	// item, whose description is the longest), not just the longest bare
	// title — a plain Text()-based measurement would pick "compact"
	// (7 cols) and clip "new"'s description entirely.
	want := len("new (start a new session)") + 2 // padding
	require.Equal(t, want, c.width)

	w, _ := c.Size()
	require.Equal(t, want, w)
}

func TestSetMaxWidthCapsPopup(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.SetMaxWidth(15)
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new", Description: "start a brand new session with a very long description"},
	})

	require.LessOrEqual(t, c.width, 15)
	require.GreaterOrEqual(t, c.width, minWidth)
}

func TestFilterDoesNotShrinkWidth(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new", Description: "start a new session"},
		{ID: "compact", Title: "compact", Description: "x"},
	})
	widthAtOpen := c.width

	// Filtering down to the item with the shortest label must not shrink
	// the popup — it stays fixed at whatever was widest at open time, so
	// the popup doesn't visibly jitter while the user types.
	c.Filter("compact")
	require.Len(t, c.filtered, 1)
	require.Equal(t, widthAtOpen, c.width)
}

func TestCommandsEscClosesWithoutSelecting(t *testing.T) {
	t.Parallel()

	c := New(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new"},
	})

	msg, ok := c.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	require.True(t, ok)
	require.IsType(t, ClosedMsg{}, msg)
	require.False(t, c.IsOpen())
}
