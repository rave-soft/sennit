package dialog

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func typeInto(t *testing.T, m *ProviderForm, s string) {
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
	typeInto(t, m, "my-provider")
	action = m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.Equal(t, "Base URL is required", m.errMsg)
	require.False(t, m.submitting)
}

func TestProviderForm_SubmitWithValidFields(t *testing.T) {
	m := newTestProviderForm(t)

	typeInto(t, m, "my-provider")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> base URL field
	typeInto(t, m, "https://api.example.com/v1")

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

	typeInto(t, m, "my-provider")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> base URL
	typeInto(t, m, "https://api.example.com/v1")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> type
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}) // -> api key
	typeInto(t, m, "secret")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := action.(ActionSubmitCustomProvider)
	require.True(t, ok)
	require.Equal(t, "secret", submit.APIKey)
	require.NotEqual(t, "openai-compat", submit.Type, "cycling right once should move off the default")
}

func TestProviderForm_ResultRoundTrip(t *testing.T) {
	m := newTestProviderForm(t)
	typeInto(t, m, "my-provider")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	typeInto(t, m, "https://api.example.com/v1")
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
