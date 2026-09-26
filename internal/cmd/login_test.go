package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// copilotLoginWorkspaceFake observes the OAuth boundary loginCopilot now
// uses: the sign-in itself, including completing it, lives behind the
// workspace (StartOAuth/OAuthFlow.Wait — see workspace.OAuthController and
// CLIENT-SERVER.md PR 1.3), so what this fake records is what the CLI
// asked the backend to do, not the individual account writes the backend
// performs on its own (those are covered in internal/workspace/appws).
type copilotLoginWorkspaceFake struct {
	stubConfigAccessor

	startResult workspace.OAuthStartResult
	startFlow   *stubOAuthFlow
	startErr    error

	// importResult/importErr are ImportCopilot's canned answer.
	importResult bool
	importErr    error

	calls         []string
	startProxies  []string
	startForceNew []bool
}

func (w *copilotLoginWorkspaceFake) ImportCopilot(context.Context) (bool, error) {
	w.calls = append(w.calls, "ImportCopilot")
	return w.importResult, w.importErr
}

func (w *copilotLoginWorkspaceFake) StartOAuth(_ context.Context, providerID, proxyURL string, forceNewAccount bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	w.calls = append(w.calls, "StartOAuth:"+providerID)
	w.startProxies = append(w.startProxies, proxyURL)
	w.startForceNew = append(w.startForceNew, forceNewAccount)
	if w.startErr != nil {
		return workspace.OAuthStartResult{}, nil, w.startErr
	}
	if w.startFlow == nil {
		return w.startResult, nil, nil
	}
	return w.startResult, w.startFlow, nil
}

func (w *copilotLoginWorkspaceFake) OAuthConfiguredProxy(string) string { return "" }

func (w *copilotLoginWorkspaceFake) OAuthValidateProxy(string, string) error { return nil }

func (w *copilotLoginWorkspaceFake) ListAccounts(string) ([]workspace.FrontendAccount, error) {
	return nil, nil
}

// newCopilotLoginFake builds a fake whose device flow completes
// immediately through a stub flow, so no test here blocks on the
// interactive "press enter" step.
func newCopilotLoginFake() *copilotLoginWorkspaceFake {
	return &copilotLoginWorkspaceFake{
		startResult: workspace.OAuthStartResult{
			DeviceCode:      "dev-code",
			UserCode:        "USER-CODE",
			VerificationURL: "https://github.com/login/device",
			ExpiresIn:       600,
			Interval:        5,
		},
		startFlow: &stubOAuthFlow{
			completion: workspace.OAuthCompletion{
				Account:       workspace.FrontendAccount{ID: "acct-copilot"},
				ModelsFetched: -1,
			},
		},
	}
}

// TestLoginCopilot_StartsDeviceFlow pins the boundary: with no disk login
// to import (importResult false, the zero value), the CLI asks the
// workspace to start the device flow and waits on it to complete, without
// performing the account write itself, and threads forceNewAccount
// through to StartOAuth (Copilot's token carries no account identifier of
// its own, so RecordAccount has no other way to tell a deliberate second
// sign-in from a routine re-login — see
// accounts.LegacyCredential.ForceNewAccount).
func TestLoginCopilot_StartsDeviceFlow(t *testing.T) {
	t.Parallel()

	for _, forceNewAccount := range []bool{false, true} {
		ws := newCopilotLoginFake()
		require.NoError(t, loginCopilot(ws, true, forceNewAccount, recordLoginIO(t)))
		if forceNewAccount {
			// A deliberate "add account" skips ImportCopilot entirely —
			// see loginCopilot's own comment on why.
			require.Equal(t, []string{"StartOAuth:copilot"}, ws.calls)
		} else {
			require.Equal(t, []string{"ImportCopilot", "StartOAuth:copilot"}, ws.calls)
		}
		require.Equal(t, []bool{forceNewAccount}, ws.startForceNew)
		require.Equal(t, 1, ws.startFlow.cancelled, "the flow must be released once the sign-in completes")
	}
}

// TestLoginCopilot_DiskLoginSkipsDeviceFlow is the regression test for the
// CLI's on-disk shortcut: `sennit login copilot` with a GitHub Copilot CLI
// login already on this machine must import it through ImportCopilot
// without ever starting a device flow — matching the pre-workspace CLI's
// own disk-token-reuse path (copilot.RefreshTokenFromDisk).
func TestLoginCopilot_DiskLoginSkipsDeviceFlow(t *testing.T) {
	t.Parallel()

	ws := newCopilotLoginFake()
	ws.importResult = true
	require.NoError(t, loginCopilot(ws, true, false, recordLoginIO(t)))
	require.Equal(t, []string{"ImportCopilot"}, ws.calls, "a disk login must not start a device flow")
}

// TestLoginCopilot_ForceNewAccountSkipsDiskLogin covers the reason
// ImportCopilot is skipped for a deliberate "add account" even when a
// disk login is present (importResult is set to true here, to prove the
// skip does not depend on whether an import would have succeeded): it has
// no notion of "which account", so `sennit accounts add copilot` must
// always reach the device flow, which threads forceNewAccount into
// RecordAccount correctly.
func TestLoginCopilot_ForceNewAccountSkipsDiskLogin(t *testing.T) {
	t.Parallel()

	ws := newCopilotLoginFake()
	ws.importResult = true
	require.NoError(t, loginCopilot(ws, true, true, recordLoginIO(t)))
	require.Equal(t, []string{"StartOAuth:copilot"}, ws.calls)
	require.Equal(t, []bool{true}, ws.startForceNew)
}

// TestLoginCopilot_ImportFailureIsFatal covers ImportCopilot itself
// failing (a disk token found but the exchange or the persist failed):
// unlike "nothing to import" (false, nil), this is an error and must stop
// the command rather than fall through to a device flow.
func TestLoginCopilot_ImportFailureIsFatal(t *testing.T) {
	t.Parallel()

	importErr := errors.New("exchanging github copilot token: boom")
	ws := newCopilotLoginFake()
	ws.importErr = importErr
	require.ErrorIs(t, loginCopilot(ws, true, false, recordLoginIO(t)), importErr)
	require.Equal(t, []string{"ImportCopilot"}, ws.calls)
}

// TestLoginCopilot_ReusedLoginSkipsDeviceFlow covers StartOAuth reporting
// a sign-in already completed with nothing to show (Completed set): the
// CLI must not wait on a flow that was never started.
func TestLoginCopilot_ReusedLoginSkipsDeviceFlow(t *testing.T) {
	t.Parallel()

	completion := workspace.OAuthCompletion{Account: workspace.FrontendAccount{ID: "acct-copilot"}, ModelsFetched: -1}
	ws := &copilotLoginWorkspaceFake{
		startResult: workspace.OAuthStartResult{Completed: &completion},
	}
	require.NoError(t, loginCopilot(ws, true, false, recordLoginIO(t)))
	require.Equal(t, []string{"ImportCopilot", "StartOAuth:copilot"}, ws.calls)
}

// TestLoginCopilot_StartOAuthFailureIsFatal covers a device-code request
// that fails outright.
func TestLoginCopilot_StartOAuthFailureIsFatal(t *testing.T) {
	t.Parallel()

	startErr := errors.New("device code request failed")
	ws := newCopilotLoginFake()
	ws.startErr = startErr
	require.ErrorIs(t, loginCopilot(ws, true, false, recordLoginIO(t)), startErr)
}

// TestLoginCmd_KeepsAuthAlias pins that `sennit auth <platform>` keeps
// working. The account-management group is named "accounts" precisely so
// this alias can stay: taking "auth" for the group would turn a working
// login command into an unknown-subcommand error for anyone whose scripts
// already use it.
func TestLoginCmd_KeepsAuthAlias(t *testing.T) {
	t.Parallel()

	require.Contains(t, loginCmd.Aliases, "auth")
	require.NotEqual(t, "auth", accountsCmd.Use)
	require.NotContains(t, accountsCmd.Aliases, "auth")
}

func TestLoginCmd_ForceFlag(t *testing.T) {
	t.Parallel()

	flag := loginCmd.Flags().Lookup("force")
	require.NotNil(t, flag)
	require.Equal(t, "f", flag.Shorthand)
}
