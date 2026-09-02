package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"

	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestOAuthCopilotStartPollingWithoutFlowReportsError is the regression
// test for a nil-pointer panic: startPolling used to snapshot the device
// code unconditionally and hand it straight to the poll, which
// dereferences it. A late ActionInitiateOAuth from an abandoned initiate
// (see TestOAuthCopilotInitiateAuthNoopsOnceStopped) landing on a fresh
// dialog whose own initiate has not completed yet used to reach here with
// nothing started at all.
func TestOAuthCopilotStartPollingWithoutFlowReportsError(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	m := &OAuthCopilot{com: &common.Common{Styles: &s}}

	cmd := m.startPolling("", 0)
	require.NotNil(t, cmd)

	var msg any
	require.NotPanics(t, func() { msg = cmd() })
	errored, ok := msg.(ActionOAuthErrored)
	require.True(t, ok, "expected ActionOAuthErrored, got %#v", msg)
	require.Error(t, errored.Error)
}

// TestOAuthCopilotInitiateAuthNoopsOnceStopped covers the other half of the
// same fix: esc during "Initializing" (OAuth.HandleMsg's Close case) calls
// stopPolling before the device-code request has necessarily returned.
// initiateAuth must notice stopped==true and drop its result rather than
// writing m.deviceCode and returning ActionInitiateOAuth for a dialog that
// is already gone — which, addressed only by the constant OAuthID, would
// otherwise land on whatever OAuth dialog opens next.
//
// stopPolling is called before initiateAuth here specifically so the
// stopped check at the top short-circuits before any network call, keeping
// this hermetic.
func TestOAuthCopilotInitiateAuthNoopsOnceStopped(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	m := &OAuthCopilot{com: &common.Common{Styles: &s}}

	m.stopPolling()
	msg := m.initiateAuth()

	require.Nil(t, msg, "a dismissed dialog's initiate result must not surface")
	require.Nil(t, m.flow, "a dropped result must not write the flow either")
}

// TestOAuthCopilotUsesConfiguredProxy pins B10: a proxy already set for the
// copilot provider must be what the device flow uses, without the dialog
// growing a proxy-entry step of its own (that would change every Copilot
// sign-in's UX, not just the ones that need a proxy — see
// TestOAuthCopilotSkipsProxyStep).
func TestOAuthCopilotUsesConfiguredProxy(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	com := &common.Common{
		Styles:    &s,
		Workspace: &completeOAuthTestWorkspace{configuredProxy: "socks5://127.0.0.1:1080"},
	}

	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, _ := NewOAuthCopilot(com, false, provider, nil, false)

	oc, ok := dlg.oAuthProvider.(*OAuthCopilot)
	require.True(t, ok)
	require.Equal(t, "socks5://127.0.0.1:1080", oc.proxy)
}

// TestOAuthCopilotNoProxyConfigured pins the no-proxy case unchanged: a
// user with nothing configured gets an empty proxy, exactly like before any
// of this proxy support existed.
func TestOAuthCopilotNoProxyConfigured(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	com := &common.Common{Styles: &s}

	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, _ := NewOAuthCopilot(com, false, provider, nil, false)

	oc, ok := dlg.oAuthProvider.(*OAuthCopilot)
	require.True(t, ok)
	require.Empty(t, oc.proxy)
}
