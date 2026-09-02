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

// RuntimeEnvironment returns the resolved runtime environment computed at
// build time (see Config.runtimeEnv and populateRuntimeEnvironment). A
// Config assembled outside the normal buildConfig pipeline (a bare &Config{}
// in a test, or one handed to NewStore) never had it populated; rather than
// silently returning an empty environment, fall back to computing it here.
// That fallback recomputes on every call, same as before this method was
// cached, but only for Configs that opted out of the build pipeline.
func (c *Config) RuntimeEnvironment() env.Env {
	if c.runtimeEnv != nil {
		return c.runtimeEnv
	}
	return c.computeRuntimeEnvironment()
}

// populateRuntimeEnvironment computes and stores the runtime environment on
// c. Callers must run this once, after c.Env is finalized and before c is
// published or handed to anything that might call RuntimeEnvironment
// (notably RuntimeResolver/ResolveValue) — see buildConfig.
func (c *Config) populateRuntimeEnvironment() {
	c.runtimeEnv = c.computeRuntimeEnvironment()
}

func (c *Config) computeRuntimeEnvironment() env.Env {
	base := os.Environ()
	environment := env.Snapshot(base, nil)
	keys := make([]string, 0, len(c.Env))
	for key := range c.Env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		resolved, err := NewShellVariableResolver(environment).ResolveValue(c.Env[key])
		if err != nil {
			slog.Warn("Skipping env var due to resolution failure.", "key", key, "value", c.Env[key], "error", err)
			continue
		}
		environment = env.Overlay(environment, map[string]string{key: resolved})
	}
	overrides := make(map[string]string)
	for _, value := range base {
		key, value, ok := strings.Cut(value, "=")
		if ok && strings.HasPrefix(key, brand.EnvPrefix) {
			overrides[strings.TrimPrefix(key, brand.EnvPrefix)] = value
		}
	}
	return env.Overlay(environment, overrides)
}

func (c *Config) AddRuntimeProblem(problem Problem) {
	c.addProblem(problem)
}
