package model

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/anim"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// spinnerWorkspace is a countingWorkspace that actually has a config, so
// the spinner setting has somewhere to come from.
type spinnerWorkspace struct {
	*countingWorkspace
	cfg *config.Config
}

func (w *spinnerWorkspace) Config() *config.Config { return w.cfg }

func configWithSpinner(mode string) *config.Config {
	return &config.Config{Options: &config.Options{TUI: &config.TUIOptions{Spinner: mode}}}
}

// TestSpinnerModeComesFromConfig pins the plumbing end to end: a config
// value reaches the Styles every chat item reads its animation settings
// from.
func TestSpinnerModeComesFromConfig(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		configured string
		want       anim.Mode
	}{
		{config.SpinnerScramble, anim.ModeScramble},
		{config.SpinnerPulse, anim.ModePulse},
		{config.SpinnerDots, anim.ModeDots},
		{config.SpinnerNone, anim.ModeNone},
		// Unset means the default, and so does a value the UI does not
		// know — the doctor reports the latter, rendering does not fail.
		{"", anim.ModeScramble},
		{"disco", anim.ModeScramble},
	} {
		ws := &spinnerWorkspace{countingWorkspace: &countingWorkspace{}, cfg: configWithSpinner(tc.configured)}
		com := common.DefaultCommon(context.Background(), ws)
		require.Equal(t, tc.want, com.Styles.WorkingSpinner, "spinner %q", tc.configured)
	}
}

// TestThemeSwitchKeepsSpinnerMode is the regression that pays for
// WorkingSpinner living on Styles at all.
//
// setTheme replaces the whole Styles value, and the palette it builds
// from cannot carry a config-derived setting — so the obvious
// implementation (`*m.com.Styles = styles.Theme(id)`) silently reset
// every spinner to the scramble the first time someone ran /theme. The
// compiler cannot catch that: the field simply goes back to its zero
// value. This test is what catches it.
func TestThemeSwitchKeepsSpinnerMode(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.com.Workspace = &spinnerWorkspace{
		countingWorkspace: &countingWorkspace{},
		cfg:               configWithSpinner(config.SpinnerDots),
	}
	u.com.Styles.WorkingSpinner = anim.ModeDots

	u.setTheme(styles.PaletteInkSage.ID)

	require.Equal(t, styles.PaletteInkSage.Bg, u.com.Styles.Background,
		"precondition: the theme actually changed")
	require.Equal(t, anim.ModeDots, u.com.Styles.WorkingSpinner,
		"a theme switch must not reset the configured spinner motion")
}
