package styles

import (
	"image/color"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPalettes_NoMissingColors is the guard for adding a palette: every
// color field must be set. A forgotten field is a nil color.Color, which
// lipgloss renders as "no color at all" — the text silently falls back to
// the terminal's own foreground instead of failing, so nothing else in the
// build or the tests would catch it.
func TestPalettes_NoMissingColors(t *testing.T) {
	t.Parallel()

	colorType := reflect.TypeOf((*color.Color)(nil)).Elem()
	for _, p := range Palettes() {
		v := reflect.ValueOf(p)
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if field.Type != colorType {
				continue
			}
			require.False(t, v.Field(i).IsNil(),
				"palette %q leaves %s unset", p.ID, field.Name)
		}
	}
}

// TestPalettes_Identified pins the metadata the picker relies on: unique,
// non-empty IDs, and a name and description to render.
func TestPalettes_Identified(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, p := range Palettes() {
		require.NotEmpty(t, p.ID, "palette has no ID")
		require.NotEmpty(t, p.Name, "palette %q has no name", p.ID)
		require.NotEmpty(t, p.Description, "palette %q has no description", p.ID)
		require.False(t, seen[p.ID], "duplicate palette ID %q", p.ID)
		seen[p.ID] = true
	}
	require.True(t, seen[DefaultThemeID], "DefaultThemeID %q names no palette", DefaultThemeID)
}

// TestPaletteByID_FallsBackToDefault covers the two ways a config can name a
// palette that isn't there — never configured, or configured and later
// renamed/removed. Both resolve to the default rather than to a zero
// Palette, which would render the entire UI colorless.
func TestPaletteByID_FallsBackToDefault(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"", "no-such-theme"} {
		require.Equal(t, PaletteSteelTeal.ID, PaletteByID(id).ID, "id %q", id)
		require.False(t, IsKnownPaletteID(id), "id %q must not be reported as known", id)
	}

	require.Equal(t, PaletteInkSage.ID, PaletteByID(PaletteInkSage.ID).ID)
	require.True(t, IsKnownPaletteID(PaletteInkSage.ID))
}

// TestPalettes_ReturnsCopy makes sure a caller mutating the returned slice
// cannot reach the registry the picker and PaletteByID read from.
func TestPalettes_ReturnsCopy(t *testing.T) {
	t.Parallel()

	got := Palettes()
	got[0] = Palette{ID: "clobbered"}
	require.Equal(t, PaletteSteelTeal.ID, Palettes()[0].ID)
	require.Equal(t, PaletteSteelTeal.ID, PaletteByID(PaletteSteelTeal.ID).ID)
}

// TestTheme_DiffersPerPalette is the end-to-end check that a palette
// actually reaches the Styles graph: two themes must not paint the same
// background. Without it, a Theme() that ignored its argument would still
// satisfy every other test here.
func TestTheme_DiffersPerPalette(t *testing.T) {
	t.Parallel()

	steel := Theme(PaletteSteelTeal.ID)
	amber := Theme(PaletteGraphiteAmber.ID)

	require.Equal(t, PaletteSteelTeal.Bg, steel.Background)
	require.Equal(t, PaletteGraphiteAmber.Bg, amber.Background)
	require.NotEqual(t, steel.Background, amber.Background)

	// BraidDark is the default palette, not a fifth hardcoded theme.
	require.Equal(t, steel.Background, BraidDark().Background)
}
