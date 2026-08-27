package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
