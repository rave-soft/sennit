package workspace

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentModelDoesNotExposeBackendModelTypes(t *testing.T) {
	data, err := os.ReadFile("workspace.go")
	require.NoError(t, err)
	require.NotContains(t, string(data), "CatalogCfg catwalk.Model")
	require.NotContains(t, string(data), "ModelCfg   config.SelectedModel")
}
