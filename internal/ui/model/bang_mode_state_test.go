package model

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/stretchr/testify/require"
)

func TestBangModeStateLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("empty prompt requires a second backspace to exit", func(t *testing.T) {
		t.Parallel()
		state := bangModeState{}
		state.enter(true)

		require.True(t, state.isActive())
		require.True(t, state.exitOnEmptyBackspace())
		require.False(t, state.isActive())
		require.False(t, state.exitOnEmptyBackspace())
	})

	t.Run("editing tracks empty transitions", func(t *testing.T) {
		t.Parallel()
		state := bangModeState{}
		state.enter(true)

		state.updateEmpty("", "echo")
		require.False(t, state.wasEmpty)
		state.updateEmpty("echo", "")
		require.True(t, state.wasEmpty)
	})

	t.Run("leading prefix preserves rune cursor offset", func(t *testing.T) {
		t.Parallel()
		state := bangModeState{}
		input := textarea.New()
		input.SetValue("\u2003!echo")

		require.True(t, state.enterFromLeadingPrefix(&input, "", input.Column()))
		require.True(t, state.isActive())
		require.Equal(t, "echo", input.Value())
		require.Equal(t, 2, input.Column())
	})

	t.Run("cancel is cleared after invocation", func(t *testing.T) {
		t.Parallel()
		state := bangModeState{}
		ctx, cancel := context.WithCancel(context.Background())
		state.setCancel(cancel)

		require.True(t, state.cancelRunning())
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		require.False(t, state.isRunning())
		require.False(t, state.cancelRunning())
	})
}
