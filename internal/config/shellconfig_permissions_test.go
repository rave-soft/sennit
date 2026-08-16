package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// loadSennitSh writes a sennit.sh into an isolated project and loads it through
// the real config pipeline (discovery -> shell execution -> merge -> typed
// Config). Asserting on the resulting *config.Config is a black-box test of
// what a shell config command actually produces, and it stays valid across
// internal changes to how config is assembled.
func loadSennitSh(t *testing.T, script string) *config.ConfigStore {
	t.Helper()
	store, err := loadSennitShErr(t, script)
	require.NoError(t, err)
	return store
}

// loadSennitShErr is loadSennitSh without asserting success, for cases that are
// expected to fail at load time.
func loadSennitShErr(t *testing.T, script string) (*config.ConfigStore, error) {
	t.Helper()
	isolateSennitHome(t)

	workDir := t.TempDir()
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "sennitrc"), []byte(script), 0o644))

	return config.Load(workDir, dataDir, false)
}

// loadSennitShGlobal is loadSennitSh for scripts that configure providers or
// select a model. Those settings are global-only — a `provider`/`model` block
// coming from a project config is stripped before the merge — so the script
// has to live in the global sennitrc to have any effect.
func loadSennitShGlobal(t *testing.T, script string) *config.ConfigStore {
	t.Helper()
	globalDir := isolateSennitHome(t)

	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "sennitrc"), []byte(script), 0o644))

	store, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	return store
}

// isolateSennitHome points every global config location at a fresh temporary
// home so only the script under test contributes, and returns the global
// config directory. No t.Parallel() in callers: this sets env vars.
func isolateSennitHome(t *testing.T) string {
	t.Helper()
	isolated := t.TempDir()
	globalDir := filepath.Join(isolated, ".config", "sennit")
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "sennit"))
	return globalDir
}

func TestShellConfigPermissionsAllow(t *testing.T) {
	store := loadSennitSh(t, `permissions allow bash read`)

	require.NotNil(t, store.Config().Permissions)
	require.ElementsMatch(t, []string{"bash", "read"}, store.Config().Permissions.AllowedTools)
}

func TestShellConfigPermissionsAccumulateAndDedup(t *testing.T) {
	store := loadSennitSh(t, `permissions allow bash
permissions allow read
permissions allow bash`)

	require.Equal(t, []string{"bash", "read"}, store.Config().Permissions.AllowedTools)
}

func TestShellConfigPermissionsLegacyFlagFails(t *testing.T) {
	_, err := loadSennitShErr(t, `permissions --allow bash`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown subcommand")
}

func TestShellConfigPermissionsAllowRequiresTool(t *testing.T) {
	_, err := loadSennitShErr(t, `permissions allow`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "usage: permissions allow")
}

// deny hides tools from the agent by writing options.disabled_tools.
func TestShellConfigPermissionsDeny(t *testing.T) {
	store := loadSennitSh(t, `permissions deny bash download
permissions deny bash`)

	require.Equal(t, []string{"bash", "download"}, store.Config().Options.DisabledTools)
}

// When a tool is both allowed and denied, deny wins: the tool lands in
// disabled_tools which removes it from the agent entirely, regardless of
// its presence in the allow-list.
func TestShellConfigPermissionsDenyWinsOverAllow(t *testing.T) {
	store := loadSennitSh(t, `permissions allow bash read
permissions deny bash`)

	require.ElementsMatch(t, []string{"bash", "read"}, store.Config().Permissions.AllowedTools)
	require.Equal(t, []string{"bash"}, store.Config().Options.DisabledTools)

	// SetupAgents resolves the effective tool set; denied tools are excluded.
	cfg := store.Config()
	cfg.SetupAgents()
	require.NotContains(t, cfg.Agents[config.AgentCoder].AllowedTools, "bash")
	require.Contains(t, cfg.Agents[config.AgentCoder].AllowedTools, "read")
}
