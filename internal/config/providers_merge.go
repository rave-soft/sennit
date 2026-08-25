package config

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// configureProviders is a thin wrapper around three phases that used to be
// inlined here as one ~290-line function: mergeCatalogProviders (merge the
// embedded catalog with user overrides, applying vendor special cases),
// discovery (pure HTTP model discovery for custom providers, itself split
// into resolveDiscoveryRequests and runDiscoveryRequests — see below), and
// validateCustomProviders (apply discovery results and drop invalid custom
// providers).
//
// processEnvMu (see PushPopEnvOverrides) is taken and released twice here
// rather than once around the whole call: everything on either side of the
// HTTP round trip resolves values through resolver, which reads process env
// on every call, so the SENNIT_ overrides must be in place for it. The
// round trip itself (runDiscoveryRequests) never touches the resolver, so
// it runs with the lock released — a slow discovery endpoint (bounded by a
// 3s timeout per provider) would otherwise hold every other workspace's
// config load hostage behind processEnvMu for that long. Splitting the
// HTTP-performing phase out this way also lets callers (Load,
// reloadFromDisk) run it before taking writeMu — see those call sites for
// why that matters.
func (c *Config) configureProviders(ctx context.Context, store *ConfigStore, env env.Env, resolver VariableResolver, knownProviders []catwalk.Provider, dependencies ...credentialsFileDependency) error {
	restore := PushPopEnvOverrides()

	// When disable_default_providers is enabled, skip all default/embedded
	// providers entirely. Users must fully specify any providers they want.
	// We skip to the custom provider validation loop which handles all
	// user-configured providers uniformly.
	if c.Options.DisableDefaultProviders {
		knownProviders = nil
	}

	credentialsFile := credentialsFileDependency{homeDir: home.Dir(), stat: os.Stat}
	if len(dependencies) > 0 {
		credentialsFile = dependencies[0]
	}
	knownProviderNames, actions, err := c.mergeCatalogProviders(env, resolver, knownProviders, credentialsFile)
	if err != nil {
		restore()
		return err
	}

	// Mark ModelsSource and fill in Models for custom providers from the
	// cache before discovery runs, so a restart doesn't re-trigger an HTTP
	// round trip just because the JSON config no longer carries the
	// discovered list (see resolveCustomProviderModels).
	resolveCustomProviderModels(c.Providers, knownProviderNames, store.globalDataPath)

	requests, discoveryResults := resolveDiscoveryRequests(c.Providers, knownProviderNames, resolver)
	restore()

	maps.Copy(discoveryResults, runDiscoveryRequests(ctx, requests))

	restore = PushPopEnvOverrides()
	defer restore()

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
		return fmt.Errorf("default providers are disabled and no custom providers are configured")
	}

	return nil
}

// removeStaleGlobalKey deletes a global-only key (e.g. a stale Claude Code
// OAuth provider entry) from whichever global config layer actually
// declares it. ConfigPath(ScopeGlobal) only ever resolves to the
// machine-owned data file, but a key like this can just as easily have
// been written -- by hand, or by an older version of Sennit that supported
// writing it there -- into the user's own global sennit.json/sennitrc
// instead. Trying every global layer in globalConfigPaths(), and skipping
// any file that does not actually contain the key, is what keeps this a
// real fix rather than a permanent no-op on the data file plus an
// unconditional rewrite of it on every load: that rewrite bumped the data
// file's mtime on every reload, which made every other running instance
// mistake this process's own no-op write for an external change and
// reload, ping-ponging between instances.
//
// Skipping a file that was never created (os.Stat) matters too: atomicWrite
// creates the parent directory unconditionally, so trying every layer
// without this check would create directories (and, for a system-wide
// path, possibly fail on permissions) for config files that don't exist and
// have no reason to.
func removeStaleGlobalKey(store *ConfigStore, key string) {
	for _, path := range globalConfigPaths() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		err := store.file.atomicWrite(path, func(data []byte) ([]byte, error) {
			// errAtomicWriteNoop is the belt to the os.Stat suspenders'
			// braces: it is what actually prevents the rewrite (and the
			// mtime bump that comes with it) when the key isn't present,
			// closing the race where the file is created or edited between
			// the Stat above and this callback running under the lock.
			if !gjson.GetBytes(data, key).Exists() {
				return nil, errAtomicWriteNoop
			}
			v, sErr := sjson.Delete(string(data), key)
			if sErr != nil {
				return nil, fmt.Errorf("failed to delete config field %s: %w", key, sErr)
			}
			return []byte(v), nil
		})
		if err != nil {
			slog.Warn("Failed to remove stale config field", "key", key, "path", path, "error", err)
		}
	}
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
			if action.scope == ScopeGlobal {
				// A global-only key like "providers.anthropic" may live in
				// any of the global config layers, not just the one
				// ConfigPath(ScopeGlobal) resolves to (the machine-owned
				// data file). See removeStaleGlobalKey.
				removeStaleGlobalKey(store, action.key)
				continue
			}
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

		// writeConfigFields is the same no-reload disk write SetConfigFields
		// uses, minus the autoReload that follows it there — calling it
		// directly (rather than SetConfigFields) is what avoids recursively
		// reloading mid-load; writeConfigFields itself never reloads.
		if err := store.writeConfigFields(action.scope, action.fields); err != nil {
			keys := make([]string, 0, len(action.fields))
			for k := range action.fields {
				keys = append(keys, k)
			}
			slices.Sort(keys)
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
// unlike runDiscoveryRequests.
//
// Per provider, this runs three passes, each factored out below so it reads
// and tests on its own: mergeProviderOverride (user overrides onto the
// catalog entry), applyProviderVendorSetup (OAuth wiring), and
// applyProviderCredentials (the checks that decide whether it loads).
func (c *Config) mergeCatalogProviders(env env.Env, resolver VariableResolver, knownProviders []catwalk.Provider, credentialsFile credentialsFileDependency) (map[string]bool, []pendingDiskAction, error) {
	knownProviderNames := make(map[string]bool)
	var actions []pendingDiskAction

	for _, p := range knownProviders {
		knownProviderNames[string(p.ID)] = true
		config, configExists := c.Providers.Get(string(p.ID))
		p = mergeProviderOverride(p, config, configExists)

		headers := map[string]string{}
		if len(p.DefaultHeaders) > 0 {
			maps.Copy(headers, p.DefaultHeaders)
		}
		if len(config.ExtraHeaders) > 0 {
			maps.Copy(headers, config.ExtraHeaders)
		}
		if err := resolveProviderHeaders(headers, resolver, string(p.ID)); err != nil {
			return nil, nil, err
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
		// catwalk catalog has no notion of a proxy). prepared started as a
		// copy of the user config, so an unresolved/failed template must be
		// explicitly cleared here or it reaches the HTTP client builder
		// verbatim and fails the whole provider instead of being ignored.
		prepared.ProxyURL = resolveOptionalProxy(config.ProxyURL, resolver, string(p.ID))
		if prepared.ExtraParams == nil {
			prepared.ExtraParams = make(map[string]string)
		}

		prepared, dropped, diskAction := c.applyProviderVendorSetup(prepared, p, config)
		if diskAction != nil {
			actions = append(actions, *diskAction)
		}
		if dropped {
			continue
		}

		prepared, ok := c.applyProviderCredentials(env, resolver, p, configExists, prepared, credentialsFile)
		if !ok {
			continue
		}

		c.Providers.Set(string(p.ID), prepared)
	}

	return knownProviderNames, actions, nil
}

// mergeProviderOverride applies the user's config overrides (base_url,
// api_key, models) onto a catalog provider entry. Pure transform, no
// resolver/disk/log side effects, so it can be tested with plain structs.
func mergeProviderOverride(p catwalk.Provider, config ProviderConfig, configExists bool) catwalk.Provider {
	if !configExists {
		return p
	}
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
	return p
}

// applyProviderVendorSetup handles providers needing vendor-specific OAuth
// wiring. Returns the (possibly modified) prepared config, whether the
// provider was dropped (caller should `continue` without Providers.Set),
// and a pending disk cleanup for the caller to collect, if any.
func (c *Config) applyProviderVendorSetup(prepared ProviderConfig, p catwalk.Provider, config ProviderConfig) (ProviderConfig, bool, *pendingDiskAction) {
	switch {
	case p.ID == catwalk.InferenceProviderAnthropic && config.OAuthToken != nil:
		// Claude Code subscription is not supported anymore. Remove to
		// show onboarding. The disk deletion is deferred to the caller
		// (applyPendingDiskActions) rather than performed here; the
		// in-memory state is kept consistent by the Providers.Del call
		// below, and any concurrent reload that races with the deferred
		// write will also see the removal because it re-reads from disk.
		action := pendingDiskAction{scope: ScopeGlobal, key: "providers.anthropic"}
		c.Providers.Del(string(p.ID))
		return prepared, true, &action
	case p.ID == catwalk.InferenceProviderCopilot && config.OAuthToken != nil:
		prepared.SetupGitHubCopilot()
	case string(p.ID) == codex.ProviderID:
		prepared.SetupCodex()
	}
	return prepared, false, nil
}

// applyProviderCredentials runs the per-vendor credential checks deciding
// whether a catalog provider loads at all, returning the (possibly
// modified) prepared config and whether to keep it.
//
// !configExists drops silently on missing credentials: this runs against
// the whole embedded catalog on every load, and warning about every
// catalog entry the user never configured would be noise. A provider the
// user did configure gets a slog.Warn and a doctor Problem.
func (c *Config) applyProviderCredentials(env env.Env, resolver VariableResolver, p catwalk.Provider, configExists bool, prepared ProviderConfig, credentialsFile credentialsFileDependency) (ProviderConfig, bool) {
	switch p.ID {
	// Handle specific providers that require additional configuration
	case catwalk.InferenceProviderVertexAI:
		var (
			project  = env.Get("VERTEXAI_PROJECT")
			location = env.Get("VERTEXAI_LOCATION")
		)
		if project == "" || location == "" {
			if configExists {
				problem := providerDropProblem(string(p.ID), "VERTEXAI_PROJECT/VERTEXAI_LOCATION not set", "static check only; set both env vars and reload")
				c.dropProvider(string(p.ID), slog.Warn, "Skipping Vertex AI provider due to missing credentials", nil, &problem)
			}
			return prepared, false
		}
		prepared.ExtraParams["project"] = project
		prepared.ExtraParams["location"] = location
	case catwalk.InferenceProviderAzure:
		endpoint, err := resolver.ResolveValue(p.APIEndpoint)
		if err != nil || endpoint == "" {
			if configExists {
				problem := providerDropProblem(string(p.ID), "missing API endpoint", "static check only; set base_url and reload")
				c.dropProvider(string(p.ID), slog.Warn, "Skipping Azure provider due to missing API endpoint", []any{"provider", p.ID, "error", err}, &problem)
			}
			return prepared, false
		}
		prepared.BaseURL = endpoint
		prepared.ExtraParams["apiVersion"] = env.Get("AZURE_OPENAI_API_VERSION")
	case catwalk.InferenceProviderBedrock, catwalk.InferenceProviderBedrockEurope:
		if p.APIKey == "" && !hasAWSCredentialsWithFiles(env, credentialsFile.homeDir, credentialsFile.stat) {
			if configExists {
				problem := providerDropProblem(string(p.ID), "no api_key and no AWS credentials found", "static check only; set api_key or AWS credentials and reload")
				c.dropProvider(string(p.ID), slog.Warn, "Skipping Bedrock provider due to missing AWS credentials", nil, &problem)
			}
			return prepared, false
		}
	default:
		// if the provider api or endpoint are missing we skip them
		v, err := resolver.ResolveValue(p.APIKey)
		if v == "" || err != nil {
			if configExists {
				problem := providerDropProblem(string(p.ID), "missing api_key", "static check only; set api_key and reload")
				c.dropProvider(string(p.ID), slog.Warn, "Skipping provider due to missing API key", []any{"provider", p.ID}, &problem)
			}
			return prepared, false
		}
	}
	return prepared, true
}
