package config

import (
	"cmp"
	"context"
	"maps"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/rave-soft/sennit/internal/config/migrate"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/modelcache"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/providers/state"
)

type testRuntimeProcessor struct{}

func (testRuntimeProcessor) CompileProvider(configured ProviderConfig, resolver VariableResolver) (state.Provider, error) {
	apiKey, err := resolver.ResolveValue(configured.APIKey)
	if err != nil {
		return state.Provider{}, err
	}
	baseURL, err := resolver.ResolveValue(configured.BaseURL)
	if err != nil {
		return state.Provider{}, err
	}
	provider := state.Provider{ID: configured.ID, Name: configured.Name, Type: configured.Type, APIKey: apiKey, APIKeyTemplate: configured.APIKey, OAuthToken: configured.OAuthToken, BaseURL: baseURL, ProxyURL: configured.ProxyURL, ConfiguredProxyURL: configured.ProxyURL, Account: configured.Account, ExtraHeaders: maps.Clone(configured.ExtraHeaders), Models: configured.Models}
	return (testRuntimeProcessor{}).ApplyProviderCredentials(provider)
}

func (testRuntimeProcessor) ApplyProviderCredentials(provider state.Provider) (state.Provider, error) {
	if provider.ExtraHeaders == nil {
		provider.ExtraHeaders = make(map[string]string)
	}
	switch provider.ID {
	case string(catwalk.InferenceProviderCopilot):
		maps.Copy(provider.ExtraHeaders, copilot.Headers())
	case codex.ProviderID:
		accountID := codex.AccountID(provider.APIKey)
		if accountID == "" && provider.OAuthToken != nil {
			accountID = codex.AccountID(provider.OAuthToken.AccessToken)
		}
		if accountID == "" {
			delete(provider.ExtraHeaders, codex.AccountIDHeader)
		}
		maps.Copy(provider.ExtraHeaders, codex.Headers(accountID))
	}
	return provider, nil
}

func (testRuntimeProcessor) Process(ctx context.Context, input RuntimeInput) (RuntimeResult, error) {
	resolver := NewShellVariableResolver(input.Config.RuntimeEnvironment())
	knownProviders := input.KnownProviders
	if knownProviders == nil && !input.Config.Options.DisableDefaultProviders {
		knownProviders = append(embedded.GetAll(), catwalk.Provider{ID: catwalk.InferenceProvider(codex.ProviderID), Name: codex.ProviderName, APIEndpoint: codex.APIBaseURL, Type: catwalk.TypeOpenAI})
	}
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
	runtimeProviders := csync.NewMap[string, state.Provider]()
	for id, configured := range input.Config.Providers.Seq2() {
		apiKey, _ := resolver.ResolveValue(configured.APIKey)
		baseURL, _ := resolver.ResolveValue(configured.BaseURL)
		provider := state.Provider{ID: id, Name: configured.Name, Type: configured.Type, APIKey: apiKey, APIKeyTemplate: configured.APIKey, OAuthToken: configured.OAuthToken, BaseURL: baseURL, ProxyURL: configured.ProxyURL, ConfiguredProxyURL: configured.ProxyURL, Account: configured.Account, ExtraHeaders: maps.Clone(configured.ExtraHeaders), Models: configured.Models}
		prepared, _ := (testRuntimeProcessor{}).ApplyProviderCredentials(provider)
		runtimeProviders.Set(id, prepared)
	}
	return RuntimeResult{KnownProviders: knownProviders, RuntimeProviders: runtimeProviders, Resolver: resolver}, nil
}

func migrateBloatedModelCache(path string, knownProviders []catwalk.Provider) {
	migrate.BloatedModelCache(path, knownProviders, func(dataPath, providerID string, models []catwalk.Model) error {
		return modelcache.New(dataPath).Save(providerID, models)
	}, fsext.AtomicWriteFile)
}

func loadRuntimeForTest(workingDir, dataDir string, debug bool) (*ConfigStore, error) {
	return LoadWithProcessor(workingDir, dataDir, debug, testRuntimeProcessor{})
}
