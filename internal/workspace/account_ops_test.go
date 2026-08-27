package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/stretchr/testify/require"
)

// newAccountTestProvider starts a throwaway discovery endpoint and
// configures a custom provider against it, mirroring
// TestConfigureCustomProvider_WritesFieldsAndDiscoversModels — accounts
// need a real, already-configured provider entry to attach to. It
// deliberately configures no APIKey: RecordAccount migrates any
// pre-existing legacy credential on the provider into an account of its
// own (see config.RecordAccount's doc comment, step 1), and these tests
// need to control the account count precisely.
func newAccountTestProvider(t *testing.T, ws *testConfigAccessor, providerID string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "model-a"}]}`))
	}))
	t.Cleanup(server.Close)

	_, err := ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, ConfigureCustomProviderParams{
		ID:      providerID,
		BaseURL: server.URL + "/v1",
		Type:    string(catwalk.TypeOpenAICompat),
	})
	require.NoError(t, err)
}

// setupTwoAccountProvider configures a custom provider and records two
// accounts on it, returning their IDs in creation order. The second
// RecordAccount call ends up active, since RecordAccount always activates
// whatever it just recorded.
func setupTwoAccountProvider(t *testing.T) (ws *testConfigAccessor, providerID string, first, second accounts.Account) {
	t.Helper()
	ws, _ = newTestConfigAccessor(t)
	providerID = "acct-test-provider"
	newAccountTestProvider(t, ws, providerID)

	var err error
	first, err = ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey: "key-one", Label: "First", ForceNewAccount: true,
	})
	require.NoError(t, err)

	second, err = ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey: "key-two", Label: "Second", ForceNewAccount: true,
	})
	require.NoError(t, err)

	return ws, providerID, first, second
}

// TestRemoveAccount_LastAccountRefused pins the rule that a provider must
// never be left with credentials configured but no account backing them —
// that's what `sennit logout` is for, not account deletion.
func TestRemoveAccount_LastAccountRefused(t *testing.T) {
	ws, _ := newTestConfigAccessor(t)
	providerID := "acct-test-provider"
	newAccountTestProvider(t, ws, providerID)

	only, err := ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey: "key-one", Label: "Only", ForceNewAccount: true,
	})
	require.NoError(t, err)

	err = ws.RemoveAccount(config.ScopeGlobal, providerID, only.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sennit logout")

	remaining, err := ws.ListAccounts(providerID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "the refused removal must not have deleted anything")
}

// TestRemoveAccount_ActiveAccountActivatesReplacementFirst covers removing
// the active account: a different account must become active before the
// removed one disappears, so the provider's "account" pointer never dangles.
func TestRemoveAccount_ActiveAccountActivatesReplacementFirst(t *testing.T) {
	ws, providerID, first, second := setupTwoAccountProvider(t)

	pc, ok := ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, second.ID, pc.Account, "RecordAccount should have left the second account active")

	require.NoError(t, ws.RemoveAccount(config.ScopeGlobal, providerID, second.ID))

	pc, ok = ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, first.ID, pc.Account, "the surviving account must now be active")

	remaining, err := ws.ListAccounts(providerID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, first.ID, remaining[0].ID)
}

// TestRemoveAccount_InactiveAccountJustRemoved covers the simple case: no
// activation dance needed for an account that was not active.
func TestRemoveAccount_InactiveAccountJustRemoved(t *testing.T) {
	ws, providerID, first, second := setupTwoAccountProvider(t)

	pc, ok := ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, second.ID, pc.Account)

	require.NoError(t, ws.RemoveAccount(config.ScopeGlobal, providerID, first.ID))

	pc, ok = ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, second.ID, pc.Account, "removing the inactive account must not disturb the active one")

	remaining, err := ws.ListAccounts(providerID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, second.ID, remaining[0].ID)
}

// TestUpdateAccount_ActiveAccountProxyChangePublishesToRuntime is the
// regression test the task calls out by name: editing the active account's
// proxy must reach the running config's effective ProxyURL immediately, not
// just the on-disk account file, or requests keep going out the old route
// until the next restart.
func TestUpdateAccount_ActiveAccountProxyChangePublishesToRuntime(t *testing.T) {
	ws, providerID, _, second := setupTwoAccountProvider(t)

	pc, ok := ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, second.ID, pc.Account)
	require.Empty(t, pc.ProxyURL, "no proxy configured yet")

	updated := second
	updated.ProxyURL = "http://proxy.example:8080"
	require.NoError(t, ws.UpdateAccount(providerID, updated))

	pc, ok = ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, "http://proxy.example:8080", pc.ProxyURL, "the new proxy must be live in the running config")

	stored, ok, err := accountsStoreFor(t).Get(providerID, second.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "http://proxy.example:8080", stored.ProxyURL, "and persisted to the account file")
}

// TestUpdateAccount_InactiveAccountDoesNotTouchRuntime covers the other
// branch: editing an account that is not currently active must not
// republish anything to the live ProviderConfig.
func TestUpdateAccount_InactiveAccountDoesNotTouchRuntime(t *testing.T) {
	ws, providerID, first, second := setupTwoAccountProvider(t)

	pc, ok := ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, second.ID, pc.Account, "second account is active, first is not")

	updated := first
	updated.Label = "Renamed"
	updated.ProxyURL = "http://proxy.example:9090"
	require.NoError(t, ws.UpdateAccount(providerID, updated))

	pc, ok = ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Empty(t, pc.ProxyURL, "editing the inactive account must not change the live provider's proxy")

	stored, ok, err := accountsStoreFor(t).Get(providerID, first.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "Renamed", stored.Label)
	require.Equal(t, "http://proxy.example:9090", stored.ProxyURL)
}

// accountsStoreFor opens the same account file newTestConfigAccessor
// pointed SENNIT_GLOBAL_DATA at for the currently running test.
func accountsStoreFor(t *testing.T) accounts.Store {
	t.Helper()
	return accounts.NewFileStore(config.GlobalAccountsFile())
}
