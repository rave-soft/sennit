package prompt

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
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

// TestBuild_UsesOnlyConfigProvider builds a prompt from a provider that is not
// a ConfigStore, including the context-file path that reads the working
// directory, so a later widening of prompt building's needs beyond the port
// shows up here rather than only at the call sites that still happen to pass
// the concrete store.
func TestBuild_UsesOnlyConfigProvider(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "NOTES.md"), []byte("hello notes"), 0o644))

	provider := fakeConfigProvider{
		cfg: &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Options:   &config.Options{ContextPaths: []string{"NOTES.md"}},
		},
		workingDir: dir,
	}

	p, err := NewPrompt("t", "{{.WorkingDir}}|{{range .ContextFiles}}{{.Content}}{{end}}")
	require.NoError(t, err)

	out, err := p.Build(context.Background(), "p", "m", provider)
	require.NoError(t, err)
	// ToSlash because promptData renders WorkingDir through it (a prompt
	// is read by a model, and one path separator is enough), which the
	// rest of this package's tests already assert the same way. Comparing
	// against the raw t.TempDir() passes on unix by coincidence and fails
	// on Windows, where the two spellings actually differ.
	require.Contains(t, out, filepath.ToSlash(dir))
	require.Contains(t, out, "hello notes")
}
