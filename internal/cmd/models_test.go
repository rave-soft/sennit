package cmd

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// newRefreshTestCmd builds a minimal cobra.Command carrying the flags that
// refreshCmd.RunE reads (cwd, data-dir, debug), matching what rootCmd
// registers as persistent flags in root.go. Tests invoke RunE directly
// rather than going through rootCmd.Execute() to keep them hermetic.
func newRefreshTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	testCmd := &cobra.Command{Use: "refresh"}
	testCmd.Flags().StringP("cwd", "c", "", "")
	testCmd.Flags().StringP("data-dir", "D", "", "")
	testCmd.Flags().BoolP("debug", "d", false, "")

	var stdout, stderr bytes.Buffer
	testCmd.SetOut(&stdout)
	testCmd.SetErr(&stderr)
	testCmd.SetArgs(nil)
	return testCmd, &stdout, &stderr
}

// setupHermeticConfigEnv points SENNIT_GLOBAL_CONFIG/SENNIT_GLOBAL_DATA at
// fresh temp dirs and seeds the data-dir config file, mirroring the
// pattern used in internal/config/load_test.go.
func setupHermeticConfigEnv(t *testing.T, seed string) string {
	t.Helper()
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	dataConfigPath := config.GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(seed), 0o644))
	return dataConfigPath
}

func modelsHandler(ids ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var b bytes.Buffer
		b.WriteString(`{"data": [`)
		for i, id := range ids {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id": %q, "object": "model"}`, id)
		}
		b.WriteString(`]}`)
		_, _ = w.Write(b.Bytes())
	}
}

func TestModelsRefreshCmd_SingleProvider(t *testing.T) {
	server := httptest.NewServer(modelsHandler("model-a", "model-b"))
	defer server.Close()

	// No seeded "models" — an empty list at load time makes this a
	// cache-sourced provider (auto-discovered against the same server),
	// which `refresh` is allowed to overwrite. A hand-written models list
	// would instead be ModelsSourceConfig and refresh would skip it (see
	// TestModelsRefreshCmd_ExplicitConfigModelsSkipped).
	seed := fmt.Sprintf(`{"providers": {"custom": {"api_key": "key", "base_url": %q}}}`, server.URL+"/v1")
	dataConfigPath := setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	cwd := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, cwd) })
	setCwdFlag(t, testCmd, cwd)

	err := refreshCmd.RunE(testCmd, []string{"custom"})
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "custom:")
	require.Contains(t, stdout.String(), "2 models")

	// Refreshed models must land in the model cache, not
	// providers.custom.models in the data-dir JSON.
	persisted, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(persisted, "providers.custom.models").Exists())

	// A subsequent load must see the refreshed models via the cache
	// without needing the (now closed-by-defer) network endpoint.
	cfg, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)
	pc, ok := cfg.Config().Providers.Get("custom")
	require.True(t, ok)
	require.Len(t, pc.Models, 2)
	var ids []string
	for _, m := range pc.Models {
		ids = append(ids, m.ID)
	}
	require.ElementsMatch(t, []string{"model-a", "model-b"}, ids)
}

func TestModelsRefreshCmd_AllProviders(t *testing.T) {
	serverA := httptest.NewServer(modelsHandler("a-model"))
	defer serverA.Close()
	serverB := httptest.NewServer(modelsHandler("b-model-1", "b-model-2"))
	defer serverB.Close()

	// No seeded "models" — see TestModelsRefreshCmd_SingleProvider for why.
	seed := fmt.Sprintf(`{"providers": {
		"custom-a": {"api_key": "key", "base_url": %q},
		"custom-b": {"api_key": "key", "base_url": %q}
	}}`, serverA.URL+"/v1", serverB.URL+"/v1")
	dataConfigPath := setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	setCwdFlag(t, testCmd, t.TempDir())

	err := refreshCmd.RunE(testCmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "custom-a:")
	require.Contains(t, stdout.String(), "custom-b:")

	persisted, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(persisted, "providers.custom-a.models").Exists())
	require.False(t, gjson.GetBytes(persisted, "providers.custom-b.models").Exists())

	cfg, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)
	pcA, ok := cfg.Config().Providers.Get("custom-a")
	require.True(t, ok)
	require.Len(t, pcA.Models, 1)
	pcB, ok := cfg.Config().Providers.Get("custom-b")
	require.True(t, ok)
	require.Len(t, pcB.Models, 2)
}

func TestModelsRefreshCmd_UnreachableEndpointLeavesDiskUntouched(t *testing.T) {
	// discover_models: true is required here: the seeded "models" entry
	// makes this provider ModelsSourceConfig (explicit), and refresh
	// otherwise skips explicit-config providers without ever reaching the
	// network (see TestModelsRefreshCmd_ExplicitConfigModelsSkipped) —
	// true is the documented escape hatch that still lets a forced refresh
	// run, and fail, against the unreachable endpoint this test wants to
	// exercise. A 3-entry-or-fewer "models" array is also below
	// modelCacheMigrationThreshold, so migrateBloatedModelCache leaves it
	// in place on disk either way.
	seed := `{"providers": {"custom": {"api_key": "key", "base_url": "http://127.0.0.1:1/v1", "discover_models": true, "models": [{"id": "seed", "name": "seed"}]}}}`
	dataConfigPath := setupHermeticConfigEnv(t, seed)

	// A plain load leaves the seeded models in place (see the threshold
	// note above); do it once up front so "before" reflects the steady
	// state a failed refresh must preserve.
	_, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)
	before, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)

	testCmd, _, stderr := newRefreshTestCmd(t)
	setCwdFlag(t, testCmd, t.TempDir())

	err = refreshCmd.RunE(testCmd, []string{"custom"})
	require.Error(t, err)
	require.Contains(t, stderr.String(), "custom: refresh failed")

	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "failed refresh must not write to disk")
}

func TestModelsRefreshCmd_UnknownProviderID(t *testing.T) {
	seed := `{"providers": {}}`
	dataConfigPath := setupHermeticConfigEnv(t, seed)
	before, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)

	testCmd, _, _ := newRefreshTestCmd(t)
	setCwdFlag(t, testCmd, t.TempDir())

	err = refreshCmd.RunE(testCmd, []string{"does-not-exist"})
	require.Error(t, err)

	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestModelsRefreshCmd_KnownCatalogProviderRejected(t *testing.T) {
	seed := `{"providers": {}}`
	setupHermeticConfigEnv(t, seed)

	testCmd, _, _ := newRefreshTestCmd(t)
	setCwdFlag(t, testCmd, t.TempDir())

	err := refreshCmd.RunE(testCmd, []string{"openai"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "known catalog provider")
}

// TestModelsRefreshCmd_ExplicitConfigModelsSkipped guards against the
// omniroute/qwen36-local incident: a hand-written models list must never be
// silently replaced by whatever a refresh discovers, cache included.
func TestModelsRefreshCmd_ExplicitConfigModelsSkipped(t *testing.T) {
	server := httptest.NewServer(modelsHandler("should-never-be-fetched"))
	defer server.Close()

	seed := fmt.Sprintf(`{"providers": {"custom": {"api_key": "key", "base_url": %q, "models": [
		{"id": "manual-model-a", "name": "manual-model-a"},
		{"id": "manual-model-b", "name": "manual-model-b"}
	]}}}`, server.URL+"/v1")
	dataConfigPath := setupHermeticConfigEnv(t, seed)
	before, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	setCwdFlag(t, testCmd, t.TempDir())

	err = refreshCmd.RunE(testCmd, []string{"custom"})
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "models are explicitly defined in config; refresh skipped")

	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "explicit config models must not be touched on disk")

	cfg, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)
	pc, ok := cfg.Config().Providers.Get("custom")
	require.True(t, ok)
	var ids []string
	for _, m := range pc.Models {
		ids = append(ids, m.ID)
	}
	require.ElementsMatch(t, []string{"manual-model-a", "manual-model-b"}, ids,
		"manual models must survive untouched, never replaced by the server's response")
}

// TestModelsRefreshCmd_DiscoveryDisabledRejected checks the other half of
// the same incident: llama.cpp-style endpoints that echo a gguf path as
// the model "id" are exactly what discover_models: false exists to opt
// out of, and refresh must respect that rather than overwriting the cache
// with junk.
func TestModelsRefreshCmd_DiscoveryDisabledRejected(t *testing.T) {
	seed := `{"providers": {"custom": {"api_key": "key", "base_url": "http://127.0.0.1:1/v1", "discover_models": false, "models": [{"id": "manual-model", "name": "manual-model"}]}}}`
	setupHermeticConfigEnv(t, seed)

	testCmd, _, stderr := newRefreshTestCmd(t)
	setCwdFlag(t, testCmd, t.TempDir())

	err := refreshCmd.RunE(testCmd, []string{"custom"})
	require.Error(t, err)
	require.Contains(t, stderr.String(), "discovery disabled for custom (discover_models: false)")
}
