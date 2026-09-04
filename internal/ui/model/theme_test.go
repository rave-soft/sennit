package model

import (
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestSetTheme_SwapsSharedStyles is the core of live theme switching: every
// component draws through the same *common.Common, so re-reading
// com.Styles at draw time repaints the whole UI without rebuilding a
// single component. If setTheme stopped updating com.Styles at all, those
// components would keep drawing in the old palette and only this test
// would notice.
//
// setTheme assigns com.Styles a *new* pointer rather than overwriting the
// old one's fields in place — this used to be require.Same here, on the
// theory that identity was part of the contract components (and
// tea.Cmd closures that snapshot the pointer, e.g. beginSessionLoad) relied
// on. It was not: nothing reads a *styles.Styles pointer's identity, only
// its fields, and a command that had already captured the old pointer was
// racing this exact in-place write on the theme's own dialog goroutine —
// see internal/devtools/uicheck's cmdclosure_test.go. Repointing keeps a
// captured snapshot a valid, frozen copy of the old palette instead.
func TestSetTheme_SwapsSharedStyles(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	before := u.com.Styles
	require.Equal(t, styles.PaletteSteelTeal.Bg, before.Background)

	u.setTheme(styles.PaletteInkSage.ID)

	require.Equal(t, styles.PaletteInkSage.Bg, u.com.Styles.Background,
		"a component reading com.Styles fresh must see the new palette")
	require.Equal(t, styles.PaletteSteelTeal.Bg, before.Background,
		"a snapshot taken before the switch must keep reading the old palette, not race the switch")
}

// TestSetTheme_RestylesCopiedStyles covers the widgets that copy a style at
// construction rather than reading Common.Styles at draw time; the status
// bar's help model stands in for the set setTheme has to refresh (the
// editor's textarea, completions and attachments are the others).
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

// TestSetTheme_RepaintsOpenCompletions covers the widgets the editor builds
// once and keeps for the whole session. The completions popup copies its
// styles into every item it holds, so without an explicit restyle it keeps
// drawing the palette it was opened in — one of the "only part of the UI
// changed color" symptoms.
func TestSetTheme_RepaintsOpenCompletions(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.completions.popup = completions.New(completions.PopupStyles{
		Normal:  u.com.Styles.Completions.Normal,
		Focused: u.com.Styles.Completions.Focused,
		Match:   u.com.Styles.Completions.Match,
		Muted:   u.com.Styles.Completions.Muted,
		Border:  u.com.Styles.Completions.Border,
	})
	u.editor.completions.popup.SetItems([]completions.FileCompletionValue{{Path: "main.go"}}, nil)
	before := u.editor.completions.popup.Render()

	u.setTheme(styles.PaletteGraphiteAmber.ID)

	require.NotEqual(t, before, u.editor.completions.popup.Render(),
		"the open completions popup kept the previous palette")
}

// TestPreviewTheme_RestoredOnCancel is the contract behind previewing: the
// UI really does switch palette as the selection moves (so the preview is
// the actual thing, not an approximation of it), and walking away from the
// dialog leaves both the screen and the config exactly as they were.
func TestPreviewTheme_ConfiguredThemeCancelDoesNotRestyle(t *testing.T) {
	t.Parallel()

	// Do not call setTheme: this exercises a newly created preview owner, whose
	// live palette must fall back to the configured theme.
	u := newTestUI()
	u.com.Workspace = &testWorkspace{cfg: &config.Config{
		Options: &config.Options{TUI: &config.TUIOptions{Theme: styles.PaletteSteelTeal.ID}},
	}}
	before := u.com.Styles.Background

	require.Nil(t, u.previewTheme(styles.PaletteSteelTeal.ID),
		"previewing the configured theme must not apply or restyle it")
	require.Nil(t, u.cancelThemePreview(),
		"cancelling an unapplied configured-theme preview must not restore it")
	require.Equal(t, before, u.com.Styles.Background)
	require.Empty(t, u.themePreview.liveID,
		"cancel must not apply or restyle the configured fallback palette")
	require.Equal(t, styles.PaletteSteelTeal.ID, u.liveThemeID())
}

func TestApplyTheme_NoOpDoesNotStartPersistence(t *testing.T) {
	t.Parallel()

	u := newTestUIWithTheme(t, styles.PaletteSteelTeal.ID)

	require.Nil(t, u.applyTheme(styles.PaletteSteelTeal.ID))
	require.Zero(t, u.themePersistence.generation)
	require.False(t, u.themePersistence.pending)
}

func TestApplyTheme_StalePersistenceFailureCannotRestoreNewerSelection(t *testing.T) {
	t.Parallel()

	u := newTestUIWithTheme(t, styles.PaletteSteelTeal.ID)
	require.NotNil(t, u.applyTheme(styles.PaletteInkSage.ID))
	first := u.themePersistence.generation
	require.NotNil(t, u.applyTheme(styles.PaletteGraphiteAmber.ID))
	second := u.themePersistence.generation

	cmds, _ := u.updateSettings(themeSetMsg{
		Err:        errors.New("first write failed"),
		Previous:   styles.PaletteSteelTeal.ID,
		generation: first,
	}, nil)
	require.Empty(t, cmds)
	require.Equal(t, styles.PaletteGraphiteAmber.ID, u.liveThemeID())

	cmds, _ = u.updateSettings(themeSetMsg{ID: styles.PaletteGraphiteAmber.ID, generation: second}, nil)
	require.Len(t, cmds, 1)
	require.Equal(t, styles.PaletteGraphiteAmber.ID, u.liveThemeID())
}

func TestPreviewTheme_RestoredOnCancel(t *testing.T) {
	t.Parallel()

	u := newTestUIWithTheme(t, styles.PaletteSteelTeal.ID)
	original := u.com.Styles.Background

	u.previewTheme(styles.PaletteInkSage.ID)
	require.Equal(t, styles.PaletteInkSage.Bg, u.com.Styles.Background,
		"previewing must paint the UI in the highlighted palette")

	// Browsing on before backing out: the restore point is where the
	// dialog opened, not the previously highlighted row.
	u.previewTheme(styles.PaletteGraphiteAmber.ID)
	u.cancelThemePreview()

	require.Equal(t, original, u.com.Styles.Background)
	require.Equal(t, styles.PaletteSteelTeal.ID, u.liveThemeID())
}

// TestPreviewTheme_KeptOnSelect checks the other exit: confirming keeps the
// previewed palette and persists it, rather than restoring the palette the
// dialog opened in.
func TestThemePreview_UnknownIDsPreserveOrCancelAsAppropriate(t *testing.T) {
	t.Parallel()

	u := newTestUIWithTheme(t, styles.PaletteSteelTeal.ID)
	u.previewTheme("no-such-theme")
	require.Equal(t, styles.PaletteSteelTeal.ID, u.liveThemeID(), "unknown preview must be ignored")

	u.previewTheme(styles.PaletteInkSage.ID)
	cmd := u.applyTheme("no-such-theme")
	require.NotNil(t, cmd, "unknown apply must report an error")
	require.Equal(t, styles.PaletteSteelTeal.ID, u.liveThemeID(), "unknown apply must cancel an active preview")
}

func TestPreviewTheme_KeptOnSelect(t *testing.T) {
	t.Parallel()

	u := newTestUIWithTheme(t, styles.PaletteSteelTeal.ID)

	u.previewTheme(styles.PaletteInkSage.ID)
	cmd := u.applyTheme(styles.PaletteInkSage.ID)

	require.NotNil(t, cmd, "confirming a previewed theme must persist it")
	require.Equal(t, styles.PaletteInkSage.Bg, u.com.Styles.Background)
	// A later cancel (e.g. the commands dialog closing behind the picker)
	// must not undo the confirmed choice.
	u.cancelThemePreview()
	require.Equal(t, styles.PaletteInkSage.Bg, u.com.Styles.Background)
}

// newTestUIWithTheme builds a test UI whose workspace config names the
// given palette, which is what the preview bookkeeping resolves "the
// palette we started from" against.
func newTestUIWithTheme(t *testing.T, id string) *UI {
	t.Helper()

	u := newTestUI()
	u.com.Workspace = &testWorkspace{cfg: &config.Config{
		Options: &config.Options{TUI: &config.TUIOptions{Theme: id}},
	}}
	u.setTheme(id)
	return u
}
