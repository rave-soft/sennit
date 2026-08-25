package providerload

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/migrate"
	"github.com/rave-soft/sennit/internal/config/modelcache"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/oauth/codex"
)

type Loader struct{}

func New() *Loader { return &Loader{} }

func (l *Loader) Process(ctx context.Context, input config.RuntimeInput) (config.RuntimeResult, error) {
	cfg := input.Config
	knownProviders := input.KnownProviders
	if knownProviders == nil {
		knownProviders = providers(cfg)
	}
	if input.Initial {
		migrate.BloatedModelCache(input.GlobalDataPath, knownProviders, func(path, providerID string, models []catwalk.Model) error {
			return modelcache.New(path).Save(providerID, models)
		}, fsext.AtomicWriteFile)
	}
	environment := env.New()
	resolver := config.NewShellVariableResolver(environment)
	cfg.ApplyRuntimeEnvironment(resolver)
	if cfg.Options.DisableDefaultProviders {
		knownProviders = nil
	}
	restore := config.PushPopEnvOverrides()
	knownNames, err := l.mergeCatalogProviders(cfg, input.Store, environment, resolver, knownProviders, input.CredentialsHome, input.Stat)
	if err != nil {
		restore()
		return config.RuntimeResult{}, err
	}
	resolveCustomProviderModels(cfg.Providers, knownNames, input.GlobalDataPath)
	requests, results := resolveDiscoveryRequests(cfg.Providers, knownNames, resolver)
	restore()
	maps.Copy(results, runDiscoveryRequests(ctx, requests))
	restore = config.PushPopEnvOverrides()
	defer restore()
	if err := l.validateCustomProviders(cfg, knownNames, resolver, results, input.GlobalDataPath); err != nil {
		return config.RuntimeResult{}, err
	}
	if cfg.Providers.Len() == 0 && cfg.Options.DisableDefaultProviders {
		return config.RuntimeResult{}, fmt.Errorf("default providers are disabled and no custom providers are configured")
	}
	return config.RuntimeResult{KnownProviders: knownProviders, Resolver: resolver}, nil
}

func providers(cfg *config.Config) []catwalk.Provider {
	if cfg.Options.DisableDefaultProviders {
		return nil
	}
	return append(embedded.GetAll(), config.CodexProvider())
}

func (l *Loader) mergeCatalogProviders(cfg *config.Config, store config.RuntimeStore, environment env.Env, resolver config.VariableResolver, known []catwalk.Provider, credentialsHome string, stat func(string) (os.FileInfo, error)) (map[string]bool, error) {
	if credentialsHome == "" {
		credentialsHome = home.Dir()
	}
	if stat == nil {
		stat = os.Stat
	}
	knownNames := make(map[string]bool)
	for _, provider := range known {
		id := string(provider.ID)
		knownNames[id] = true
		userConfig, exists := cfg.Providers.Get(id)
		provider = mergeProviderOverride(provider, userConfig, exists)
		headers := map[string]string{}
		maps.Copy(headers, provider.DefaultHeaders)
		maps.Copy(headers, userConfig.ExtraHeaders)
		if err := config.ResolveProviderHeaders(headers, resolver, id); err != nil {
			return nil, err
		}
		prepared := userConfig
		prepared.ID = id
		prepared.Name = provider.Name
		prepared.BaseURL = provider.APIEndpoint
		prepared.APIKey = provider.APIKey
		prepared.APIKeyTemplate = provider.APIKey
		prepared.Type = provider.Type
		prepared.Models = provider.Models
		prepared.ExtraHeaders = headers
		prepared.ProxyURL = config.ResolveOptionalProviderProxy(userConfig.ProxyURL, resolver, id)
		if prepared.ExtraParams == nil {
			prepared.ExtraParams = make(map[string]string)
		}
		if provider.ID == catwalk.InferenceProviderAnthropic && userConfig.OAuthToken != nil {
			cfg.Providers.Del(id)
			if store != nil {
				store.RemoveRuntimeConfigField(config.ScopeGlobal, "providers.anthropic")
			}
			continue
		}
		if provider.ID == catwalk.InferenceProviderCopilot && userConfig.OAuthToken != nil {
			prepared.SetupGitHubCopilot()
		}
		if id == codex.ProviderID {
			prepared.SetupCodex()
		}
		if !l.applyCredentials(cfg, environment, resolver, provider, exists, &prepared, credentialsHome, stat) {
			continue
		}
		cfg.Providers.Set(id, prepared)
	}
	return knownNames, nil
}

func mergeProviderOverride(provider catwalk.Provider, userConfig config.ProviderConfig, exists bool) catwalk.Provider {
	if !exists {
		return provider
	}
	if userConfig.BaseURL != "" {
		provider.APIEndpoint = userConfig.BaseURL
	}
	if userConfig.APIKey != "" {
		provider.APIKey = userConfig.APIKey
	}
	if len(userConfig.Models) == 0 {
		return provider
	}
	models := make([]catwalk.Model, 0, len(userConfig.Models)+len(provider.Models))
	seen := make(map[string]bool)
	for _, model := range append(slices.Clone(userConfig.Models), provider.Models...) {
		if seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		if model.Name == "" {
			model.Name = model.ID
		}
		models = append(models, model)
	}
	provider.Models = models
	return provider
}

func (l *Loader) applyCredentials(cfg *config.Config, environment env.Env, resolver config.VariableResolver, provider catwalk.Provider, exists bool, prepared *config.ProviderConfig, credentialsHome string, stat func(string) (os.FileInfo, error)) bool {
	id := string(provider.ID)
	switch provider.ID {
	case catwalk.InferenceProviderVertexAI:
		project, location := environment.Get("VERTEXAI_PROJECT"), environment.Get("VERTEXAI_LOCATION")
		if project == "" || location == "" {
			if exists {
				l.dropProvider(cfg, id, "VERTEXAI_PROJECT/VERTEXAI_LOCATION not set", "static check only; set both env vars and reload")
			}
			return false
		}
		prepared.ExtraParams["project"], prepared.ExtraParams["location"] = project, location
	case catwalk.InferenceProviderAzure:
		endpoint, err := resolver.ResolveValue(provider.APIEndpoint)
		if err != nil || endpoint == "" {
			if exists {
				l.dropProvider(cfg, id, "missing API endpoint", "static check only; set base_url and reload")
			}
			return false
		}
		prepared.BaseURL = endpoint
		prepared.ExtraParams["apiVersion"] = environment.Get("AZURE_OPENAI_API_VERSION")
	case catwalk.InferenceProviderBedrock, catwalk.InferenceProviderBedrockEurope:
		if provider.APIKey == "" && !hasAWSCredentials(environment, credentialsHome, stat) {
			if exists {
				l.dropProvider(cfg, id, "no api_key and no AWS credentials found", "static check only; set api_key or AWS credentials and reload")
			}
			return false
		}
	default:
		value, err := resolver.ResolveValue(provider.APIKey)
		if value == "" || err != nil {
			if exists {
				l.dropProvider(cfg, id, "missing api_key", "static check only; set api_key and reload")
			}
			return false
		}
	}
	return true
}

func hasAWSCredentials(environment env.Env, homeDir string, stat func(string) (os.FileInfo, error)) bool {
	if environment.Get("AWS_BEARER_TOKEN_BEDROCK") != "" || environment.Get("AWS_PROFILE") != "" || environment.Get("AWS_DEFAULT_PROFILE") != "" || environment.Get("AWS_REGION") != "" || environment.Get("AWS_DEFAULT_REGION") != "" || environment.Get("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" || environment.Get("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return true
	}
	if environment.Get("AWS_ACCESS_KEY_ID") != "" && environment.Get("AWS_SECRET_ACCESS_KEY") != "" {
		return true
	}
	for _, name := range []string{".aws/credentials", ".aws/login"} {
		if _, err := stat(filepath.Join(homeDir, name)); err == nil {
			return true
		}
	}
	return false
}

func (l *Loader) dropProvider(cfg *config.Config, id, reason, hint string) {
	slog.Warn("Skipping provider", "provider", id, "reason", reason)
	cfg.AddRuntimeProblem(providerDropProblem(id, reason, hint))
	cfg.Providers.Del(id)
}

func providerProblem(id, message, hint string) config.Problem {
	return config.Problem{Severity: config.SeverityWarn, Area: config.AreaProvider, Subject: id, Message: message, Hint: hint}
}

func providerDropProblem(id, reason, hint string) config.Problem {
	return providerProblem(id, fmt.Sprintf("provider %s dropped: %s", id, reason), hint)
}

func resolveProviderHeaders(headers map[string]string, resolver config.VariableResolver, providerID string) error {
	return config.ResolveProviderHeaders(headers, resolver, providerID)
}

func resolveOptionalProxy(proxyURL string, resolver config.VariableResolver, providerID string) string {
	return config.ResolveOptionalProviderProxy(proxyURL, resolver, providerID)
}

var _ = cmp.Or[string]
