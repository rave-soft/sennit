package dialog

import (
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/rendercachetest"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestSessionItem_RenderIsGenuinelyCached exercises the real renderItem
// path (list.BaseItem embedded in a concrete ListItem, exactly as every
// dialog list uses it: SessionItem, CommandItem, ModelItem, ...) and
// proves a second Render(width) is a genuine cache hit rather than a
// recomputation that happens to match. See TECHDEBT.md's "Кэши, которые
// молча не кэшируют": renderItem's nil-guard used to reassign its own
// local `cache` parameter instead of writing through to the field it was
// handed, so BaseItem.Cache() being non-nil (or holding a stale entry)
// proved nothing about whether Render actually reads from it.
func TestSessionItem_RenderIsGenuinelyCached(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := &SessionItem{
		BaseItem: list.NewBaseItem(),
		Session:  session.Session{ID: "s1", Title: "a genuinely cached session"},
		t:        &sty,
	}

	rendercachetest.AssertPerWidthCacheHit(t, item.Render, item.Cache(), 40, 70)
}

// TestSessionItem_CacheRepopulatesAfterInvalidate covers the other half:
// invalidate() (via SetFocused, which the dialog calls on every selection
// change) must not just clear stale entries but leave the cache in a
// state the next Render can actually write into and read back from.
func TestSessionItem_CacheRepopulatesAfterInvalidate(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := &SessionItem{
		BaseItem: list.NewBaseItem(),
		Session:  session.Session{ID: "s1", Title: "a repopulated session"},
		t:        &sty,
	}

	first := item.Render(50)
	require.Contains(t, item.Cache(), 50)

	item.SetFocused(true) // invalidate()
	require.NotContains(t, item.Cache(), 50, "invalidation must drop the stale entry")

	second := item.Render(50)
	require.Contains(t, item.Cache(), 50, "a render after invalidation must repopulate the cache")
	require.NotEqual(t, first, second, "focus changed, so the focused render must differ from the blurred one")

	// And the repopulated entry must itself be a genuine cache: overwrite
	// it with a sentinel and confirm the next render at the same width
	// reads it back instead of recomputing.
	item.Cache()[50] = "sentinel-after-invalidate"
	require.Equal(t, "sentinel-after-invalidate", item.Render(50))
}
