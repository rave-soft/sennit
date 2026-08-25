package config

import (
	"cmp"
	"context"
	"maps"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config/migrate"
	"github.com/rave-soft/sennit/internal/config/modelcache"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/fsext"
)

type testRuntimeProcessor struct{}

func (testRuntimeProcessor) Process(ctx context.Context, input RuntimeInput) (RuntimeResult, error) {
	resolver := NewShellVariableResolver(env.New())
	input.Config.ApplyRuntimeEnvironment(resolver)
	knownProviders := Providers(input.Config)
	if input.Initial {
		migrateBloatedModelCache(input.GlobalDataPath, knownProviders)
	}
	known := make(map[string]bool, len(knownProviders))
	for _, provider := range knownProviders {
		id := string(provider.ID)
		known[id] = true
		configured, _ := input.Config.Providers.Get(id)
		apiKey, err := resolver.ResolveValue(cmp.Or(configured.APIKey, provider.APIKey))
		if err != nil || apiKey == "" {
			input.Config.Providers.Del(id)
			continue
		}
		configured.ID = id
		configured.Name = provider.Name
		configured.APIKey = cmp.Or(configured.APIKey, provider.APIKey)
		configured.BaseURL = cmp.Or(configured.BaseURL, provider.APIEndpoint)
		configured.Type = provider.Type
		if len(configured.Models) == 0 {
			configured.Models = provider.Models
		}
		input.Config.Providers.Set(id, configured)
	}

	restore := PushPopEnvOverrides()
	type request struct {
		id  string
		cfg discover.Config
	}
	var requests []request
	for id, provider := range input.Config.Providers.Seq2() {
		if known[id] {
			continue
		}
		provider.ID = id
		provider.Name = cmp.Or(provider.Name, id)
		provider.Type = cmp.Or(provider.Type, catwalk.TypeOpenAICompat)
		if len(provider.Models) == 0 {
			if cached, ok := modelcache.New(input.GlobalDataPath).Load(id); ok {
				provider.Models = cached
				provider.ModelsSource = ModelsSourceCache
			}
		}
		if len(provider.Models) == 0 && (provider.AutoDiscoverModels == nil || *provider.AutoDiscoverModels) {
			baseURL, _ := resolver.ResolveValue(provider.BaseURL)
			apiKey, _ := resolver.ResolveValue(provider.APIKey)
			requests = append(requests, request{id: id, cfg: discover.Config{ID: id, BaseURL: baseURL, APIKey: apiKey, ExistingModels: provider.Models}})
		} else if len(provider.Models) > 0 && provider.ModelsSource == "" {
			provider.ModelsSource = ModelsSourceConfig
		}
		input.Config.Providers.Set(id, provider)
	}
	restore()

	for _, request := range requests {
		models, err := discover.DiscoverModels(ctx, request.cfg, IdentityResolver())
		provider, _ := input.Config.Providers.Get(request.id)
		if err != nil || len(models) == 0 {
			input.Config.Providers.Del(request.id)
			continue
		}
		provider.Models = models
		provider.ModelsSource = ModelsSourceCache
		input.Config.Providers.Set(request.id, provider)
		modelcache.New(input.GlobalDataPath).SaveBestEffort(request.id, models)
	}

	for id, provider := range input.Config.Providers.Seq2() {
		if provider.ExtraHeaders == nil {
			provider.ExtraHeaders = map[string]string{}
		}
		maps.Copy(provider.ExtraHeaders, map[string]string{})
		input.Config.Providers.Set(id, provider)
	}
	return RuntimeResult{KnownProviders: knownProviders, Resolver: resolver}, nil
}

func migrateBloatedModelCache(path string, knownProviders []catwalk.Provider) {
	migrate.BloatedModelCache(path, knownProviders, func(dataPath, providerID string, models []catwalk.Model) error {
		return modelcache.New(dataPath).Save(providerID, models)
	}, fsext.AtomicWriteFile)
}

func loadRuntimeForTest(workingDir, dataDir string, debug bool) (*ConfigStore, error) {
	return LoadWithProcessor(workingDir, dataDir, debug, testRuntimeProcessor{})
}
