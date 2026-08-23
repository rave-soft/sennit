package common

import (
	"testing"

	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestMarkdownRenderer_RebuiltOnThemeSwitch and its chroma sibling cover
// the pair of process-wide caches that made a theme switch look half
// applied: both are shared across the UI and both hand back an object with
// the palette baked in, so keying them by width (or by the *Styles pointer,
// which live switching reuses) kept chat markdown and syntax highlighting
// in the previous theme's colors indefinitely.
func TestMarkdownRenderer_RebuiltOnThemeSwitch(t *testing.T) {
	sty := styles.Theme(styles.PaletteSteelTeal.ID)
	before := MarkdownRenderer(&sty, 80)
	require.Same(t, before, MarkdownRenderer(&sty, 80), "same palette must hit the cache")

	// A live theme switch overwrites the shared value in place.
	sty = styles.Theme(styles.PaletteInkSage.ID)
	require.NotSame(t, before, MarkdownRenderer(&sty, 80),
		"renderer survived a theme switch and would keep the old colors")
}

// TestMarkdownRenderer_ThemeSwitchEvictsStaleRendererLocks covers a
// leak alongside the rebuild above: LockMarkdownRenderer keys its
// per-renderer mutexes by pointer in rendererLocks, and a map key
// holds a strong reference to it. A theme switch drops the renderer
// from mdCache/quietMDCache, but until resetMDCachesForRev also swept
// rendererLocks, the stale pointer (and its renderer) stayed reachable
// through that map forever — every theme switch over a session's
// lifetime leaked one more renderer.
func TestMarkdownRenderer_ThemeSwitchEvictsStaleRendererLocks(t *testing.T) {
	sty := styles.Theme(styles.PaletteSteelTeal.ID)
	before := MarkdownRenderer(&sty, 80)
	LockMarkdownRenderer(before) // registers `before` in rendererLocks

	rendererLocksMu.Lock()
	_, held := rendererLocks[before]
	rendererLocksMu.Unlock()
	require.True(t, held, "sanity: the renderer must be registered before the switch")

	sty = styles.Theme(styles.PaletteInkSage.ID)
	require.NotSame(t, before, MarkdownRenderer(&sty, 80), "theme switch must rebuild the renderer")

	rendererLocksMu.Lock()
	_, stillHeld := rendererLocks[before]
	rendererLocksMu.Unlock()
	require.False(t, stillHeld, "the old renderer's lock entry must be evicted on a theme switch")
}

// TestMarkdownRenderer_WidthsCachedIndependently proves mdCache is
// genuinely keyed by width, not just "return whatever was built most
// recently": pointer identity is the observation here, and it's a
// non-tautological one — glamour.NewTermRenderer always heap-allocates a
// fresh renderer, so two calls returning the *same* pointer can only mean
// the second one read the cache instead of building again, and two
// different widths returning *different* pointers can only mean they
// weren't served from the same entry.
func TestMarkdownRenderer_WidthsCachedIndependently(t *testing.T) {
	sty := styles.Theme(styles.PaletteSteelTeal.ID)

	at80 := MarkdownRenderer(&sty, 80)
	at120 := MarkdownRenderer(&sty, 120)
	require.NotSame(t, at80, at120, "different widths must not share a cache entry")

	require.Same(t, at80, MarkdownRenderer(&sty, 80), "width 80 must still hit its own cache entry")
	require.Same(t, at120, MarkdownRenderer(&sty, 120), "width 120 must still hit its own cache entry")
}

// TestMarkdownRenderer_CacheRepopulatesAfterThemeSwitch is the other half
// of TestMarkdownRenderer_RebuiltOnThemeSwitch: that test only proves the
// entry is *dropped* on a switch. This proves the dropped entry doesn't
// stay dropped — the next render at the same width must populate a real,
// reusable cache entry rather than rebuilding on every call forever
// (which the old `c.cache = nil`-style bug in the per-item caches would
// have produced if applied here: an invalidate that never comes back).
func TestMarkdownRenderer_CacheRepopulatesAfterThemeSwitch(t *testing.T) {
	sty := styles.Theme(styles.PaletteSteelTeal.ID)
	_ = MarkdownRenderer(&sty, 90)

	sty = styles.Theme(styles.PaletteInkSage.ID)
	rebuilt := MarkdownRenderer(&sty, 90)
	require.Same(t, rebuilt, MarkdownRenderer(&sty, 90),
		"the render right after a theme switch must itself be cached, not rebuilt on every subsequent call")
}

func TestChromaStyle_RebuiltOnThemeSwitch(t *testing.T) {
	sty := styles.Theme(styles.PaletteSteelTeal.ID)
	before := ChromaStyle(&sty, nil)
	require.Same(t, before, ChromaStyle(&sty, nil), "same palette must hit the cache")

	sty = styles.Theme(styles.PaletteInkSage.ID)
	require.NotSame(t, before, ChromaStyle(&sty, nil),
		"chroma style survived a theme switch and would keep the old colors")
}
