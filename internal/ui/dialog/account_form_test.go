package dialog

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

func newTestAccountForm(t *testing.T, account workspace.FrontendAccount, active bool) *AccountForm {
	t.Helper()
	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	return NewAccountForm(com, "openai", account, active)
}

func typeIntoAccountForm(t *testing.T, m *AccountForm, s string) {
	t.Helper()
	for _, r := range s {
		action := m.HandleMsg(keyMsg(r))
		require.Nil(t, action)
	}
}

// TestAccountForm_EmptyLabelAllowed covers submitting with an empty label —
// it's optional; the account's ID is what's shown in its place. The
// account already has no label, so the field is untouched and Edit.Label
// stays nil (see submit()'s "only send what changed" rule).
func TestAccountForm_EmptyLabelAllowed(t *testing.T) {
	m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", HasAPIKey: true}, false)

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := action.(ActionSubmitAccountForm)
	require.True(t, ok, "expected ActionSubmitAccountForm, got %#v", action)
	require.Equal(t, "openai", submit.ProviderID)
	require.Nil(t, submit.Edit.Label, "an untouched field must not be sent")
	require.True(t, m.submitting)
}

// TestAccountForm_LabelEditSendsOnlyLabel covers the actual edit case: only
// the field the person touched must appear on Edit, everything else stays
// nil so an unrelated save can never overwrite ProxyURL/Disabled.
func TestAccountForm_LabelEditSendsOnlyLabel(t *testing.T) {
	m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", Label: "Old", ProxyURL: "http://user@host:8080"}, false)
	typeIntoAccountForm(t, m, "New")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := action.(ActionSubmitAccountForm)
	require.True(t, ok, "expected ActionSubmitAccountForm, got %#v", action)
	require.NotNil(t, submit.Edit.Label)
	require.Equal(t, "OldNew", *submit.Edit.Label)
	require.Nil(t, submit.Edit.ProxyURL, "the untouched, already-redacted proxy field must not be sent back")
	require.Nil(t, submit.Edit.Disabled)
}

// TestAccountForm_InvalidProxyRejectedBeforeSaving pins the requirement
// that the proxy value is validated against the entered text before
// submitting — not after a failed request — and that a rejected value
// leaves the form open with an error rather than producing a submit action.
func TestAccountForm_InvalidProxyRejectedBeforeSaving(t *testing.T) {
	m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", HasAPIKey: true}, false)
	m.advanceFocus(1) // move to the proxy field
	typeIntoAccountForm(t, m, "://not-a-url")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action, "an invalid proxy must not submit")
	require.NotEmpty(t, m.errMsg)
	require.False(t, m.submitting)
}

// TestAccountForm_ValidProxySubmits covers each of the proxy field's three
// legal states: empty (inherit, unchanged from the account's own empty
// ProxyURL so nothing is sent), "none" (direct), and a real URL (both
// actually typed, so both are sent as-is).
func TestAccountForm_ValidProxySubmits(t *testing.T) {
	for _, proxy := range []string{"", "none", "http://proxy.example:8080"} {
		t.Run(proxy, func(t *testing.T) {
			m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", HasAPIKey: true}, false)
			m.advanceFocus(1)
			if proxy != "" {
				typeIntoAccountForm(t, m, proxy)
			}

			action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
			submit, ok := action.(ActionSubmitAccountForm)
			require.True(t, ok, "expected ActionSubmitAccountForm, got %#v", action)
			if proxy == "" {
				require.Nil(t, submit.Edit.ProxyURL, "an untouched empty proxy field must not be sent")
				return
			}
			require.NotNil(t, submit.Edit.ProxyURL)
			require.Equal(t, proxy, *submit.Edit.ProxyURL)
		})
	}
}

// TestAccountForm_ActiveAccountCannotBeDisabled covers the invariant this
// form enforces: an account cannot be both the provider's active one and
// disabled (see submit()'s doc comment for why — a disabled active account
// is a state the rest of Sennit treats as unusable). Toggling Enabled off
// on an active account must block the submit with an explanatory error
// instead of quietly disabling and orphaning it.
func TestAccountForm_ActiveAccountCannotBeDisabled(t *testing.T) {
	m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", HasAPIKey: true}, true)
	m.advanceFocus(2) // move to the Enabled field
	require.Equal(t, accountFormFieldEnabled, m.focus)

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	require.Nil(t, action)
	require.False(t, m.enabled)

	action = m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action, "disabling the active account must not submit")
	require.Contains(t, m.errMsg, "active account")
	require.False(t, m.submitting)
}

// TestAccountForm_InactiveAccountCanBeDisabled is the control case for the
// rule above: disabling an account that isn't active must submit normally.
func TestAccountForm_InactiveAccountCanBeDisabled(t *testing.T) {
	m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", HasAPIKey: true}, false)
	m.advanceFocus(2)
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	require.False(t, m.enabled)

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := action.(ActionSubmitAccountForm)
	require.True(t, ok, "expected ActionSubmitAccountForm, got %#v", action)
	require.NotNil(t, submit.Edit.Disabled)
	require.True(t, *submit.Edit.Disabled)
}

// TestAccountForm_ResultRoundTrip covers HandleMsg's ActionAccountFormResult
// branch: an error keeps the dialog open and shows it; success closes the
// loop by returning ActionAccountSaved.
func TestAccountForm_ResultRoundTrip(t *testing.T) {
	t.Run("error keeps the form open", func(t *testing.T) {
		m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1"}, false)
		m.submitting = true

		action := m.HandleMsg(ActionAccountFormResult{ProviderID: "openai", Err: errors.New("boom")})
		require.Nil(t, action)
		require.False(t, m.submitting)
		require.NotEmpty(t, m.errMsg)
	})

	t.Run("success closes the loop", func(t *testing.T) {
		m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1"}, false)
		m.submitting = true

		action := m.HandleMsg(ActionAccountFormResult{ProviderID: "openai"})
		require.Equal(t, ActionAccountSaved{ProviderID: "openai"}, action)
		require.False(t, m.submitting)
	})
}

// TestAccountForm_CtrlATriggersSignIn pins that ctrl+a returns
// ActionAddAccount for the form's provider, regardless of which field
// has focus: the chord cannot collide with typing.
func TestAccountForm_CtrlATriggersSignIn(t *testing.T) {
	t.Parallel()
	m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", HasAPIKey: true}, false)

	for i := 0; i < int(accountFormFieldCount); i++ {
		m.advanceFocus(1)
		action := m.HandleMsg(ctrlAMsg())
		add, ok := action.(ActionAddAccount)
		require.True(t, ok, "field %d: expected ActionAddAccount, got %#v", i, action)
		require.Equal(t, "openai", add.ProviderID)
	}
}

// TestAccountForm_PlainATypesIntoLabel pins that plain 'a' is a regular
// character in the label field: it does not trigger sign-in.
func TestAccountForm_PlainATypesIntoLabel(t *testing.T) {
	t.Parallel()
	m := newTestAccountForm(t, workspace.FrontendAccount{ID: "acct-1", HasAPIKey: true}, false)

	action := m.HandleMsg(keyMsg('a'))
	_, isAdd := action.(ActionAddAccount)
	require.False(t, isAdd, "'a' must not trigger sign-in")
	require.Equal(t, "a", m.label.Value())
}
