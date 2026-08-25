package configruntime

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

type countingProcessor struct {
	calls atomic.Int64
}

func (processor *countingProcessor) Process(_ context.Context, input config.RuntimeInput) (config.RuntimeResult, error) {
	processor.calls.Add(1)
	input.Config.Providers = csync.NewMap(map[string]config.ProviderConfig{
		"mock": {ID: "mock", Name: "Mock", Type: catwalk.TypeOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", APIKey: "test", Models: []catwalk.Model{{ID: "mock-model"}}},
	})
	return config.RuntimeResult{KnownProviders: []catwalk.Provider{{ID: "mock", Models: []catwalk.Model{{ID: "mock-model"}}}}, Resolver: config.IdentityResolver()}, nil
}

func TestProcessorRetainedAcrossReload(t *testing.T) {
	global := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_DATA", global)
	require.NoError(t, os.WriteFile(filepath.Join(global, "sennit.json"), []byte(`{"model":{"provider":"mock","model":"mock-model"}}`), 0o600))
	processor := &countingProcessor{}
	store, err := config.LoadWithProcessor(t.TempDir(), t.TempDir(), false, processor)
	require.NoError(t, err)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.Equal(t, int64(2), processor.calls.Load())
}
