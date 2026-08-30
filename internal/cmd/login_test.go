package cmd

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/stretchr/testify/require"
)

// recordAccountConfigAccessor extends stubConfigAccessor with a
// RecordAccount that captures its argument, which recordCopilotAccount's
// tests need to assert on but the shared stub (a no-op there) does not
// track.
type recordAccountConfigAccessor struct {
	stubConfigAccessor
	recordedProvider string
	recorded         []accounts.LegacyCredential
}

func (s *recordAccountConfigAccessor) RecordAccount(_ config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	s.recordedProvider = providerID
	s.recorded = append(s.recorded, cred)
	return accounts.Account{ID: "acct-copilot"}, nil
}

// TestRecordCopilotAccount_RoutesThroughRecordAccount guards the fix for a
// bug where loginCopilot ended in SetProviderAPIKey, which overwrites
// providers.copilot.oauth outright instead of adding/updating an account
// on record — so `sennit accounts add copilot` never actually recorded a
// second account, contradicting authAddOAuth's doc comment. It also pins
// that ForceNewAccount is threaded through unchanged, since Copilot's
// token carries no account identifier RecordAccount could otherwise use
// to tell a deliberate second sign-in from a routine re-login.
func TestRecordCopilotAccount_RoutesThroughRecordAccount(t *testing.T) {
	t.Parallel()

	token := &oauth.Token{AccessToken: "tok"}

	for _, forceNewAccount := range []bool{false, true} {
		ws := &recordAccountConfigAccessor{}
		_, err := recordCopilotAccount(ws, token, forceNewAccount)
		require.NoError(t, err)
		require.Equal(t, "copilot", ws.recordedProvider)
		require.Len(t, ws.recorded, 1)
		require.Equal(t, token, ws.recorded[0].Token)
		require.Equal(t, forceNewAccount, ws.recorded[0].ForceNewAccount)
	}
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
