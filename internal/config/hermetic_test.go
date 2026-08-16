package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/stretchr/testify/require"
)

// hermeticGlobalDirs points the global config and data directories at fresh
// temporary directories and verifies the redirect actually took.
//
// The verification is the point. These variables are named after the brand
// prefix, and when the product was renamed the tests kept setting the old
// BRAID_* names: the redirect silently stopped working, GlobalConfigData()
// resolved to the developer's real ~/.local/share/sennit/sennit.json, and
// the tests seeded, migrated and overwrote it. The only symptom was a
// model-count assertion failing on whatever the developer happened to have
// configured. Resolving the paths here turns that back into an immediate,
// legible failure instead of a wrong answer plus collateral damage.
func hermeticGlobalDirs(tb testing.TB, globalDir, dataDir string) {
	tb.Helper()

	tb.Setenv(brand.EnvPrefix+"GLOBAL_CONFIG", globalDir)
	tb.Setenv(brand.EnvPrefix+"GLOBAL_DATA", dataDir)

	requireUnder(tb, dataDir, GlobalConfigData())
	requireUnder(tb, globalDir, GlobalConfig())
}

// requireUnder fails the test unless path resolves inside dir.
func requireUnder(tb testing.TB, dir, path string) {
	tb.Helper()

	rel, err := filepath.Rel(dir, path)
	require.NoError(tb, err)
	require.False(tb, strings.HasPrefix(rel, ".."),
		"%s escaped its temporary directory %s - the environment override did not take, "+
			"and this test would read and overwrite the real user config", path, dir)
}
