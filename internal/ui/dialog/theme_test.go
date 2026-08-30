package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// themeTestWorkspace is a minimal [workspace.Workspace] stub exposing just
// the config themeItems reads.
type themeTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

// KnownProviders mirrors what the UI used to compute for itself:
// the embedded catalog for this fake's config.
func (w themeTestWorkspace) KnownProviders() []catwalk.Provider { return config.Providers(w.cfg) }

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w themeTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w themeTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w themeTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *themeTestWorkspace) Config() *config.Config { return w.cfg }

func newThemeTestCommon(themeID string) *common.Common {
	s := styles.SennitDark()
	return &common.Common{
		Styles: &s,
		Workspace: &themeTestWorkspace{cfg: &config.Config{
			Options:   &config.Options{TUI: &config.TUIOptions{Theme: themeID}},
			Providers: csync.NewMap[string, config.ProviderConfig](),
		}},
	}
}

// TestThemeItems_SelectsConfiguredPalette checks the dialog opens on the
// palette actually in use, and marks exactly that one as current.
func TestThemeItems_SelectsConfiguredPalette(t *testing.T) {
	t.Parallel()

	items, selected, err := themeItems(newThemeTestCommon(styles.PaletteInkSage.ID))
	require.NoError(t, err)
	require.Len(t, items, len(styles.Palettes()))

	require.Equal(t, styles.PaletteInkSage.ID, items[selected].(*ThemeItem).ID())

	var currents int
	for _, item := range items {
		if item.(*ThemeItem).isCurrent {
			currents++
		}
	}
	require.Equal(t, 1, currents, "exactly one palette may be marked current")
}

// TestThemeItems_UnconfiguredSelectsDefault covers a fresh config and a
// stale one: neither may leave the dialog with nothing selected, because
// the palette in effect in both cases is the default.
func TestThemeItems_UnconfiguredSelectsDefault(t *testing.T) {
	t.Parallel()

	for _, themeID := range []string{"", "removed-theme"} {
		items, selected, err := themeItems(newThemeTestCommon(themeID))
		require.NoError(t, err)
		require.Equal(t, styles.DefaultThemeID, items[selected].(*ThemeItem).ID(), "configured theme %q", themeID)
	}
}

// TestThemeItems_FilterMatchesNameAndID pins that typing either half of a
// palette's identity finds it: the displayed name or the ID persisted in
// the config file.
func TestThemeItems_FilterMatchesNameAndID(t *testing.T) {
	t.Parallel()

	items, _, err := themeItems(newThemeTestCommon(""))
	require.NoError(t, err)

	for _, item := range items {
		themeItem := item.(*ThemeItem)
		require.Contains(t, themeItem.Filter(), themeItem.palette.Name)
		require.Contains(t, themeItem.Filter(), themeItem.palette.ID)
	}
}

// TestNewTheme_RendersAndSelects walks the dialog the way a user does:
// open it, move down one row, hit enter, and check the action names the
// palette that row shows.
func TestNewTheme_RendersAndSelects(t *testing.T) {
	t.Parallel()

	d, err := NewTheme(newThemeTestCommon(styles.DefaultThemeID))
	require.NoError(t, err)

	area := image.Rect(0, 0, 70, 16)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() { d.Draw(scr, area) })
	require.Contains(t, scr.String(), styles.PaletteSteelTeal.Name)

	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	selected, ok := action.(ActionSelectTheme)
	require.True(t, ok, "enter must select a theme, got %T", action)
	require.Equal(t, styles.Palettes()[1].ID, selected.ID)
}

// TestSystemCommandItems_ThemeOpensPicker keeps "/theme" wired to the
// picker dialog rather than to a direct action.
func TestSystemCommandItems_ThemeOpensPicker(t *testing.T) {
	t.Parallel()

	com := newCommandsNamesTestCommon(t)
	items := systemCommandItems(com, "sess-1", true /* hasSession */, false, false, 200, nil)

	theme := findByID(t, items, "select_theme")
	require.Equal(t, "theme", theme.Title())
	require.Equal(t, ActionOpenDialog{DialogID: ThemeID}, theme.Action())
}

// TestNewTheme_PreviewsOnMove pins the preview contract from the dialog's
// side: moving the selection announces the highlighted palette so the UI
// can paint itself in it, and typing a filter — which also moves the
// selection — announces it alongside the input's own command.
func TestNewTheme_PreviewsOnMove(t *testing.T) {
	t.Parallel()

	d, err := NewTheme(newThemeTestCommon(styles.DefaultThemeID))
	require.NoError(t, err)

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	preview, ok := action.(ActionPreviewTheme)
	require.True(t, ok, "moving down must preview a theme, got %T", action)
	require.Equal(t, styles.Palettes()[1].ID, preview.ID)

	batch, ok := d.HandleMsg(tea.KeyPressMsg{Code: 'i', Text: "i"}).(ActionBatch)
	require.True(t, ok, "filtering must keep the input command and preview the new top row")
	var previewed string
	for _, a := range batch.Actions {
		if p, ok := a.(ActionPreviewTheme); ok {
			previewed = p.ID
		}
	}
	require.NotEmpty(t, previewed)
	require.Equal(t, d.selectedID(), previewed)
}
