package config

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/env"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/rave-soft/sennit/internal/providers/state"
)

// RuntimeProcessor runs the provider-catalog/discovery pipeline that turns
// cfg.Providers into RuntimeInput.KnownProviders/RuntimeProviders. ConfigStore
// calls internal/providers/runtime.FromConfig/ApplyPostCredentialSetup
// directly for the smaller, single-provider calls — see
// applyProviderCredentials below and credential_store.go — leaving Process
// as the one real injection seam: it does substantial, non-trivial work
// (catalog merge, model discovery, credential validation) that config's own
// tests substitute fakes for (see runtime_test.go's testRuntimeProcessor) to
// stay fast and deterministic rather than hitting the network or the
// embedded catalog on every test.
type RuntimeProcessor interface {
	Process(context.Context, RuntimeInput) (RuntimeResult, error)
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

// applyProviderCredentials runs post-credential vendor setup (Copilot's
// static headers, Codex's chatgpt-account-id header) on provider.
func (s *ConfigStore) applyProviderCredentials(provider state.Provider) state.Provider {
	providerruntime.ApplyPostCredentialSetup(&provider)
	return provider
}

type RuntimeStore interface {
	RemoveRuntimeConfigField(Scope, string)
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
// against: the process environment, overlaid with each Env entry resolved
// in sorted key order (so a later entry may refer to an earlier one),
// overlaid with the SENNIT_-prefixed process variables stripped of that
// prefix.
//
// This is recomputed on every call, deliberately, and that is a tested
// contract rather than an oversight: an Env entry written as "$OTHER_VAR"
// must reflect the process environment as it stands now, not as it stood
// when the config was built. Two things depend on it — the auth-error
// retry path re-resolves a key written as "$MY_KEY" precisely because it
// may have been rotated since (see runtime_builder.go's
// refreshApiKeyTemplate), and TestLoadRuntimeResolverReadsCurrentEnvironment
// in internal/providerload pins the lazy behavior directly.
//
// Caching this is therefore not a free optimization, however tempting the
// cost looks: an Env entry containing "$(cmd)" runs that command on every
// call, so N entries and M resolved values means N*M executions. An
// attempt to cache it (reverted) broke both of the paths above. Any future
// attempt has to keep variable interpolation live while avoiding the
// repeated command substitution, and must satisfy both tests named here.
func (c *Config) RuntimeEnvironment() env.Env {
	// Strip herdr pane-ownership vars: resolving an Env entry can run a
	// real subprocess via $(cmd) substitution, which must not be able to
	// attach to the parent pane's agent authority (see
	// env.WithoutHerdrEnv).
	base := env.WithoutHerdrEnv(os.Environ())
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
