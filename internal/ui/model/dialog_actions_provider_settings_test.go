package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestApplyProviderDialogAction_SubmitProviderSettings_RotationOnlyLeavesProxyUntouched
// is the end-to-end regression test for the proxy-password-deletion
// defect: the settings dialog pre-fills its proxy field from
// FrontendProvider.ProxyURL, which already had its userinfo password
// stripped for display (redactProxyURL). Submitting after touching only
// the rotation toggle must not call SetProviderProxy at all - sending the
// redacted display value back would silently overwrite the real stored
// password.
func TestApplyProviderDialogAction_SubmitProviderSettings_RotationOnlyLeavesProxyUntouched(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		providerProxyURL:    "http://user:pw@proxy.example:8080",
		accountCapabilities: workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold},
	}
	m := newCmdDrivenUI(t, ws)

	dlg, _ := dialog.NewProviderSettings(m.com, "test-provider")

	require.Nil(t, dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})) // -> Enabled field
	toggleAction := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	require.Nil(t, toggleAction)

	submitAction := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := submitAction.(dialog.ActionSubmitProviderSettings)
	require.True(t, ok, "expected ActionSubmitProviderSettings, got %#v", submitAction)
	require.Nil(t, submit.Proxy, "an untouched proxy field must not be sent")
	require.NotNil(t, submit.Rotation)

	cmd, handled := m.applyProviderDialogAction(submit)
	require.True(t, handled)
	require.NotNil(t, cmd)
	runGuardedCmd(m, cmd)

	require.Zero(t, ws.setProviderProxyCalls, "SetProviderProxy must not run when the proxy field was never touched")
	require.Equal(t, "http://user:pw@proxy.example:8080", ws.providerProxyURL, "the stored proxy (with its password) must be unchanged")
}

// TestApplyProviderDialogAction_SubmitProviderSettings_EditedProxySent is
// the control case: actually editing the proxy field must still reach
// SetProviderProxy with exactly what was typed.
func TestApplyProviderDialogAction_SubmitProviderSettings_EditedProxySent(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		providerProxyURL:    "http://user:pw@proxy.example:8080",
		accountCapabilities: workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold},
	}
	m := newCmdDrivenUI(t, ws)

	dlg, _ := dialog.NewProviderSettings(m.com, "test-provider")
	for range "http://user@proxy.example:8080" {
		dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	for _, r := range "http://h2:1" {
		dlg.HandleMsg(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	submitAction := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := submitAction.(dialog.ActionSubmitProviderSettings)
	require.True(t, ok, "expected ActionSubmitProviderSettings, got %#v", submitAction)
	require.NotNil(t, submit.Proxy)
	require.Equal(t, "http://h2:1", *submit.Proxy)

	cmd, handled := m.applyProviderDialogAction(submit)
	require.True(t, handled)
	require.NotNil(t, cmd)
	runGuardedCmd(m, cmd)

	require.Equal(t, 1, ws.setProviderProxyCalls)
	require.Equal(t, "http://h2:1", ws.lastSetProviderProxy)
}
