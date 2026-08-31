package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEditorEscapeStateEscapeSequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(*testing.T, *editorEscapeState)
	}{
		{
			name: "first escape starts sequence",
			run: func(t *testing.T, state *editorEscapeState) {
				require.False(t, state.escape())
			},
		},
		{
			name: "second escape is consecutive",
			run: func(t *testing.T, state *editorEscapeState) {
				require.False(t, state.escape())
				require.True(t, state.escape())
			},
		},
		{
			name: "non-escape breaks sequence",
			run: func(t *testing.T, state *editorEscapeState) {
				require.False(t, state.escape())
				state.nonEscape()
				require.False(t, state.escape())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.run(t, &editorEscapeState{})
		})
	}
}

func TestEditorEscapeStateHidesGhostForSpecificValue(t *testing.T) {
	t.Parallel()

	state := editorEscapeState{}
	state.hideGhostFor("hello")

	require.True(t, state.hidesGhostFor("hello"))
	require.False(t, state.hidesGhostFor("hello world"))
}
