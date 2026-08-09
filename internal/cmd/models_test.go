package cmd

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/braid/internal/config"
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

// setupHermeticConfigEnv points BRAID_GLOBAL_CONFIG/BRAID_GLOBAL_DATA at
// fresh temp dirs and seeds the data-dir config file, mirroring the
// pattern used in internal/config/load_test.go.
func setupHermeticConfigEnv(t *testing.T, seed string) string {
	t.Helper()
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)

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

	seed := fmt.Sprintf(`{"providers": {"custom": {"api_key": "key", "base_url": %q, "models": [{"id": "old-model", "name": "old-model"}]}}}`, server.URL+"/v1")
	dataConfigPath := setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	err := refreshCmd.RunE(testCmd, []string{"custom"})
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "custom:")
	require.Contains(t, stdout.String(), "2 models")

	persisted, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	models := gjson.GetBytes(persisted, "providers.custom.models")
	require.True(t, models.Exists())
	require.Len(t, models.Array(), 2)
	var ids []string
	for _, m := range models.Array() {
		ids = append(ids, m.Get("id").String())
	}
	require.ElementsMatch(t, []string{"model-a", "model-b"}, ids)
}

func TestModelsRefreshCmd_AllProviders(t *testing.T) {
	serverA := httptest.NewServer(modelsHandler("a-model"))
	defer serverA.Close()
	serverB := httptest.NewServer(modelsHandler("b-model-1", "b-model-2"))
	defer serverB.Close()

	seed := fmt.Sprintf(`{"providers": {
		"custom-a": {"api_key": "key", "base_url": %q, "models": [{"id": "seed", "name": "seed"}]},
		"custom-b": {"api_key": "key", "base_url": %q, "models": [{"id": "seed", "name": "seed"}]}
	}}`, serverA.URL+"/v1", serverB.URL+"/v1")
	dataConfigPath := setupHermeticConfigEnv(t, seed)

	testCmd, stdout, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	err := refreshCmd.RunE(testCmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "custom-a:")
	require.Contains(t, stdout.String(), "custom-b:")

	persisted, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(persisted, "providers.custom-a.models").Array(), 1)
	require.Len(t, gjson.GetBytes(persisted, "providers.custom-b.models").Array(), 2)
}

func TestModelsRefreshCmd_UnreachableEndpointLeavesDiskUntouched(t *testing.T) {
	seed := `{"providers": {"custom": {"api_key": "key", "base_url": "http://127.0.0.1:1/v1", "models": [{"id": "seed", "name": "seed"}]}}}`
	dataConfigPath := setupHermeticConfigEnv(t, seed)

	before, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)

	testCmd, _, stderr := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

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
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

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
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	err := refreshCmd.RunE(testCmd, []string{"openai"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "known catalog provider")
}
