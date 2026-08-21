package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/projects"
	"github.com/stretchr/testify/require"
)

func TestProjectsEmpty(t *testing.T) {
	// Use a temp directory for projects.json
	tmpDir := t.TempDir()
	// The package's TestMain isolates the profile by setting
	// SENNIT_GLOBAL_DATA process-wide, and GlobalConfigData checks that
	// before XDG_DATA_HOME — so overriding XDG_DATA_HOME here would have no
	// effect and both projects tests would share one projects.json,
	// making them order-dependent under -count>1.
	t.Setenv(brand.EnvPrefix+"GLOBAL_DATA", tmpDir)

	var b bytes.Buffer
	projectsCmd.SetOut(&b)
	projectsCmd.SetErr(&b)
	projectsCmd.SetIn(bytes.NewReader(nil))
	err := projectsCmd.RunE(projectsCmd, nil)
	require.NoError(t, err)
	require.Equal(t, "No projects tracked yet.\n", b.String())
}

func TestProjectsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	// The package's TestMain isolates the profile by setting
	// SENNIT_GLOBAL_DATA process-wide, and GlobalConfigData checks that
	// before XDG_DATA_HOME — so overriding XDG_DATA_HOME here would have no
	// effect and both projects tests would share one projects.json,
	// making them order-dependent under -count>1.
	t.Setenv(brand.EnvPrefix+"GLOBAL_DATA", tmpDir)

	// Register a project
	err := projects.Register("/test/project", "/test/project/.sennit")
	require.NoError(t, err)

	var b bytes.Buffer
	projectsCmd.SetOut(&b)
	projectsCmd.SetErr(&b)
	projectsCmd.SetIn(bytes.NewReader(nil))

	// Set the --json flag
	require.NoError(t, projectsCmd.Flags().Set("json", "true"))
	defer func() { _ = projectsCmd.Flags().Set("json", "false") }()

	err = projectsCmd.RunE(projectsCmd, nil)
	require.NoError(t, err)

	// Parse the JSON output
	var result struct {
		Projects []projects.Project `json:"projects"`
	}
	err = json.Unmarshal(b.Bytes(), &result)
	require.NoError(t, err)

	require.Len(t, result.Projects, 1)
	require.Equal(t, "/test/project", result.Projects[0].Path)
	require.Equal(t, "/test/project/.sennit", result.Projects[0].DataDir)
}
