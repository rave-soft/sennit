package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPromptHistoryStateNavigationPreservesDraftAndBounds(t *testing.T) {
	t.Parallel()

	state := promptHistoryState{messages: []string{"newest", "older"}, index: -1}

	entry, ok := state.previous("draft")
	require.True(t, ok)
	require.Equal(t, "newest", entry)
	require.Equal(t, "draft", state.draft)

	entry, ok = state.previous("changed draft must not replace saved draft")
	require.True(t, ok)
	require.Equal(t, "older", entry)

	_, ok = state.previous("ignored")
	require.False(t, ok)
	require.Equal(t, 1, state.index)

	entry, ok = state.next()
	require.True(t, ok)
	require.Equal(t, "newest", entry)

	entry, ok = state.next()
	require.True(t, ok)
	require.Equal(t, "draft", entry)
	require.Equal(t, -1, state.index)
}

func TestPromptHistoryStateLoadResetsNavigationAndGhostLookup(t *testing.T) {
	t.Parallel()

	state := promptHistoryState{
		messages:        []string{"old entry"},
		index:           0,
		draft:           "draft",
		ghostQuery:      "old",
		ghostSuggestion: "old entry",
	}

	state.load([]string{"new entry"})

	require.Equal(t, []string{"new entry"}, state.messages)
	require.Equal(t, -1, state.index)
	require.Empty(t, state.draft)
	require.Equal(t, "new entry", state.suggestionFor("new"))
}
