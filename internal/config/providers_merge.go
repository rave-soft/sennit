package config

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/tidwall/sjson"
)

// configureProviders is a thin wrapper around three phases that used to be
// inlined here as one ~290-line function: mergeCatalogProviders (merge the
// embedded catalog with user overrides, applying vendor special cases),
// discoverCustomProviderModels (pure HTTP model discovery for custom
// providers), and validateCustomProviders (apply discovery results and drop
// invalid custom providers). Splitting the HTTP-performing phase out lets
// callers (Load, reloadFromDisk) run it before taking writeMu — see those
// call sites for why that matters.
func (c *Config) configureProviders(ctx context.Context, store *ConfigStore, env env.Env, resolver VariableResolver, knownProviders []catwalk.Provider) error {
	restore := PushPopBraidEnv()
	defer restore()

	// When disable_default_providers is enabled, skip all default/embedded
	// providers entirely. Users must fully specify any providers they want.
	// We skip to the custom provider validation loop which handles all
	// user-configured providers uniformly.
	if c.Options.DisableDefaultProviders {
		knownProviders = nil
	}

	knownProviderNames, actions, err := c.mergeCatalogProviders(env, resolver, knownProviders)
	if err != nil {
		return err
	}

	// Mark ModelsSource and fill in Models for custom providers from the
	// cache before discovery runs, so a restart doesn't re-trigger an HTTP
	// round trip just because the JSON config no longer carries the
	// discovered list (see resolveCustomProviderModels).
	resolveCustomProviderModels(c.Providers, knownProviderNames, store.globalDataPath)

	discoveryResults := discoverCustomProviderModels(ctx, c.Providers, knownProviderNames, resolver)

	err = c.validateCustomProviders(knownProviderNames, resolver, discoveryResults, store.globalDataPath)
	if err != nil {
		return err
	}

	// Disk writes collected while merging (e.g. dropping a stale Claude
	// Code OAuth provider) are applied here via a direct write rather
	// than through RemoveConfigField/SetConfigFields: their autoReload
	// trigger would be pointless mid-load/reload (a fresh config is about
	// to be swapped in anyway) and, prior to this refactor, was the thing
	// that forced this whole call to run under writeMu just so the
	// re-entrant autoReload could TryLock-and-noop instead of deadlocking.
	applyPendingDiskActions(store, actions)

	if c.Providers.Len() == 0 && c.Options.DisableDefaultProviders {
		return fmt.Errorf("default providers are disabled and there are no custom providers are configured")
	}

	return nil
}

// pendingDiskAction records a best-effort config-file mutation discovered
// while merging or validating providers, to be applied by the caller after
// that phase completes. See configureProviders for why this is not just a
// direct store.RemoveConfigField/SetConfigFields call.
//
// A nil fields means "delete key" (e.g. dropping a stale OAuth provider
// entry); a non-nil fields means "sjson.Set each entry in fields", and key
// is ignored in that case.
type pendingDiskAction struct {
	scope  Scope
	key    string
	fields map[string]any
}

// applyPendingDiskActions performs the disk mutations collected during
// provider merging and validation. Failures are logged and otherwise
// ignored: this is best-effort persistence, not something that should fail
// config loading.
func applyPendingDiskActions(store *ConfigStore, actions []pendingDiskAction) {
	for _, action := range actions {
		if action.fields == nil {
			err := store.atomicWrite(action.scope, func(data []byte) ([]byte, error) {
				v, sErr := sjson.Delete(string(data), action.key)
				if sErr != nil {
					return nil, fmt.Errorf("failed to delete config field %s: %w", action.key, sErr)
				}
				return []byte(v), nil
			})
			if err != nil {
				slog.Warn("Failed to remove stale config field", "key", action.key, "error", err)
			}
			continue
		}

		// Sort keys for deterministic output regardless of map iteration
		// order, same as writeConfigFields. This is a deliberate inline
		// duplicate of that logic rather than a call to it (or to
		// SetConfigFields): both of those trigger autoReload, which
		// would recursively reload mid-load.
		keys := make([]string, 0, len(action.fields))
		for k := range action.fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)

		err := store.atomicWrite(action.scope, func(data []byte) ([]byte, error) {
			v := string(data)
			for _, key := range keys {
				var sErr error
				if v, sErr = sjson.Set(v, key, action.fields[key]); sErr != nil {
					return nil, fmt.Errorf("failed to set config field %s: %w", key, sErr)
				}
			}
			return []byte(v), nil
		})
		if err != nil {
			slog.Warn("Failed to persist config fields", "keys", keys, "error", err)
		}
	}
}

// mergeCatalogProviders merges the embedded/known provider catalog with the
// user's overrides (base_url, api_key, models, headers), applying
// vendor-specific special cases (Anthropic OAuth removal, Copilot OAuth
// setup, Vertex AI/Azure/Bedrock credential requirements). It returns the
// set of known provider IDs (so later phases can skip them) and any disk
// cleanup the caller should apply once merging is done.
//
// This is pure in-memory work plus resolver calls (which may run shell
// commands for $(...) substitutions, but never network I/O) — no HTTP,
// unlike discoverCustomProviderModels.
func (c *Config) mergeCatalogProviders(env env.Env, resolver VariableResolver, knownProviders []catwalk.Provider) (map[string]bool, []pendingDiskAction, error) {
	knownProviderNames := make(map[string]bool)
	var actions []pendingDiskAction

	for _, p := range knownProviders {
		knownProviderNames[string(p.ID)] = true
		config, configExists := c.Providers.Get(string(p.ID))
		// if the user configured a known provider we need to allow it to override a couple of parameters
		if configExists {
			if config.BaseURL != "" {
				p.APIEndpoint = config.BaseURL
			}
			if config.APIKey != "" {
				p.APIKey = config.APIKey
			}
			if len(config.Models) > 0 {
				models := []catwalk.Model{}
				seen := make(map[string]bool)

				for _, model := range config.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}
				for _, model := range p.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}

				p.Models = models
			}
		}

		headers := map[string]string{}
		if len(p.DefaultHeaders) > 0 {
			maps.Copy(headers, p.DefaultHeaders)
		}
		if len(config.ExtraHeaders) > 0 {
			maps.Copy(headers, config.ExtraHeaders)
		}
		// Provider headers use the same error contract as MCP headers:
		// a failing $(...) aborts the provider load with a clear
		// message, and a header that resolves to the empty string
		// (unset bare $VAR under lenient nounset, $(echo), or literal
		// "") is dropped from the outgoing request.
		for k, v := range headers {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving provider %s header %q: %w", p.ID, k, err)
			}
			if resolved == "" {
				delete(headers, k)
				continue
			}
			headers[k] = resolved
		}
		// Start from user config so all user fields survive without
		// explicit copying. Overlay catwalk identity/endpoint fields
		// (already merged with user overrides above).
		prepared := config
		prepared.ID = string(p.ID)
		prepared.Name = p.Name
		prepared.BaseURL = p.APIEndpoint
		prepared.APIKey = p.APIKey
		prepared.APIKeyTemplate = p.APIKey // Store original template for re-resolution
		prepared.Type = p.Type
		prepared.Models = p.Models
		prepared.ExtraHeaders = headers
		// The proxy URL only ever comes from the user's override (the
		// catwalk catalog has no notion of a proxy), and it's purely
		// optional: unlike api_key, a failed or empty resolution must
		// not block the provider from loading, so we just warn and
		// leave it unset.
		if config.ProxyURL != "" {
			resolvedProxy, err := resolver.ResolveValue(config.ProxyURL)
			if err != nil || resolvedProxy == "" {
				slog.Warn("Ignoring provider proxy_url due to resolution failure", "provider", p.ID, "error", err)
			} else {
				prepared.ProxyURL = resolvedProxy
			}
		}
		if prepared.ExtraParams == nil {
			prepared.ExtraParams = make(map[string]string)
		}

		switch {
		case p.ID == catwalk.InferenceProviderAnthropic && config.OAuthToken != nil:
			// Claude Code subscription is not supported anymore. Remove to
			// show onboarding. The disk deletion is deferred to the caller
			// (applyPendingDiskActions) rather than performed here; the
			// in-memory state is kept consistent by the Providers.Del call
			// below, and any concurrent reload that races with the deferred
			// write will also see the removal because it re-reads from disk.
			actions = append(actions, pendingDiskAction{scope: ScopeGlobal, key: "providers.anthropic"})
			c.Providers.Del(string(p.ID))
			continue
		case p.ID == catwalk.InferenceProviderCopilot && config.OAuthToken != nil:
			prepared.SetupGitHubCopilot()
		}

		switch p.ID {
		// Handle specific providers that require additional configuration
		case catwalk.InferenceProviderVertexAI:
			var (
				project  = env.Get("VERTEXAI_PROJECT")
				location = env.Get("VERTEXAI_LOCATION")
			)
			if project == "" || location == "" {
				if configExists {
					slog.Warn("Skipping Vertex AI provider due to missing credentials")
					c.addProblem(Problem{
						Severity: SeverityWarn,
						Area:     AreaProvider,
						Subject:  string(p.ID),
						Message:  fmt.Sprintf("provider %s dropped: VERTEXAI_PROJECT/VERTEXAI_LOCATION not set", p.ID),
						Hint:     "static check only; set both env vars and reload",
					})
					c.Providers.Del(string(p.ID))
				}
				continue
			}
			prepared.ExtraParams["project"] = project
			prepared.ExtraParams["location"] = location
		case catwalk.InferenceProviderAzure:
			endpoint, err := resolver.ResolveValue(p.APIEndpoint)
			if err != nil || endpoint == "" {
				if configExists {
					slog.Warn("Skipping Azure provider due to missing API endpoint", "provider", p.ID, "error", err)
					c.addProblem(Problem{
						Severity: SeverityWarn,
						Area:     AreaProvider,
						Subject:  string(p.ID),
						Message:  fmt.Sprintf("provider %s dropped: missing API endpoint", p.ID),
						Hint:     "static check only; set base_url and reload",
					})
					c.Providers.Del(string(p.ID))
				}
				continue
			}
			prepared.BaseURL = endpoint
			prepared.ExtraParams["apiVersion"] = env.Get("AZURE_OPENAI_API_VERSION")
		case catwalk.InferenceProviderBedrock, catwalk.InferenceProviderBedrockEurope:
			if p.APIKey == "" && !hasAWSCredentials(env) {
				if configExists {
					slog.Warn("Skipping Bedrock provider due to missing AWS credentials")
					c.addProblem(Problem{
						Severity: SeverityWarn,
						Area:     AreaProvider,
						Subject:  string(p.ID),
						Message:  fmt.Sprintf("provider %s dropped: no api_key and no AWS credentials found", p.ID),
						Hint:     "static check only; set api_key or AWS credentials and reload",
					})
					c.Providers.Del(string(p.ID))
				}
				continue
			}
		default:
			// if the provider api or endpoint are missing we skip them
			v, err := resolver.ResolveValue(p.APIKey)
			if v == "" || err != nil {
				if configExists {
					slog.Warn("Skipping provider due to missing API key", "provider", p.ID)
					c.addProblem(Problem{
						Severity: SeverityWarn,
						Area:     AreaProvider,
						Subject:  string(p.ID),
						Message:  fmt.Sprintf("provider %s dropped: missing api_key", p.ID),
						Hint:     "static check only; set api_key and reload",
					})
					c.Providers.Del(string(p.ID))
				}
				continue
			}
		}
		c.Providers.Set(string(p.ID), prepared)
	}

	return knownProviderNames, actions, nil
}
