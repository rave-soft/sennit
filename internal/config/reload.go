package config

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/env"
)

// ReloadFromDisk re-runs the config load/merge flow and updates the in-memory
// config atomically. It rebuilds the staleness snapshot after successful reload.
// Concurrent calls (including a racing autoReload) are serialised via
// reloadMu; see reloadFromDisk for why that is a separate lock from writeMu.
func (s *ConfigStore) ReloadFromDisk(ctx context.Context) error {
	if s.workingDir == "" {
		return fmt.Errorf("cannot reload: working directory not set")
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.reloadFromDisk(ctx)
}

// snapshotOverrides returns a copy of the runtime overrides deep enough to
// read without holding writeMu afterward: the top-level struct plus its
// slice/pointer fields are cloned so a concurrent mutator (which replaces
// overrides.Model under writeMu, see pinPreferredModelLocked) cannot race a
// caller reading the snapshot later.
func (s *ConfigStore) snapshotOverrides() RuntimeOverrides {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	var model *SelectedModel
	if s.overrides.Model != nil {
		m := *s.overrides.Model
		model = &m
	}
	return RuntimeOverrides{
		SkipPermissionRequests: s.overrides.SkipPermissionRequests,
		EnabledChannels:        slices.Clone(s.overrides.EnabledChannels),
		Model:                  model,
	}
}

// reloadFromDisk performs the actual reload. Caller must hold reloadMu (to
// serialise reload attempts against each other) but must NOT hold writeMu on
// entry: nearly all of this — disk reads, JSON merging, provider catalog
// refresh, and any model-discovery HTTP calls for custom providers run via
// configureProviders — happens before writeMu is ever touched, so a slow
// discovery endpoint never blocks a concurrent mutator's Lock or autoReload's
// TryLock. writeMu is acquired only for the final swap into store state.
//
// Because model resolution (resolveSelectedModels) also runs before writeMu
// is taken, a failure there returns before any store state changes, which is
// what makes the old rollback-on-setup-failure logic unnecessary here: there
// is nothing to roll back yet.
func (s *ConfigStore) reloadFromDisk(ctx context.Context) error {
	// Migrate deprecated disable_notifications before reloading config.
	migrateDisableNotifications()

	s.writeMu.RLock()
	startCredentialVersion := s.CredentialVersion()
	startConfig := s.Config()
	s.writeMu.RUnlock()
	configPaths := lookupConfigs(s.workingDir)
	cfg, loadedPaths, err := loadFromConfigPaths(ctx, configPaths)
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// Apply defaults (using existing data directory if set)
	var dataDir string
	if cur := s.Config(); cur != nil && cur.Options != nil {
		dataDir = cur.Options.DataDirectory
	}
	cfg.setDefaults(s.workingDir, dataDir)

	if err := applyWorkspaceConfig(cfg, s.workingDir, &loadedPaths); err != nil {
		return err
	}

	// Apply the same environment-derived defaults Load applies at startup,
	// so a reload does not silently drop them — see
	// TestLoad_AppleTerminalDefaultSurvivesReload.
	applyEnvironmentDefaults(cfg)

	// Validate hooks after all config merging is complete so matcher
	// regexes are recompiled on the reloaded config (mirrors Load).
	if err := cfg.ValidateHooks(); err != nil {
		return fmt.Errorf("invalid hook configuration on reload: %w", err)
	}

	// Snapshot runtime overrides up front (a brief writeMu.RLock) rather
	// than reading s.overrides directly, since the rest of this function
	// runs without writeMu held and a concurrent mutator could otherwise
	// race a later read of the same field.
	overrides := s.snapshotOverrides()

	// Reapply a model choice made in this instance. The global config file
	// is shared, so it may now name a model a sibling instance selected; a
	// reload triggered by an unrelated write must not swap the user's model
	// mid-session. An external edit to the config still takes effect when
	// this instance never chose a model of its own.
	if overrides.Model != nil {
		cfg.Model = *overrides.Model
	}

	// Reconfigure providers
	env := env.New()
	resolver := NewShellVariableResolver(env)

	// Apply top-level env vars before configuring providers so variables
	// like AWS_PROFILE are visible to the AWS SDK credential chain.
	cfg.applyEnv(resolver)

	providers, err := Providers(cfg)
	if err != nil {
		if len(providers) == 0 {
			return fmt.Errorf("failed to load providers during reload: %w", err)
		}
		slog.Warn("Reload continuing with the previously known providers", "error", err)
	}

	// configureProviders may run model-discovery HTTP calls for custom
	// providers (see discoverCustomProviderModels). This runs here, before
	// writeMu is taken below — see the writeMu doc comment on ConfigStore
	// and TestReloadFromDiskLocked_DiscoveryDoesNotBlockWriteMu.
	if err := cfg.configureProviders(ctx, s, env, resolver, providers); err != nil {
		return fmt.Errorf("failed to configure providers during reload: %w", err)
	}

	var resolved resolvedModel
	configured := cfg.IsConfigured()
	if configured {
		resolved, err = resolveSelectedModel(cfg, providers)
		if err != nil {
			return fmt.Errorf("failed to configure selected models during reload: %w", err)
		}
		// Unlike Load, a fallback model correction (resolved.Fallback)
		// is not persisted to disk here, only applied in memory. This
		// matches reloadFromDiskLocked's pre-existing behavior; persisting
		// via updateLocked here would need its own failure handling (Load
		// can simply discard the whole store on error, but a reload has
		// already published a config other goroutines may be reading).
		// Left as-is rather than risked as part of this refactor.
		cfg.Model = resolved.Model
	} else {
		slog.Warn("No providers configured after reload")
	}

	// Everything above is pure computation (plus the best-effort disk
	// cleanup configureProviders may have performed); no store state has
	// changed yet. Take writeMu only for the swap, so the compute-heavy
	// (and potentially network-bound) part of a reload is never on the
	// critical path for a concurrent mutator.
	//
	// Unlike the old reloadFromDiskLocked, there is no rollback path here:
	// every fallible step above (loadFromConfigPaths, ValidateHooks,
	// Providers, configureProviders, resolveSelectedModels) already
	// returned before this point on failure, so the swap below cannot fail
	// partway through in a way that would need undoing.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if configured {
		// Set up agents on the new config before publishing it.
		// This preserves the invariant that a published Config is never
		// mutated in place: SetupAgents is called on cfg (the not-yet-
		// published clone) and only then is cfg swapped into the store.
		cfg.SetupAgentsWithInherited(s.inheritedAgents)
	}

	if s.CredentialVersion() != startCredentialVersion {
		current := s.Config()
		if startConfig != nil && current != nil && startConfig.Providers != nil && current.Providers != nil && cfg.Providers != nil {
			for id, currentProvider := range current.Providers.Seq2() {
				startProvider, existed := startConfig.Providers.Get(id)
				if existed && startProvider.APIKey == currentProvider.APIKey && reflect.DeepEqual(startProvider.OAuthToken, currentProvider.OAuthToken) {
					continue
				}
				provider, ok := cfg.Providers.Get(id)
				if !ok {
					continue
				}
				provider.APIKey = currentProvider.APIKey
				provider.OAuthToken = currentProvider.OAuthToken
				if id == string(catwalk.InferenceProviderCopilot) {
					provider.SetupGitHubCopilot()
				}
				cfg.Providers.Set(id, provider)
			}
		}
	}

	s.setConfig(cfg)
	s.loadedPaths = loadedPaths
	s.resolver = resolver
	s.knownProviders = providers
	s.overrides = overrides
	s.workspacePath = filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName))

	// Rebuild staleness tracking. Track every discovered config path, not
	// just the ones that loaded, so a config file created after this reload
	// is detected as a change on the next staleness check.
	s.captureStalenessSnapshot(append(slices.Clone(configPaths), loadedPaths...))
	s.captureAgentFileSnapshot()

	return nil
}

// autoReload conditionally reloads config from disk after writes.
// It returns nil (no error) for expected skip cases: when auto-reload is
// disabled during load/reload flows, or when working directory is not set
// (e.g., during testing). Only actual reload failures return an error.
func (s *ConfigStore) autoReload(ctx context.Context) error {
	if s.workingDir == "" {
		return nil // Expected skip: working directory not set
	}
	// Skip if a reload is already in progress. reloadMu (not writeMu) is
	// the right lock to probe here: writeMu is now only held for the brief
	// final swap of a reload, so TryLock-ing it would not detect a reload
	// whose disk/HTTP work is still in flight. This still covers both
	// concurrent auto-reloads after parallel writes and any call made
	// while an explicit ReloadFromDisk is running.
	//
	// Note: if a write completes after the in-progress reload has
	// already read the config file, that write won't be reflected in
	// memory until the next reload. This is acceptable because writes
	// are rare and the next user action or file-watch tick will pick
	// up the change. Callers that need guaranteed fresh state after a
	// write should call ReloadFromDisk explicitly.
	if !s.reloadMu.TryLock() {
		return nil
	}
	defer s.reloadMu.Unlock()
	return s.reloadFromDisk(ctx)
}
