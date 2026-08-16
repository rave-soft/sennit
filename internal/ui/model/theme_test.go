package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestSetTheme_SwapsSharedStyles is the core of live theme switching: every
// component holds the *same* *styles.Styles pointer that lives on Common, so
// replacing the value behind it repaints the whole UI without rebuilding a
// single component. If setTheme ever assigned a new pointer instead, the
// components would keep drawing in the old palette and only this test would
// notice.
func TestSetTheme_SwapsSharedStyles(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	shared := u.com.Styles
	require.Equal(t, styles.PaletteSteelTeal.Bg, shared.Background)

	u.setTheme(styles.PaletteInkSage.ID)

	require.Same(t, shared, u.com.Styles, "setTheme must not repoint Common.Styles")
	require.Equal(t, styles.PaletteInkSage.Bg, shared.Background)
}

// TestSetTheme_RestylesCopiedStyles covers the widgets that copy a style at
// construction rather than reading Common.Styles at draw time — the help
// model in the status bar is the one such case in the chat UI.
func TestSetTheme_RestylesCopiedStyles(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	before := u.status.help.Styles.ShortKey.GetForeground()

	u.setTheme(styles.PaletteGraphiteAmber.ID)

	after := u.status.help.Styles.ShortKey.GetForeground()
	require.NotEqual(t, before, after, "status help kept the previous palette")
	require.Equal(t, u.com.Styles.Help.ShortKey.GetForeground(), after)
}

// TestApplyTheme_RejectsUnknownID guards the path an action could take with
// a stale ID (a palette removed between builds): it must report the error
// and leave the palette alone, not persist a name nothing resolves to.
func TestApplyTheme_RejectsUnknownID(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	before := u.com.Styles.Background

	cmd := u.applyTheme("no-such-theme")

	require.NotNil(t, cmd, "an unknown theme must report an error")
	require.Equal(t, before, u.com.Styles.Background)
}
