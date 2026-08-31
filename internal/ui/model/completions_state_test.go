package model

import (
	"image"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/stretchr/testify/require"
)

func newCompletionsLifecycleState() completionsLifecycleState {
	style := lipgloss.NewStyle()
	return completionsLifecycleState{popup: completions.New(completions.PopupStyles{
		Normal: style, Focused: style, Match: style, Muted: style, Border: style, ScrollbarThumb: style, ScrollbarTrack: style,
	})}
}

func TestCompletionsLifecycleOpenAndCloseResetState(t *testing.T) {
	t.Parallel()

	state := newCompletionsLifecycleState()
	anchor := image.Pt(7, 9)
	state.openCommands(3, anchor, 40, nil)

	require.True(t, state.open)
	require.Equal(t, completionsModeCommand, state.mode)
	require.Equal(t, 3, state.startIndex)
	require.Equal(t, anchor, state.anchor)
	require.True(t, state.popup.IsOpen())

	state.close()

	require.False(t, state.open)
	require.Equal(t, completionsModeFile, state.mode)
	require.Zero(t, state.startIndex)
	require.Empty(t, state.query)
	require.Zero(t, state.anchor)
	require.False(t, state.popup.IsOpen())
}

func TestCompletionsLifecycleUpdatesQueryAndClosesInvalidSpan(t *testing.T) {
	t.Parallel()

	state := newCompletionsLifecycleState()
	state.openCommands(2, image.Point{}, 40, []completions.CommandCompletionValue{{Title: "model"}})

	state.updateQuery(6, "/mod", false)
	require.True(t, state.open)
	require.Equal(t, "mod", state.query)
	require.Equal(t, "mod", state.popup.Query())

	state.updateQuery(2, "/mod", false)
	require.False(t, state.open, "cursor at the trigger closes the popup")
}

func TestCompletionsLifecycleReplacementRejectsInvalidUTF8Boundaries(t *testing.T) {
	t.Parallel()

	for name, startIndex := range map[string]int{
		"negative":     -1,
		"past end":     len("é @old") + 1,
		"inside UTF-8": 1,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			input := textarea.New()
			input.SetValue("é @old")
			input.MoveToEnd()
			state := newCompletionsLifecycleState()
			state.startIndex = startIndex

			require.False(t, state.replace(&input, "new"))
			require.Equal(t, "é @old", input.Value())
		})
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		input := textarea.New()
		input.SetValue("é @old")
		input.MoveToEnd()
		state := newCompletionsLifecycleState()
		state.startIndex = 3 // byte offset of @

		require.True(t, state.replace(&input, "new"))
		require.Equal(t, "é new ", input.Value())
	})
}
