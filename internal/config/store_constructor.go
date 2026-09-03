package config

import (
	"slices"
	"time"

	"github.com/rave-soft/sennit/internal/env"
)

// StoreOptions describes the runtime state for a ConfigStore constructed
// around an already assembled Config, without reading configuration files.
type StoreOptions struct {
	Config                     *Config
	WorkingDir                 string
	GlobalDataPath             string
	LoadedPaths                []string
	Resolver                   VariableResolver
	ExternalChangePollInterval time.Duration
}

// NewStore constructs a ConfigStore without running the disk load pipeline.
// Config must not be mutated after publication. LoadedPaths is copied,
// Resolver defaults to the process-backed shell resolver, and a non-positive
// poll interval uses the production default.
//
// Unlike the load pipeline (Load/LoadWithProcessor, via setConfig), this
// constructor does not freeze Config.Providers: options.Config is published
// exactly as given, so the caller remains responsible for not mutating it
// afterward. This exists for tests that hand-build a Config (see
// internal/config/configtest) rather than reading one from disk; production
// code should use the load pipeline instead.
func NewStore(options StoreOptions) *ConfigStore {
	resolver := options.Resolver
	if resolver == nil {
		resolver = NewShellVariableResolver(env.New())
	}
	pollInterval := options.ExternalChangePollInterval
	if pollInterval <= 0 {
		pollInterval = externalChangePollInterval
	}
	return &ConfigStore{
		config:         options.Config,
		workingDir:     options.WorkingDir,
		resolver:       resolver,
		globalDataPath: options.GlobalDataPath,
		loadedPaths:    slices.Clone(options.LoadedPaths),
		watcher:        externalChangeWatcher{pollInterval: pollInterval},
	}
}
