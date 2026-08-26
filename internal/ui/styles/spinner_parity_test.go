package styles

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/spin"
)

// TestSpinnerModeParity holds internal/config's spinner names and
// internal/spin's modes to the same list.
//
// The two cannot share a type: config must not import the TUI, so it
// names the modes as plain strings and SpinnerMode converts. Nothing in
// the compiler notices when one side gains a mode the other has never
// heard of — the conversion just falls through to its default, and a
// setting the schema advertises as valid silently does nothing.
func TestSpinnerModeParity(t *testing.T) {
	t.Parallel()

	for _, name := range config.SpinnerModes {
		require.Equal(t, name, string(SpinnerMode(name)),
			"config advertises %q but SpinnerMode does not map it to a mode of that name", name)
	}

	// And the other direction: a mode added to anim without a config
	// constant is one no one can ever select.
	for _, mode := range []spin.Mode{spin.ModeScramble, spin.ModePulse, spin.ModeDots, spin.ModeNone} {
		require.Contains(t, config.SpinnerModes, string(mode),
			"anim has mode %q with no config constant to select it", mode)
	}
}

// TestWithSpinnerLeavesPaletteAlone pins that the motion setting rides
// alongside the palette rather than being part of it: WithSpinner must
// change nothing a theme decided.
func TestWithSpinnerLeavesPaletteAlone(t *testing.T) {
	t.Parallel()

	base := Theme(PaletteSteelTeal.ID)
	withMode := Theme(PaletteSteelTeal.ID).WithSpinner(spin.ModeDots)

	require.Equal(t, spin.ModeScramble, base.WorkingSpinner,
		"Theme alone must produce the default motion")
	require.Equal(t, spin.ModeDots, withMode.WorkingSpinner)
	require.Equal(t, base.WorkingGradFromColor, withMode.WorkingGradFromColor)
	require.Equal(t, base.WorkingLabelColor, withMode.WorkingLabelColor)
}
