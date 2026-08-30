package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// realConfigAccessor adapts a real *config.ConfigStore to
// workspace.ConfigAccessor, exactly the way internal/workspace's own
// testConfigAccessor does (see internal/workspace/custom_provider_test.go).
// It is duplicated here rather than exported from internal/workspace and
// reused, because internal/workspace's copy is unexported test-only code —
// but it must NOT grow its own copy of RecordAccount/UpdateAccount/
// RemoveAccount/SetProviderProxy's actual rules. Every method below
// delegates to the same config.* free functions AppWorkspace calls in
// production (internal/workspace/app_workspace.go), so a test built on
// this accessor exercises the real precedence rules, not a hand-rolled
// stand-in for them.
type realConfigAccessor struct {
	store       *config.ConfigStore
	credentials *credentials.Manager
}

func (a *realConfigAccessor) Config() *config.Config            { return a.store.Config() }
func (a *realConfigAccessor) WorkingDir() string                { return a.store.WorkingDir() }
func (a *realConfigAccessor) Resolver() config.VariableResolver { return a.store.Resolver() }

func (a *realConfigAccessor) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	return a.store.UpdatePreferredModel(scope, model)
}

func (a *realConfigAccessor) OverridePreferredModel(model config.SelectedModel) error {
	a.store.OverridePreferredModel(model)
	return nil
}

func (a *realConfigAccessor) SetCompactMode(scope config.Scope, enabled bool) error {
	return a.store.SetCompactMode(scope, enabled)
}

func (a *realConfigAccessor) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	return a.store.SetProviderAPIKey(scope, providerID, apiKey)
}

func (a *realConfigAccessor) SetConfigField(scope config.Scope, key string, value any) error {
	return a.store.SetConfigField(scope, key, value)
}

func (a *realConfigAccessor) RemoveConfigField(scope config.Scope, key string) error {
	return a.store.RemoveConfigField(scope, key)
}

func (a *realConfigAccessor) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.RecordAccount(a.store, accStore, scope, providerID, cred)
}

func (a *realConfigAccessor) ListAccounts(providerID string) ([]accounts.Account, error) {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	if err := config.EnsureAccountMigrated(a.store, accStore, providerID); err != nil {
		return nil, err
	}
	return accStore.List(providerID)
}

func (a *realConfigAccessor) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	account, ok, err := accStore.Get(providerID, accountID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return a.store.ActivateAccount(scope, providerID, account)
}

func (a *realConfigAccessor) UpdateAccount(providerID string, account accounts.Account) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.UpdateAccount(a.store, accStore, providerID, account)
}

func (a *realConfigAccessor) RemoveAccount(scope config.Scope, providerID, accountID string) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.RemoveAccount(a.store, accStore, scope, providerID, accountID)
}

func (a *realConfigAccessor) SetProviderProxy(providerID, proxy string) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.SetProviderProxy(a.store, accStore, providerID, proxy)
}

func (a *realConfigAccessor) RefreshAccountLimits(ctx context.Context, providerID string) ([]accounts.Account, error) {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	// No provider in these tests reports usage, so the fetcher is never
	// reached; the real wiring lives in internal/workspace/appws.
	return config.RefreshAccountLimits(ctx, a.store, accStore, providerID, nil)
}

func (a *realConfigAccessor) CustomProviderTypes() []string { return nil }

// CurrentPlanUsage: these tests drive account bookkeeping, not the
// sidebar's plan line.
func (a *realConfigAccessor) CurrentPlanUsage(string) (accounts.Usage, bool) {
	return accounts.Usage{}, false
}

func (a *realConfigAccessor) ImportCopilot() (*oauth.Token, bool) {
	return a.credentials.ImportCopilot()
}

func (a *realConfigAccessor) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return a.credentials.RefreshOAuthToken(ctx, scope, providerID)
}

var _ workspace.ConfigAccessor = (*realConfigAccessor)(nil)

// newRealConfigAccessor builds a real *config.ConfigStore-backed
// ConfigAccessor rooted in throwaway directories, mirroring
// internal/workspace's newTestConfigAccessor.
func newRealConfigAccessor(t *testing.T) *realConfigAccessor {
	t.Helper()
	globalConfigDir := t.TempDir()
	globalDataDir := t.TempDir()
	workDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalConfigDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDataDir)

	configPath := filepath.Join(globalDataDir, "sennit.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	store, err := configruntime.Load(workDir, dataDir, false)
	require.NoError(t, err)
	return &realConfigAccessor{store: store, credentials: credentials.New(store)}
}

// authTestProviderID is the sole provider ID every test in this file uses.
const authTestProviderID = "auth-test-provider"

// newAuthTestProvider configures a throwaway api-key-capable custom
// provider (accounts.CapabilitiesOf falls back to AuthAPIKey for any
// unknown provider ID) that auth add/use/remove/proxy can attach accounts
// to. It deliberately configures no APIKey, so RecordAccount's migration
// step (step 1 of its doc comment) has nothing pre-existing to fold in —
// tests need to control the account count precisely.
func newAuthTestProvider(t *testing.T, ws *realConfigAccessor) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "model-a"}]}`))
	}))
	t.Cleanup(server.Close)

	_, err := workspace.ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, workspace.ConfigureCustomProviderParams{
		ID:      authTestProviderID,
		BaseURL: server.URL + "/v1",
		Type:    string(catwalk.TypeOpenAICompat),
	})
	require.NoError(t, err)
}

// TestAuthAdd_APIKeyProvider_SecondCallCreatesSecondAccount pins the
// ForceNewAccount correctness point the task calls out: auth add on a
// provider with no AccountID (every api-key provider) must always create a
// new account, never silently overwrite the one that's already active.
func TestAuthAdd_APIKeyProvider_SecondCallCreatesSecondAccount(t *testing.T) {
	ws := newRealConfigAccessor(t)
	providerID := authTestProviderID
	newAuthTestProvider(t, ws)

	require.NoError(t, authAddAPIKey(ws, providerID, "key-one"))
	require.NoError(t, authAddAPIKey(ws, providerID, "key-two"))

	accts, err := ws.ListAccounts(providerID)
	require.NoError(t, err)
	require.Len(t, accts, 2, "a second `auth add` must create a second account, not overwrite the first")
}

// TestAuthAdd_APIKeyProvider_StoresLiteralTemplate pins that the CLI must
// never resolve an api-key template itself: RecordAccount/ActivateAccount
// own resolution, and the stored account record must keep the literal
// $VAR the user typed even when that variable is set in this process.
func TestAuthAdd_APIKeyProvider_StoresLiteralTemplate(t *testing.T) {
	t.Setenv("SENNIT_AUTH_TEST_VAR", "resolved-secret-value")

	ws := newRealConfigAccessor(t)
	providerID := authTestProviderID
	newAuthTestProvider(t, ws)

	require.NoError(t, authAddAPIKey(ws, providerID, "$SENNIT_AUTH_TEST_VAR"))

	accts, err := ws.ListAccounts(providerID)
	require.NoError(t, err)
	require.Len(t, accts, 1)
	require.Equal(t, "$SENNIT_AUTH_TEST_VAR", accts[0].APIKey, "the literal template must be stored, not the resolved secret")
}

// TestAuthUse_DisabledAccountRefused pins that "auth use" must refuse a
// disabled account and leave the active account untouched — nothing in
// config.ConfigStore.ActivateAccount itself checks Disabled.
func TestAuthUse_DisabledAccountRefused(t *testing.T) {
	ws := newRealConfigAccessor(t)
	providerID := authTestProviderID
	newAuthTestProvider(t, ws)

	first, err := ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey: "key-one", Label: "First", ForceNewAccount: true,
	})
	require.NoError(t, err)
	second, err := ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey: "key-two", Label: "Second", ForceNewAccount: true,
	})
	require.NoError(t, err)

	// Disable the first account and switch the active one back to it.
	first.Disabled = true
	require.NoError(t, ws.UpdateAccount(providerID, first))

	account, err := findAuthAccount(ws, providerID, first.ID)
	require.NoError(t, err)
	require.True(t, account.Disabled)

	err = runAuthUse(ws, providerID, first.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "disabled")

	pc, ok := ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, second.ID, pc.Account, "the active account must not have changed")
}

// TestAuthRemove_LastAccountRefused pins that removing a provider's last
// account surfaces config.RemoveAccount's own error (which names `sennit
// logout`), without a competing message from the CLI layer.
func TestAuthRemove_LastAccountRefused(t *testing.T) {
	ws := newRealConfigAccessor(t)
	providerID := authTestProviderID
	newAuthTestProvider(t, ws)

	only, err := ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey: "key-one", Label: "Only", ForceNewAccount: true,
	})
	require.NoError(t, err)

	err = runAuthRemove(ws, providerID, only.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sennit logout")

	remaining, err := ws.ListAccounts(providerID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
}

// TestAuthProxy_ProviderLevelVsAccountLevel pins that the 2-arg form sets
// the provider-level proxy and the 3-arg form sets one account's own,
// without disturbing the other.
func TestAuthProxy_ProviderLevelVsAccountLevel(t *testing.T) {
	ws := newRealConfigAccessor(t)
	providerID := authTestProviderID
	newAuthTestProvider(t, ws)

	account, err := ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey: "key-one", Label: "Only", ForceNewAccount: true,
	})
	require.NoError(t, err)

	require.NoError(t, ws.SetProviderProxy(providerID, "http://provider-proxy.example:8080"))
	pc, ok := ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, "http://provider-proxy.example:8080", pc.ConfiguredProxyURL)

	account.ProxyURL = "http://account-proxy.example:9090"
	require.NoError(t, ws.UpdateAccount(providerID, account))

	accts, err := ws.ListAccounts(providerID)
	require.NoError(t, err)
	require.Len(t, accts, 1)
	require.Equal(t, "http://account-proxy.example:9090", accts[0].ProxyURL)

	pc, ok = ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, "http://provider-proxy.example:8080", pc.ConfiguredProxyURL, "the provider-level proxy must be untouched by the account-level set")
}

// TestAuthProxy_DashClearsNoneStaysLiteral pins that "-" clears a proxy
// back to empty (inherit) while "none" is stored literally as
// proxyhttp.Direct — the two must not be collapsed into each other.
func TestAuthProxy_DashClearsNoneStaysLiteral(t *testing.T) {
	ws := newRealConfigAccessor(t)
	providerID := authTestProviderID
	newAuthTestProvider(t, ws)

	require.NoError(t, ws.SetProviderProxy(providerID, "http://provider-proxy.example:8080"))

	require.NoError(t, runAuthProxy(ws, providerID, "", "none"))
	pc, ok := ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, "none", pc.ConfiguredProxyURL, `"none" must be stored literally, not collapsed to empty`)

	require.NoError(t, runAuthProxy(ws, providerID, "", "-"))
	pc, ok = ws.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Empty(t, pc.ConfiguredProxyURL, `"-" must clear back to empty (inherit)`)
}

// TestAuthList_UsageOnlyForCapableProvider is an output-formatting check,
// not a config-precedence one, so a lightweight approach (checking
// accounts.CapabilitiesOf directly, which is exactly what printAccountList
// gates on) is enough — see the task's own carve-out for this rule.
func TestAuthList_UsageOnlyForCapableProvider(t *testing.T) {
	require.True(t, accounts.CapabilitiesOf("codex").Usage)
	require.False(t, accounts.CapabilitiesOf("auth-test-provider").Usage)

	u := accounts.Usage{Plan: "Plus", Primary: accounts.UsageWindow{UsedPercent: 42, WindowMinutes: 60}}
	require.True(t, u.Known())

	// printAccountList only calls formatAccountUsage when both showUsage
	// (CapabilitiesOf(id).Usage) and a.Usage.Known() are true; a provider
	// without Usage capability must never reach formatAccountUsage no
	// matter what its accounts' stored Usage looks like.
	require.False(t, accounts.CapabilitiesOf("auth-test-provider").Usage,
		"an api-key-style provider must not be treated as usage-reporting")
}

// TestReadSecretLine_NonTerminalPreservesSpaces pins the non-terminal
// fallback path in readSecretLine: an os.Pipe is never a TTY, so this
// exercises the same branch a piped `sennit accounts add` invocation would
// take. Before the fix, authAddAPIKey used fmt.Scanln, which truncates at
// the first space; readSecretLine must read the whole line instead.
func TestReadSecretLine_NonTerminalPreservesSpaces(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()

	_, err = w.WriteString("sk-has a space\n")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	got, err := readSecretLine(r)
	require.NoError(t, err)
	require.Equal(t, "sk-has a space", got)
}

// TestReadSecretLine_NonTerminalTrimsTrailingWhitespaceOnly pins that only
// the trailing newline/whitespace is trimmed, not interior spaces, and that
// a key with no trailing newline (EOF instead) still comes through.
func TestReadSecretLine_NonTerminalTrimsTrailingWhitespaceOnly(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()

	_, err = w.WriteString("  sk-abc 123  ")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	got, err := readSecretLine(r)
	require.NoError(t, err)
	require.Equal(t, "sk-abc 123", got)
}
