package lsp

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// fakeConfigProvider implements ConfigProvider with nothing but the three
// values the port names. It exists so the port is proven sufficient by a
// caller that has no ConfigStore at all, not merely declared sufficient by
// the compile-time assertion next to it.
type fakeConfigProvider struct {
	cfg        *config.Config
	workingDir string
}

func (f fakeConfigProvider) Config() *config.Config            { return f.cfg }
func (f fakeConfigProvider) Resolver() config.VariableResolver { return nil }
func (f fakeConfigProvider) WorkingDir() string                { return f.workingDir }

// TestManager_UsesOnlyConfigProvider builds a Manager from a provider that is
// not a ConfigStore and drives the config-reading paths, so a later widening
// of Manager's needs beyond the port shows up here rather than only at the
// call sites that still happen to pass the concrete store.
func TestManager_UsesOnlyConfigProvider(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	provider := fakeConfigProvider{
		cfg: &config.Config{
			Options: &config.Options{},
			LSP: map[string]config.LSPConfig{
				"configured": {Command: "configured-lsp", FileTypes: []string{"go"}},
				"turned-off": {Command: "off-lsp", Disabled: true},
			},
		},
		workingDir: workingDir,
	}

	manager := NewManager(provider)

	require.True(t, manager.isUserConfigured("configured"))
	require.False(t, manager.isUserConfigured("turned-off"))
	require.False(t, manager.isUserConfigured("never-mentioned"))
}
