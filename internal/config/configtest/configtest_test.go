package configtest_test

import (
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/stretchr/testify/require"
)

func TestNewStoreExplicitGlobalPathOverridesDefaultAndCanBeShared(t *testing.T) {
	t.Parallel()

	sharedPath := filepath.Join(t.TempDir(), "shared.json")
	first := configtest.NewStore(t, &config.Config{}, configtest.WithGlobalDataPath(sharedPath))
	second := configtest.NewStore(t, &config.Config{}, configtest.WithGlobalDataPath(sharedPath))

	firstPath, err := first.ConfigPath(config.ScopeGlobal)
	require.NoError(t, err)
	secondPath, err := second.ConfigPath(config.ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, sharedPath, firstPath)
	require.Equal(t, sharedPath, secondPath)
}
