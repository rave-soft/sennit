package dialog

import (
	"image"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestProviderSettings_CursorTracksFocusedField draws the Codex settings
// dialog with each text field focused in turn and checks the cursor lands
// on that field's rendered input line, not on a neighboring label or a
// different field's line. This guards against the anchor-search in
// Cursor() drifting from what Draw() actually renders.
func TestProviderSettings_CursorTracksFocusedField(t *testing.T) {
	t.Parallel()

	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})

	area := image.Rect(0, 0, 80, 30)
	draw := func(want string) int {
		t.Helper()
		scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
		cur := m.Draw(scr, area)
		require.NotNil(t, cur, "focused field should have a visible cursor")

		lines := strings.Split(scr.String(), "\n")
		require.Less(t, cur.Y, len(lines))

		line := lines[cur.Y]
		require.Contains(t, line, want,
			"cursor row %d should be the %q input's line, got %q", cur.Y, want, line)
		require.LessOrEqual(t, cur.X, len(strings.TrimRight(line, " ")),
			"cursor X should fall within the rendered line's content")
		return cur.Y
	}

	// Proxy is focused from the start; type into it and check.
	typeIntoProviderSettings(t, m, "zzproxyzz")
	proxyRow := draw("zzproxyzz")

	// Tab to Enabled (no text input, cursor hidden), then tab to Threshold.
	m.advanceFocus(1) // -> Enabled
	require.Nil(t, m.Draw(uv.NewScreenBuffer(area.Dx(), area.Dy()), area))

	m.advanceFocus(1) // -> Threshold
	typeIntoProviderSettings(t, m, "42")
	thresholdRow := draw("42")

	require.Less(t, proxyRow, thresholdRow,
		"proxy row should precede threshold row")
}
