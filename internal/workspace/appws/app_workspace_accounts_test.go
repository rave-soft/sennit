package appws

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestUpdateAccountFields_LabelEditKeepsTokenByteIdentical is CLIENT-SERVER.md
// PR 0.5c's central guarantee: editing an OAuth account's label (the only
// thing a frontend can send back today) must not disturb its stored
// credential at all. UpdateAccountFields never takes a Token/APIKey from
// the caller - AccountEdit has no field for either - so this proves the
// stored account really does come back with the same token, not merely
// that the type system happens to prevent the caller from setting one.
func TestUpdateAccountFields_LabelEditKeepsTokenByteIdentical(t *testing.T) {
	w := newOAuthTestWorkspace(t)

	originalToken := &oauth.Token{
		AccessToken:  "access-original",
		RefreshToken: "refresh-original",
		ExpiresIn:    3600,
		ExpiresAt:    1234567890,
	}
	// recordAccount (unexported), not RecordAccount: the public method's
	// contract type, workspace.AccountCredential, carries no Token (see its
	// doc comment) - this test needs an OAuth account on file, which only
	// the internal helper the OAuth completion path itself uses can create.
	recorded, err := w.recordAccount(config.ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:           originalToken,
		AccountID:       "acct-remote-1",
		ForceNewAccount: true,
	})
	require.NoError(t, err)
	require.True(t, recorded.HasToken)

	// This is what a frontend actually sends back today (see
	// AccountForm.submit): only Label, ProxyURL and Disabled ever change.
	newLabel := "Renamed"
	require.NoError(t, w.UpdateAccountFields(codex.ProviderID, recorded.ID, workspace.AccountEdit{Label: &newLabel}))

	store := w.accountStore()
	stored, ok, err := store.Get(codex.ProviderID, recorded.ID)
	require.NoError(t, err)
	require.True(t, ok)

	require.Equal(t, newLabel, stored.Label, "the edited field must apply")
	require.NotNil(t, stored.Token)
	require.Equal(t, *originalToken, *stored.Token, "the token must survive a label edit byte-identical")
	require.Empty(t, stored.APIKey, "an OAuth account must not grow an api key from an unrelated edit")
}

// TestUpdateAccountFields_APIKeyEditKeepsAPIKeyUnchanged is the api-key
// mirror of the OAuth test above: editing the proxy of an api-key account
// must not disturb its stored key template.
func TestUpdateAccountFields_APIKeyEditKeepsAPIKeyUnchanged(t *testing.T) {
	t.Setenv("SENNIT_ACCOUNTS_TEST_KEY", "resolved-secret-value")
	w := newOAuthTestWorkspace(t)

	recorded, err := w.RecordAccount(config.ScopeGlobal, "openai", workspace.AccountCredential{
		APIKey:          "$SENNIT_ACCOUNTS_TEST_KEY",
		ForceNewAccount: true,
	})
	require.NoError(t, err)
	require.True(t, recorded.HasAPIKey)

	newProxy := "http://proxy.example:8080"
	require.NoError(t, w.UpdateAccountFields("openai", recorded.ID, workspace.AccountEdit{ProxyURL: &newProxy}))

	store := w.accountStore()
	stored, ok, err := store.Get("openai", recorded.ID)
	require.NoError(t, err)
	require.True(t, ok)

	require.Equal(t, newProxy, stored.ProxyURL)
	require.Equal(t, "$SENNIT_ACCOUNTS_TEST_KEY", stored.APIKey, "the literal api key template must survive a proxy edit unchanged")
	require.Nil(t, stored.Token)
}

// TestListAccounts_JSONNeverLeaksCredential is the secret check CLIENT-
// SERVER.md PR 0.5c calls for: marshaling what ListAccounts hands back to
// a frontend must contain neither the access token, the refresh token,
// nor the api key template - only the presence/expiry facts FrontendAccount
// carries.
func TestListAccounts_JSONNeverLeaksCredential(t *testing.T) {
	t.Setenv("SENNIT_ACCOUNTS_TEST_KEY", "resolved-secret-value")
	w := newOAuthTestWorkspace(t)

	// recordAccount (unexported): see the comment on the same call in
	// TestUpdateAccountFields_LabelEditKeepsTokenByteIdentical above.
	_, err := w.recordAccount(config.ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token: &oauth.Token{
			AccessToken:  "super-secret-access-token",
			RefreshToken: "super-secret-refresh-token",
			ExpiresIn:    3600,
		},
		AccountID:       "acct-remote-1",
		ForceNewAccount: true,
	})
	require.NoError(t, err)

	_, err = w.RecordAccount(config.ScopeGlobal, "openai", workspace.AccountCredential{
		APIKey:          "$SENNIT_ACCOUNTS_TEST_KEY",
		ForceNewAccount: true,
	})
	require.NoError(t, err)

	for _, providerID := range []string{codex.ProviderID, "openai"} {
		accs, err := w.ListAccounts(providerID)
		require.NoError(t, err)
		require.NotEmpty(t, accs)

		data, err := json.Marshal(accs)
		require.NoError(t, err)
		s := string(data)
		require.NotContains(t, s, "super-secret-access-token")
		require.NotContains(t, s, "super-secret-refresh-token")
		require.NotContains(t, s, "SENNIT_ACCOUNTS_TEST_KEY")
		require.NotContains(t, s, "resolved-secret-value")
	}
}
