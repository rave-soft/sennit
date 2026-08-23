package completions

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

// TestCompletionItem_RenderPopulatesWidthCache is a regression test
// for a dead per-width cache: renderItem's nil-guard (`if cache ==
// nil { cache = make(map[int]string) }`) reassigns its own local
// parameter, not the caller's field, so a CompletionItem whose cache
// field started nil (the pre-fix constructor never initialized it)
// never actually cached anything — every render rebuilt the content
// from scratch and the write on the last line of renderItem landed in
// a map that was thrown away when the call returned.
func TestCompletionItem_RenderPopulatesWidthCache(t *testing.T) {
	t.Parallel()

	item := NewCompletionItem(
		"internal/ui/chat/user.go",
		FileCompletionValue{Path: "internal/ui/chat/user.go"},
		lipgloss.NewStyle(),
		lipgloss.NewStyle(),
		lipgloss.NewStyle(),
	)

	out := item.Render(40)
	require.NotEmpty(t, out)
	require.Contains(t, item.cache, 40, "Render must write its output back into the item's own cache field")
	require.Equal(t, out, item.cache[40])
}

// TestCompletionItem_CacheSurvivesInvalidation covers the other half
// of the same defect: mutators used to reset the cache field to nil
// outright (c.cache = nil), which is fine for the immediate render
// that follows (the nil-guard papers over it once), but a *second*
// invalidation-then-render cycle would again write into a throwaway
// local map, so the cache never held more than a single entry's worth
// of benefit across the item's lifetime. Clearing in place keeps the
// field itself usable indefinitely.
func TestCompletionItem_CacheSurvivesInvalidation(t *testing.T) {
	t.Parallel()

	item := NewCompletionItem(
		"a-completion-item",
		FileCompletionValue{Path: "a-completion-item"},
		lipgloss.NewStyle(),
		lipgloss.NewStyle(),
		lipgloss.NewStyle(),
	)

	_ = item.Render(30)
	require.Contains(t, item.cache, 30)

	item.SetFocused(true) // invalidates the cache
	require.NotContains(t, item.cache, 30, "invalidation must drop the stale entry")

	_ = item.Render(30)
	require.Contains(t, item.cache, 30, "a render after invalidation must repopulate the cache, not just the nil-guard's throwaway map")

	item.SetFocused(false) // invalidate again
	_ = item.Render(30)
	require.Contains(t, item.cache, 30, "the cache must still be writable after a second invalidation cycle")
}
