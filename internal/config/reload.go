package config

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
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

	// Snapshot runtime overrides up front (a brief writeMu.RLock) rather
	// than reading s.overrides directly, since the rest of this function
	// runs without writeMu held and a concurrent mutator could otherwise
	// race a later read of the same field. This snapshot is only used to
	// seed buildConfig's presetModel below — unlike the old behaviour, it
	// is never written back to s.overrides, since s.overrides may have
	// been mutated (a model pin, SkipPermissionRequests, EnabledChannels)
	// by someone else while this reload's disk/HTTP work was in flight,
	// and writing the stale snapshot back would silently revert that.
	overrides := s.snapshotOverrides()

	// Reapply this instance's pinned model as buildConfig's presetModel —
	// see that field's doc comment for why.
	var presetModel *SelectedModel
	if overrides.Model != nil {
		m := *overrides.Model
		presetModel = &m
	}

	// Stat every currently tracked config file now, before buildConfig
	// re-reads their contents below; see preReloadFileSnapshots. Statting
	// only at the end (after the swap) would record the mtime/size of a
	// file written mid-reload without ever having loaded its new content,
	// silently absorbing that write instead of flagging it as stale.
	preRead := s.preReloadFileSnapshots()

	// Apply defaults using the existing data directory, if set.
	var dataDir string
	if cur := s.Config(); cur != nil && cur.Options != nil {
		dataDir = cur.Options.DataDirectory
	}

	// buildConfig runs configureProviders (which may perform model-discovery
	// HTTP calls) before writeMu is taken below — see the writeMu doc
	// comment on ConfigStore and TestReloadFromDiskLocked_DiscoveryDoesNotBlockWriteMu.
	built, err := buildConfig(s, buildConfigOptions{
		ctx:             ctx,
		workingDir:      s.workingDir,
		dataDir:         dataDir,
		presetModel:     presetModel,
		persistFallback: false, // see buildConfigOptions.persistFallback
	})
	if err != nil {
		return err
	}
	cfg := built.cfg

	if !built.configured {
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

	if built.configured {
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

	// presetModel (snapshotted before the slow work above) may now be
	// stale: a concurrent OverridePreferredModel/pinPreferredModelLocked
	// call could have pinned a different model while buildConfig's
	// disk/HTTP work was in flight. s.overrides is authoritative here —
	// we hold writeMu and, unlike presetModel, nothing below overwrites
	// it — so re-check against it and, if it moved, apply the newer pin
	// to cfg rather than publishing a config built around a model choice
	// that has since been superseded.
	if s.overrides.Model != nil && (presetModel == nil || !reflect.DeepEqual(*s.overrides.Model, *presetModel)) {
		cfg.Model = *s.overrides.Model
	}

	s.setConfig(cfg)
	s.loadedPaths = built.loadedPaths
	s.resolver = built.resolver
	s.knownProviders = built.providers
	s.workspacePath.Set(filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName)))

	// Rebuild staleness tracking. Track every discovered config path, not
	// just the ones that loaded, so a config file created after this reload
	// is detected as a change on the next staleness check. preRead supplies
	// the pre-buildConfig stat for every path already tracked, so a write
	// that landed after we read the files but before this point is not
	// mistaken for "seen" — see preReloadFileSnapshots.
	s.captureStalenessSnapshot(append(slices.Clone(built.configPaths), built.loadedPaths...), preRead)
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
