package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/home"
)

// buildConfigOptions parameterizes buildConfig, the pipeline shared by Load
// and reloadFromDisk. Every field is either identical between the two
// callers or a deliberate, named divergence — see each field's comment.
type buildConfigOptions struct {
	ctx        context.Context
	workingDir string
	dataDir    string

	// migrateModelCache runs the one-time bloated-model-cache migration.
	// Idempotent but pointless to repeat every reload, so only Load sets it.
	migrateModelCache bool

	// presetModel, non-nil, seeds cfg.Model before resolution: reloadFromDisk
	// reapplies the model it already pinned (pinPreferredModelLocked) so a
	// reload can't let a sibling instance's write swap it out. nil for Load.
	presetModel *SelectedModel

	// persistFallback: persist a fallback model correction found during
	// resolution. Load does; reloadFromDisk does not (persisting mid-reload
	// would need its own failure handling, unlike Load discarding the whole
	// store on error). The one explicitly-known behavioral divergence here.
	persistFallback bool
	credentialsFile credentialsFileDependency
	processor       RuntimeProcessor
}

// builtConfig is the result of one buildConfig run. configured mirrors
// cfg.IsConfigured() at the point resolution would otherwise run; when
// false, resolved is unset. persistFallback is opts.persistFallback &&
// resolved.Fallback — callers act on it under their own writeMu section,
// since that write choreography differs between Load and reloadFromDisk.
type builtConfig struct {
	cfg             *Config
	configPaths     []string
	loadedPaths     []string
	providers       []catwalk.Provider
	resolver        VariableResolver
	configured      bool
	resolved        resolvedModel
	persistFallback bool
}

// buildConfig runs the config-building pipeline shared by Load and
// reloadFromDisk: discover config paths, merge every layer (including the
// workspace config), apply defaults and env-derived overrides, configure
// providers (which may run model-discovery HTTP calls), and resolve the
// selected model. It has no store-publishing side effects — store is used
// only for globalDataPath and atomicWrite, both disk-only and already safe
// before a reload's writeMu swap. Callers own everything after: creating or
// updating the ConfigStore, pinning/persisting the model,
// SetupAgents(WithInherited), and the staleness snapshot.
func providersFromConfig(cfg *Config) []catwalk.Provider {
	providers := make([]catwalk.Provider, 0, cfg.Providers.Len())
	for id, configured := range cfg.Providers.Seq2() {
		providers = append(providers, catwalk.Provider{Name: configured.Name, ID: catwalk.InferenceProvider(id), Models: configured.Models})
	}
	return providers
}

func hasProjectConfig(paths []string, workspacePath string) bool {
	for _, path := range paths {
		if !isGlobalConfigPath(path) {
			if _, err := os.Stat(path); err == nil {
				return true
			}
		}
	}
	_, err := os.Stat(workspacePath)
	return err == nil
}

func buildConfig(store *ConfigStore, opts buildConfigOptions) (*builtConfig, error) {
	if opts.credentialsFile.stat == nil {
		opts.credentialsFile = credentialsFileDependency{homeDir: home.Dir(), stat: os.Stat}
	}
	configPaths := lookupConfigs(opts.workingDir)

	trusted := IsTrusted(opts.workingDir)
	cfg, loadedPaths, err := loadFromConfigPaths(opts.ctx, configPaths, trusted)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	cfg.setDefaults(opts.workingDir, opts.dataDir)

	// Reapply the process's --debug flag on every build, not just the first
	// one: it lives on the store (store.debugOverride), not in the config
	// file, so a reload's fresh disk read would otherwise silently drop it
	// back to false. See debugOverride's doc comment.
	if store.debugOverride {
		cfg.Options.Debug = true
	}

	if trusted {
		if err := applyWorkspaceConfig(cfg, opts.workingDir, &loadedPaths); err != nil {
			return nil, err
		}
	} else if hasProjectConfig(configPaths, filepath.Join(cfg.Options.DataDirectory, appName+".json")) {
		cfg.addProblem(Problem{
			Severity: SeverityWarn,
			Area:     AreaEnvironment,
			Subject:  opts.workingDir,
			Message:  "project configuration is disabled because this project is not trusted",
			Hint:     "Review the project, then restart with --trust-project to enable its configuration.",
		})
	}

	// Validate hooks after all config merging is complete so workspace
	// hooks also get their matcher regexes compiled.
	if err := cfg.ValidateHooks(); err != nil {
		return nil, fmt.Errorf("invalid hook configuration: %w", err)
	}

	applyEnvironmentDefaults(cfg)

	if opts.presetModel != nil {
		cfg.Model = *opts.presetModel
	}

	var providers []catwalk.Provider
	resolver := cfg.RuntimeResolver()
	if opts.processor != nil {
		// KnownProviders is intentionally left unset here: providers is only
		// ever populated below, from the processor's own result, so there is
		// nothing yet to hand it. RuntimeProcessor implementations (see
		// internal/providerload) fall back to their own default catalog when
		// it is empty.
		result, err := opts.processor.Process(opts.ctx, RuntimeInput{
			Config:          cfg,
			Store:           store,
			GlobalDataPath:  store.globalDataPath,
			CredentialsHome: opts.credentialsFile.homeDir,
			Stat:            opts.credentialsFile.stat,
			Initial:         opts.migrateModelCache,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to configure providers: %w", err)
		}
		providers = result.KnownProviders
		cfg.RuntimeProviders = result.RuntimeProviders
		resolver = result.Resolver
	} else {
		providers = providersFromConfig(cfg)
	}

	built := &builtConfig{
		cfg:         cfg,
		configPaths: configPaths,
		loadedPaths: loadedPaths,
		providers:   providers,
		resolver:    resolver,
	}

	built.configured = opts.processor != nil && cfg.IsConfigured()
	if !built.configured {
		return built, nil
	}

	resolved, err := resolveSelectedModel(cfg, providers)
	if err != nil {
		return nil, fmt.Errorf("failed to configure selected models: %w", err)
	}
	cfg.Model = resolved.Model
	built.resolved = resolved
	built.persistFallback = opts.persistFallback && resolved.Fallback

	return built, nil
}
