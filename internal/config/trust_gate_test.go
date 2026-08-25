package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_ProjectTrustGate(t *testing.T) {
	globalDir := t.TempDir()
	globalDataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDataDir)
	project := t.TempDir()
	sideEffect := filepath.Join(t.TempDir(), "project-shell-ran")
	require.NoError(t, os.MkdirAll(filepath.Dir(GlobalConfig()), 0o700))
	require.NoError(t, os.WriteFile(shellConfigSibling(GlobalConfig()), []byte("option debug true\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(project, "sennit.json"), []byte(`{"env":{"SENNIT_PROJECT_JSON":"json"},"options":{"notifications":"bell"}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(project, "sennitrc"), []byte("touch "+sideEffect+"\noption notifications osc\n"), 0o600))

	store, err := LoadData(project, "", false)
	require.NoError(t, err)
	require.True(t, store.Config().Options.Debug)
	require.Empty(t, store.Config().Env)
	_, exists := os.LookupEnv("SENNIT_PROJECT_JSON")
	require.False(t, exists)
	_, err = os.Stat(sideEffect)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, Trust(project))
	store, err = LoadData(project, "", false)
	require.NoError(t, err)
	require.True(t, store.Config().Options.Debug)
	require.Equal(t, "json", store.Config().Env["SENNIT_PROJECT_JSON"])
	require.Equal(t, "osc", store.Config().Options.Notifications)
	_, err = os.Stat(sideEffect)
	require.NoError(t, err)
}

func TestLoad_UntrustedProjectConfigDiagnosticUsesDataDirectory(t *testing.T) {
	t.Setenv("SENNIT_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("SENNIT_GLOBAL_DATA", t.TempDir())
	project := t.TempDir()

	for _, dataDir := range []string{"", t.TempDir()} {
		t.Run(dataDir, func(t *testing.T) {
			workspaceDir := dataDir
			if workspaceDir == "" {
				workspaceDir = filepath.Join(project, defaultDataDirectory)
			}
			require.NoError(t, os.MkdirAll(workspaceDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, appName+".json"), []byte(`{"env":{"SENNIT_PROJECT_WORKSPACE":"enabled"}}`), 0o600))

			store, err := LoadData(project, dataDir, false)
			require.NoError(t, err)
			require.Empty(t, store.Config().Env)
			require.Contains(t, store.Config().Problems, Problem{
				Severity: SeverityWarn,
				Area:     AreaEnvironment,
				Subject:  project,
				Message:  "project configuration is disabled because this project is not trusted",
				Hint:     "Review the project, then restart with --trust-project to enable its configuration.",
			})
		})
	}
}
