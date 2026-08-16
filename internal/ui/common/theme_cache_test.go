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

func TestChromaStyle_RebuiltOnThemeSwitch(t *testing.T) {
	sty := styles.Theme(styles.PaletteSteelTeal.ID)
	before := ChromaStyle(&sty, nil)
	require.Same(t, before, ChromaStyle(&sty, nil), "same palette must hit the cache")

	sty = styles.Theme(styles.PaletteInkSage.ID)
	require.NotSame(t, before, ChromaStyle(&sty, nil),
		"chroma style survived a theme switch and would keep the old colors")
}
