package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// testConfigAccessor adapts a *config.ConfigStore to the ConfigAccessor
// interface. ConfigStore.OverridePreferredModel has no error return (an
// in-memory-only operation), so it needs the same thin wrapper AppWorkspace
// uses in production.
type testConfigAccessor struct {
	store *config.ConfigStore
}

func (a *testConfigAccessor) Config() *config.Config            { return a.store.Config() }
func (a *testConfigAccessor) WorkingDir() string                { return a.store.WorkingDir() }
func (a *testConfigAccessor) Resolver() config.VariableResolver { return a.store.Resolver() }

func (a *testConfigAccessor) UpdatePreferredModel(scope config.Scope, modelType config.SelectedModelType, model config.SelectedModel) error {
	return a.store.UpdatePreferredModel(scope, modelType, model)
}

func (a *testConfigAccessor) OverridePreferredModel(modelType config.SelectedModelType, model config.SelectedModel) error {
	a.store.OverridePreferredModel(modelType, model)
	return nil
}

func (a *testConfigAccessor) SetCompactMode(scope config.Scope, enabled bool) error {
	return a.store.SetCompactMode(scope, enabled)
}

func (a *testConfigAccessor) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	return a.store.SetProviderAPIKey(scope, providerID, apiKey)
}

func (a *testConfigAccessor) SetConfigField(scope config.Scope, key string, value any) error {
	return a.store.SetConfigField(scope, key, value)
}

func (a *testConfigAccessor) RemoveConfigField(scope config.Scope, key string) error {
	return a.store.RemoveConfigField(scope, key)
}

func (a *testConfigAccessor) ImportCopilot() (*oauth.Token, bool) {
	return a.store.ImportCopilot()
}

func (a *testConfigAccessor) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return a.store.RefreshOAuthToken(ctx, scope, providerID)
}

var _ ConfigAccessor = (*testConfigAccessor)(nil)

// newTestConfigAccessor builds a real *config.ConfigStore-backed
// ConfigAccessor rooted at four distinct directories (global config,
// global data, working dir, workspace data dir). They must stay distinct:
// internal/config/load.go's Load merges the global-config path and the
// global-data path as two independent layers (and separately merges a
// third "workspace config" layer from the workspace data dir), and its
// merge library concatenates JSON arrays rather than overriding them, so
// collapsing any of these paths onto the same file double- or triple-counts
// array fields like a provider's models list on every load.
func newTestConfigAccessor(t *testing.T) (accessor *testConfigAccessor, globalDataConfigPath string) {
	t.Helper()
	globalConfigDir := t.TempDir()
	globalDataDir := t.TempDir()
	workDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalConfigDir)
	t.Setenv("BRAID_GLOBAL_DATA", globalDataDir)

	configPath := filepath.Join(globalDataDir, "braid.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	return &testConfigAccessor{store: store}, configPath
}

func TestConfigureCustomProvider_WritesFieldsAndDiscoversModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "model-a"}, {"id": "model-b"}]}`))
	}))
	defer server.Close()

	ws, _ := newTestConfigAccessor(t)

	params := ConfigureCustomProviderParams{
		ID:      "my-custom",
		Name:    "My Custom Provider",
		BaseURL: server.URL + "/v1",
		Type:    string(catwalk.TypeOpenAICompat),
		APIKey:  "test-key",
	}

	models, err := ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, params)
	require.NoError(t, err)
	require.Len(t, models, 2)

	pc, ok := ws.Config().Providers.Get("my-custom")
	require.True(t, ok)
	require.Equal(t, server.URL+"/v1", pc.BaseURL)
	require.Equal(t, catwalk.TypeOpenAICompat, pc.Type)
	require.Equal(t, "My Custom Provider", pc.Name)
	require.Equal(t, "test-key", pc.APIKey)
	require.Len(t, pc.Models, 2)
}

func TestConfigureCustomProvider_NoModelsFoundKeepsFieldsPersisted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": []}`))
	}))
	defer server.Close()

	ws, configPath := newTestConfigAccessor(t)

	params := ConfigureCustomProviderParams{
		ID:      "empty-provider",
		BaseURL: server.URL + "/v1",
		Type:    string(catwalk.TypeOpenAICompat),
	}

	models, err := ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, params)
	require.Error(t, err)
	require.Nil(t, models)

	// A provider with zero models is dropped from the in-memory catalog by
	// the config loader (see discoverCustomProviderModels /
	// validateCustomProviders in internal/config/load.go), but the
	// base_url/type fields we wrote must still be on disk despite the
	// error, so the user can retry via `braid models refresh` without
	// reconfiguring from scratch.
	raw, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, server.URL+"/v1", gjson.GetBytes(raw, "providers.empty-provider.base_url").String())
	require.Equal(t, string(catwalk.TypeOpenAICompat), gjson.GetBytes(raw, "providers.empty-provider.type").String())
}

func TestConfigureCustomProvider_RequiresIDAndBaseURL(t *testing.T) {
	ws, _ := newTestConfigAccessor(t)

	_, err := ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, ConfigureCustomProviderParams{})
	require.Error(t, err)

	_, err = ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, ConfigureCustomProviderParams{ID: "x"})
	require.Error(t, err)
}

// TestConfigureCustomProvider_FullCycle_SurvivesRestartWithEndpointDown
// exercises the whole "Configure Providers → Custom provider…" flow end to
// end: configure against a live discovery endpoint, confirm the provider is
// usable immediately, then simulate a process restart (a fresh config.Load
// against the same on-disk config) with the endpoint unreachable. A custom
// provider must not depend on its endpoint being reachable at every
// startup — its models were already discovered and must have been
// persisted to disk, not just held in memory.
func TestConfigureCustomProvider_FullCycle_SurvivesRestartWithEndpointDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "model-a"}, {"id": "model-b"}]}`))
	}))
	baseURL := server.URL + "/v1"

	ws, configPath := newTestConfigAccessor(t)

	params := ConfigureCustomProviderParams{
		ID:      "restart-test",
		Name:    "Restart Test",
		BaseURL: baseURL,
		Type:    string(catwalk.TypeOpenAICompat),
		APIKey:  "test-key",
	}

	models, err := ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, params)
	require.NoError(t, err)
	require.Len(t, models, 2)

	// (a) provider must be live in memory immediately.
	pc, ok := ws.Config().Providers.Get("restart-test")
	require.True(t, ok, "provider should be live in memory right after configuring")
	require.Equal(t, baseURL, pc.BaseURL)
	require.Len(t, pc.Models, 2)

	// (b) the config file on disk must carry base_url/type and models.
	raw, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, baseURL, gjson.GetBytes(raw, "providers.restart-test.base_url").String())
	require.Equal(t, string(catwalk.TypeOpenAICompat), gjson.GetBytes(raw, "providers.restart-test.type").String())
	require.Len(t, gjson.GetBytes(raw, "providers.restart-test.models").Array(), 2)

	// (c) simulate a restart with the endpoint unreachable: a brand new
	// config.Load, against the same directories, must still find the
	// provider alive with its previously-discovered models rather than
	// dropping it for want of a fresh (and now-impossible) discovery call.
	server.Close()

	store2, err := config.Load(ws.WorkingDir(), ws.store.Config().Options.DataDirectory, false)
	require.NoError(t, err)

	pc2, ok := store2.Config().Providers.Get("restart-test")
	require.True(t, ok, "custom provider must survive a restart even when its endpoint is down")
	require.Equal(t, baseURL, pc2.BaseURL)
	require.Len(t, pc2.Models, 2)
}

// TestConfigureCustomProvider_IDWithDots verifies that a provider ID
// containing '.' — a common shape for domain-style IDs like
// "api.example.com" — round-trips correctly. providers.<id>.<field> is a
// gjson/sjson path, and an unescaped '.' inside <id> splits into nested
// path segments instead of naming one literal "providers" entry, so the
// provider silently vanishes (SetConfigField succeeds, but writes to the
// wrong place, and the in-memory reload can't find it either).
func TestConfigureCustomProvider_IDWithDots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "model-a"}]}`))
	}))
	defer server.Close()

	ws, configPath := newTestConfigAccessor(t)

	params := ConfigureCustomProviderParams{
		ID:      "api.example.com",
		BaseURL: server.URL + "/v1",
		Type:    string(catwalk.TypeOpenAICompat),
	}

	models, err := ConfigureCustomProvider(context.Background(), ws, config.ScopeGlobal, params)
	require.NoError(t, err)
	require.Len(t, models, 1)

	pc, ok := ws.Config().Providers.Get("api.example.com")
	require.True(t, ok, "provider with a dotted ID must be saved under its literal ID")
	require.Equal(t, server.URL+"/v1", pc.BaseURL)
	require.Len(t, pc.Models, 1)

	raw, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, server.URL+"/v1", gjson.GetBytes(raw, `providers.api\.example\.com.base_url`).String())
}
