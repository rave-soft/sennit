package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/providers/state"
)

type RuntimeProcessor interface {
	Process(context.Context, RuntimeInput) (RuntimeResult, error)
	CompileProvider(ProviderConfig, VariableResolver) (state.Provider, error)
	ApplyProviderCredentials(state.Provider) (state.Provider, error)
}

type RuntimeInput struct {
	Config          *Config
	Store           RuntimeStore
	GlobalDataPath  string
	CredentialsHome string
	Stat            func(string) (os.FileInfo, error)
	Initial         bool
	KnownProviders  []catwalk.Provider
}

type RuntimeResult struct {
	KnownProviders   []catwalk.Provider
	RuntimeProviders *csync.Map[string, state.Provider]
	Resolver         VariableResolver
}

func (s *ConfigStore) applyProviderCredentials(provider state.Provider) (state.Provider, error) {
	if s.processor == nil {
		return state.Provider{}, fmt.Errorf("provider runtime processor is not configured")
	}
	return s.processor.ApplyProviderCredentials(provider)
}

type RuntimeStore interface {
	RemoveRuntimeConfigField(Scope, string)
	WriteRuntimeConfigFields(Scope, map[string]any)
}

type runtimeEnvironmentResolver struct {
	config *Config
}

func (r runtimeEnvironmentResolver) ResolveValue(value string) (string, error) {
	return NewShellVariableResolver(r.config.RuntimeEnvironment()).ResolveValue(value)
}

func (c *Config) RuntimeResolver() VariableResolver {
	return runtimeEnvironmentResolver{config: c}
}

// RuntimeEnvironment returns the environment config values are resolved
// against: the process environment, overlaid with each Env entry, overlaid
// with the SENNIT_-prefixed process variables (stripped of that prefix).
//
// The process environment is re-read on every call, deliberately. A key
// written as "$MY_KEY" is resolved through here, and the auth-error retry
// path re-resolves it precisely because the value may have been rotated
// since the config was built (see runtime_builder.go's
// refreshApiKeyTemplate); freezing the process environment would make that
// retry re-resolve the dead key forever.
//
// What is cached is the expensive half: resolving the Env entries
// themselves, which may contain "$(cmd)" and so execute a command per
// entry. Those are resolved once, at build time, against the environment
// as it stood then — so an Env entry that interpolates a process variable
// keeps its build-time value, while a plain process variable stays live.
func (c *Config) RuntimeEnvironment() env.Env {
	base := os.Environ()
	environment := env.Snapshot(base, nil)

	overlay := c.resolvedEnv
	if overlay == nil {
		// A Config assembled outside buildConfig (a bare &Config{} in a
		// test, or one handed to NewStore) never had its Env resolved;
		// resolve it here rather than silently dropping it.
		overlay = resolveEnvEntries(environment, c.Env)
	}
	environment = env.Overlay(environment, overlay)

	overrides := make(map[string]string)
	for _, value := range base {
		key, value, ok := strings.Cut(value, "=")
		if ok && strings.HasPrefix(key, brand.EnvPrefix) {
			overrides[strings.TrimPrefix(key, brand.EnvPrefix)] = value
		}
	}
	return env.Overlay(environment, overrides)
}

// populateRuntimeEnvironment resolves c.Env once and stores the result.
// Callers must run this after c.Env is finalized and before c is published
// — see buildConfig.
func (c *Config) populateRuntimeEnvironment() {
	c.resolvedEnv = resolveEnvEntries(env.Snapshot(os.Environ(), nil), c.Env)
}

// resolveEnvEntries resolves each entry of envs against base, in sorted key
// order so a later entry can refer to an earlier one, and returns the flat
// result. An entry that fails to resolve is skipped with a warning rather
// than failing the whole environment.
func resolveEnvEntries(base env.Env, envs map[string]string) map[string]string {
	if len(envs) == 0 {
		return map[string]string{}
	}
	keys := make([]string, 0, len(envs))
	for key := range envs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	environment := base
	resolved := make(map[string]string, len(keys))
	for _, key := range keys {
		value, err := NewShellVariableResolver(environment).ResolveValue(envs[key])
		if err != nil {
			slog.Warn("Skipping env var due to resolution failure.", "key", key, "value", envs[key], "error", err)
			continue
		}
		resolved[key] = value
		environment = env.Overlay(environment, map[string]string{key: value})
	}
	return resolved
}

func (c *Config) AddRuntimeProblem(problem Problem) {
	c.addProblem(problem)
}
