package dialog

import (
	"errors"
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTestProviderForm(t *testing.T) *ProviderForm {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	return NewProviderForm(com)
}

func typeIntoProviderForm(t *testing.T, m *ProviderForm, s string) {
	t.Helper()
	for _, r := range s {
		action := m.HandleMsg(keyMsg(r))
		require.Nil(t, action)
	}
}

func TestProviderForm_SubmitBlockedWithoutRequiredFields(t *testing.T) {
	m := newTestProviderForm(t)

	// Submitting with everything empty must not produce
	// ActionSubmitCustomProvider, and must surface an error.
	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.Equal(t, "Provider ID is required", m.errMsg)
	require.False(t, m.submitting)

	// ID filled in but base URL still empty.
	typeIntoProviderForm(t, m, "my-provider")
	action = m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.Equal(t, "Base URL is required", m.errMsg)
	require.False(t, m.submitting)
}

func TestProviderForm_SubmitWithValidFields(t *testing.T) {
	m := newTestProviderForm(t)

	typeIntoProviderForm(t, m, "my-provider")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> base URL field
	typeIntoProviderForm(t, m, "https://api.example.com/v1")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := action.(ActionSubmitCustomProvider)
	require.True(t, ok, "expected ActionSubmitCustomProvider, got %#v", action)
	require.Equal(t, "my-provider", submit.ID)
	require.Equal(t, "https://api.example.com/v1", submit.BaseURL)
	require.Equal(t, "openai-compat", submit.Type)
	require.Equal(t, "", submit.APIKey)
	require.True(t, m.submitting)
	require.Equal(t, "", m.errMsg)
}

func TestProviderForm_SubmitIncludesAPIKeyAndType(t *testing.T) {
	m := newTestProviderForm(t)

	typeIntoProviderForm(t, m, "my-provider")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> base URL
	typeIntoProviderForm(t, m, "https://api.example.com/v1")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> type
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> api key
	typeIntoProviderForm(t, m, "secret")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := action.(ActionSubmitCustomProvider)
	require.True(t, ok)
	require.Equal(t, "secret", submit.APIKey)
	require.NotEqual(t, "openai-compat", submit.Type, "cycling right once should move off the default")
}

// TestProviderForm_CursorTracksFocusedField draws the dialog with each text
// field focused in turn and checks the returned cursor actually lands on
// that field's rendered input line, not on a neighboring label or a
// different field entirely. This guards against the row offsets drifting
// out of sync with what Draw() actually renders (see Cursor()).
func TestProviderForm_CursorTracksFocusedField(t *testing.T) {
	t.Parallel()

	m := newTestProviderForm(t)
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

	// ID is focused from the start; type into it and check the cursor sits
	// on its line before tabbing away.
	typeIntoProviderForm(t, m, "zzidzz")
	idRow := draw("zzidzz")

	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> base URL
	typeIntoProviderForm(t, m, "zzurlzz")
	urlRow := draw("zzurlzz")

	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> type
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> api key
	typeIntoProviderForm(t, m, "zzkeyzz")
	keyRow := draw("zzkeyzz")

	require.Less(t, idRow, urlRow, "id row should precede base URL row")
	require.Less(t, urlRow, keyRow, "base URL row should precede API key row")
}

// TestProviderForm_CursorHiddenOnTypeField verifies the type selector (no
// text input) hides the cursor entirely rather than drawing it somewhere
// meaningless.
func TestProviderForm_CursorHiddenOnTypeField(t *testing.T) {
	t.Parallel()

	m := newTestProviderForm(t)
	m.focus = providerFormFieldType

	area := image.Rect(0, 0, 80, 30)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	cur := m.Draw(scr, area)
	require.Nil(t, cur)
}

func TestProviderForm_ResultRoundTrip(t *testing.T) {
	m := newTestProviderForm(t)
	typeIntoProviderForm(t, m, "my-provider")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	typeIntoProviderForm(t, m, "https://api.example.com/v1")
	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, ok := action.(ActionSubmitCustomProvider)
	require.True(t, ok)
	require.True(t, m.submitting)

	// A key press while submitting must not race the async result.
	require.Nil(t, m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))

	// Failure keeps the dialog open with an error rather than closing it.
	errAction := m.HandleMsg(ActionCustomProviderResult{ProviderID: "my-provider", Err: errors.New("no models found")})
	require.Nil(t, errAction)
	require.False(t, m.submitting)
	require.Equal(t, "no models found", m.errMsg)

	// Success hands control to the caller to close the dialogs.
	m.submitting = true
	successAction := m.HandleMsg(ActionCustomProviderResult{ProviderID: "my-provider"})
	require.Equal(t, ActionProviderConfigured{ProviderID: "my-provider"}, successAction)
	require.False(t, m.submitting)
}
