package cmd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Logout used to clear only providers.<id>.api_key/.oauth, leaving the
// OAuth token itself in the account store — so `sennit accounts use
// <provider> <id>` republished it and the user was logged back in without
// re-authenticating. config.RemoveAccount cannot close that hole (it
// refuses a provider's last account and points the user at logout), so
// logout goes through PurgeAccounts instead.
func TestLogoutProvider_PurgesStoredAccounts(t *testing.T) {
	t.Parallel()

	ws := &stubConfigAccessor{}
	require.NoError(t, logoutCopilot(ws))

	require.Equal(t, []string{"copilot"}, ws.purged,
		"logout must revoke the stored credential, not only the config fields")
}

// A field removal failing must not skip the purge: a half-done logout that
// left the token on disk is the exact failure this guards.
func TestLogoutProvider_PurgesEvenWhenFieldRemovalFails(t *testing.T) {
	t.Parallel()

	wantErr := fmt.Errorf("oauth removal failed")
	ws := &stubConfigAccessor{errs: map[string]error{
		"providers.copilot.oauth": wantErr,
	}}

	require.ErrorIs(t, logoutCopilot(ws), wantErr)
	require.Equal(t, []string{"copilot"}, ws.purged)
}

// The purge's own failure is reported when nothing earlier failed, rather
// than being swallowed into a success message.
func TestLogoutProvider_ReportsPurgeFailure(t *testing.T) {
	t.Parallel()

	wantErr := fmt.Errorf("purge failed")
	ws := &stubConfigAccessor{purgeErr: wantErr}

	require.ErrorIs(t, logoutCopilot(ws), wantErr)
}
