package dialog

import (
	"testing"

	"charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newCodexDialog(t *testing.T) *OAuth {
	t.Helper()
	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	provider := catwalk.Provider{ID: catwalk.InferenceProvider(codex.ProviderID), Name: codex.ProviderName}
	dlg, _ := NewOAuthCodex(com, false, provider, nil)
	return dlg
}

// TestOAuthCodexStartsOnProxyStep: Codex is unreachable without a proxy for
// some users, so the dialog must ask before it touches the network — a flow
// started first would simply fail.
func TestOAuthCodexStartsOnProxyStep(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	require.Equal(t, OAuthStateProxy, dlg.State)
	require.NotNil(t, dlg.proxyInput)
}

// TestOAuthCopilotSkipsProxyStep keeps the step opt-in: a device flow that
// never needed one must not grow an extra screen.
func TestOAuthCopilotSkipsProxyStep(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, _ := NewOAuthCopilot(com, false, provider, nil)

	require.Equal(t, OAuthStateInitializing, dlg.State)
	require.Nil(t, dlg.proxyInput)
}

// TestOAuthCodexRejectsBadProxy: an unusable value keeps the user on the
// step, rather than starting a sign-in that fails a few seconds later with
// something less obviously about the proxy.
func TestOAuthCodexRejectsBadProxy(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	dlg.proxyInput.SetValue("ftp://nope:21")

	action := dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	require.Equal(t, OAuthStateProxy, dlg.State, "a bad proxy must not start the flow")
	require.IsType(t, ActionCmd{}, action, "the failure must be reported to the user")
}

// TestOAuthCodexAcceptsProxy: a good value is handed to the provider and
// moves the dialog on to authenticating.
func TestOAuthCodexAcceptsProxy(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	dlg.proxyInput.SetValue("  socks5://127.0.0.1:1080  ")

	dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	require.Equal(t, OAuthStateInitializing, dlg.State)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)
	require.Equal(t, "socks5://127.0.0.1:1080", provider.proxy, "surrounding whitespace must be trimmed")
}

// TestOAuthCodexEmptyProxyIsAllowed: the setting is optional, and an empty
// field means "whatever the environment does".
func TestOAuthCodexEmptyProxyIsAllowed(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)

	dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	require.Equal(t, OAuthStateInitializing, dlg.State)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)
	require.Empty(t, provider.proxy)
}

// TestOAuthCodexProxyStepTypes: keys that are not enter or escape edit the
// field instead of falling through to the display-state bindings, where "c"
// and "u" are copy shortcuts.
func TestOAuthCodexProxyStepTypes(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	for _, r := range "custom" {
		dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}

	require.Equal(t, "custom", dlg.proxyInput.Value())
	require.Equal(t, OAuthStateProxy, dlg.State)
}
