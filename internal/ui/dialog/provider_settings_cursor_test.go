package dialog

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

// TestProviderSettings_CursorWithIdenticalFieldValues pins the field down
// by render position rather than by its text. A RotateBoth provider draws
// three inputs sharing one prompt; when two of them hold the same string,
// anything that identifies the line by its value or placeholder lands on
// the first match instead of the focused field.
func TestProviderSettings_CursorWithIdenticalFieldValues(t *testing.T) {
	t.Parallel()

	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateBoth})

	// Proxy, Enabled, Threshold, Cooldown - three of them text inputs.
	require.Len(t, m.fields, 4)

	const same = "30"
	m.proxy.SetValue(same)
	m.threshold.SetValue(same)
	m.cooldown.SetValue(same)

	area := image.Rect(0, 0, 80, 30)
	rowOf := func() int {
		t.Helper()
		cur := m.Draw(uv.NewScreenBuffer(area.Dx(), area.Dy()), area)
		require.NotNil(t, cur)
		return cur.Y
	}

	proxyRow := rowOf()
	m.advanceFocus(1) // -> Enabled, no input
	require.Nil(t, m.Draw(uv.NewScreenBuffer(area.Dx(), area.Dy()), area))
	m.advanceFocus(1) // -> Threshold
	thresholdRow := rowOf()
	m.advanceFocus(1) // -> Cooldown
	cooldownRow := rowOf()

	require.Less(t, proxyRow, thresholdRow, "each field must get its own row despite sharing a value")
	require.Less(t, thresholdRow, cooldownRow, "each field must get its own row despite sharing a value")
}

// TestPromptCursor covers the search itself: which line it picks, how it
// counts occurrences, and what it refuses.
func TestPromptCursor(t *testing.T) {
	t.Parallel()

	at := func(x, y int) *tea.Cursor { return &tea.Cursor{Position: tea.Position{X: x, Y: y}} }

	// Four border styles drawing the same two-field dialog. The left edge
	// is the only part that matters, and none of these runes may be
	// hardcoded anywhere: a theme picks the border, not this package.
	borders := map[string]string{
		"rounded": "╭────────╮\n│ Title  │\n│ > one  │\n│ > two  │\n╰────────╯",
		"double":  "╔════════╗\n║ Title  ║\n║ > one  ║\n║ > two  ║\n╚════════╝",
		"thick":   "┏━━━━━━━━┓\n┃ Title  ┃\n┃ > one  ┃\n┃ > two  ┃\n┗━━━━━━━━┛",
		"block":   "▛▀▀▀▀▀▀▀▀▜\n▌ Title  ▐\n▌ > one  ▐\n▌ > two  ▐\n▙▄▄▄▄▄▄▄▄▟",
	}

	for name, view := range borders {
		t.Run(name+" first field", func(t *testing.T) {
			t.Parallel()
			cur := promptCursor(view, "> ", 0, at(2, 0))
			require.NotNil(t, cur)
			// Row 2 is the first "> " line; the prompt starts at column 2
			// (border plus padding), and the input's own X rides on top.
			require.Equal(t, 2, cur.Y)
			require.Equal(t, 4, cur.X)
		})

		t.Run(name+" second field", func(t *testing.T) {
			t.Parallel()
			cur := promptCursor(view, "> ", 1, at(0, 0))
			require.NotNil(t, cur)
			require.Equal(t, 3, cur.Y)
			require.Equal(t, 2, cur.X)
		})
	}

	t.Run("a prompt that is not the first thing on the line is not the field", func(t *testing.T) {
		t.Parallel()
		// The arrow here sits inside a sentence, not at the field position.
		view := "│ see the docs > here │\n│ > real field        │"
		cur := promptCursor(view, "> ", 0, at(0, 0))
		require.NotNil(t, cur)
		require.Equal(t, 1, cur.Y, "the prose line must not be mistaken for a field")
	})

	t.Run("no match drops the cursor", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, promptCursor("│ nothing here │", "> ", 0, at(0, 0)))
		require.Nil(t, promptCursor("│ > one │", "> ", 5, at(0, 0)), "occurrence past the last field")
	})

	t.Run("refuses what it cannot place", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, promptCursor("│ > one │", "> ", 0, nil), "no input cursor")
		require.Nil(t, promptCursor("", "> ", 0, at(0, 0)), "nothing rendered yet")
		require.Nil(t, promptCursor("│ > one │", "", 0, at(0, 0)), "an empty prompt matches every line")
	})

	t.Run("the cursor it was given is not modified", func(t *testing.T) {
		t.Parallel()
		in := at(3, 0)
		out := promptCursor("│ > one │", "> ", 0, in)
		require.NotNil(t, out)
		require.Equal(t, 3, in.X, "promptCursor returns a copy")
		require.Equal(t, 0, in.Y)
		require.Equal(t, 5, out.X)
	})
}
