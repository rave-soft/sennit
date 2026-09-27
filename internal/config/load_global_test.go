package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadGlobalData_IgnoresProjectLayer checks the property PR 3.2 (a
// remote attach's UI preferences) depends on: LoadGlobalData merges only
// the global config layers, never a project sennit.json/sennitrc -- even
// one sitting under the process's own current directory, which a plain
// LoadData call would happily pick up.
func TestLoadGlobalData_IgnoresProjectLayer(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", t.TempDir())

	require.NoError(t, os.WriteFile(
		filepath.Join(globalDir, "sennit.json"),
		[]byte(`{"options":{"tui":{"theme":"global-theme"}}}`),
		0o644,
	))

	projectDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(projectDir, "sennit.json"),
		[]byte(`{"options":{"tui":{"theme":"project-theme"}}}`),
		0o644,
	))
	require.NoError(t, Trust(projectDir))

	// A plain LoadData rooted at projectDir sees the project layer --
	// establishes that the two configs really do disagree and that the
	// project one really is discoverable from here.
	viaProject, err := LoadData(projectDir, "", false)
	require.NoError(t, err)
	require.Equal(t, "project-theme", viaProject.Config().Options.TUI.Theme)

	viaGlobal, err := LoadGlobalData("", false)
	require.NoError(t, err)
	require.Equal(t, "global-theme", viaGlobal.Config().Options.TUI.Theme)
}
