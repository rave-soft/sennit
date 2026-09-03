package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RuntimeOverrides holds per-session settings that are never persisted to
// disk. They are applied on top of the loaded Config and survive only for
// the lifetime of the process (or workspace).
type RuntimeOverrides struct {
	SkipPermissionRequests bool
	// EnabledChannels lists the MCP servers opted in as channels for this
	// session (via the --channels flag). A server present in MCP config only
	// pushes channel events when it also appears here. Entries may be written
	// as "server:<name>" or as a bare "<name>".
	EnabledChannels []string
	// Model is the model this instance is running: pinned at startup from
	// the config file and updated whenever this instance selects another,
	// persisted or not. It is reapplied after a config reload so that the
	// shared config file — which every sibling instance also writes to —
	// cannot switch a running session's model. See pinPreferredModelLocked.
	// Nil only before Load has resolved a model at all.
	Model *SelectedModel
}

// cloneRuntimeOverrides deep-copies o's slice/pointer fields (EnabledChannels,
// Model) so the result stays safe to read after the caller's lock on the live
// overrides is released. It does no locking itself; callers hold whatever
// lock guards s.overrides (writeMu) for the read that produces o.
func cloneRuntimeOverrides(o RuntimeOverrides) RuntimeOverrides {
	var model *SelectedModel
	if o.Model != nil {
		m := *o.Model
		model = &m
	}
	return RuntimeOverrides{
		SkipPermissionRequests: o.SkipPermissionRequests,
		EnabledChannels:        slices.Clone(o.EnabledChannels),
		Model:                  model,
	}
}

// ConfigStore is the single entry point for all config access. It owns the
// pure-data Config, runtime state (working directory, resolver, known
// providers), and persistence to both global and workspace config files.
//
// file.mu serialises all config file mutations (SetConfigFields,
// RemoveConfigField, PersistRefreshedToken) to prevent both in-process
// goroutine races and, together with the shared lock.File, cross-process
// races on the config file. See configFile's doc comment.
//
// writeMu serialises every operation that produces a new in-memory Config:
// the typed copy-on-write mutators (SetCompactMode, UpdatePreferredModel,
// ...) and the final swap step of a reload. This is what lets published
// Configs be treated as immutable: a mutator clones, mutates the clone, and
// swaps it in under writeMu rather than mutating the live Config in place.
//
// Unlike mutators, a reload does most of its work — disk reads, JSON
// merging, provider catalog refresh, and any model-discovery HTTP calls for
// custom providers — before it ever touches writeMu; see reloadFromDisk.
// Holding writeMu across a slow discovery round trip would block every
// other mutator (SetConfigField, UpdatePreferredModel, ...) for the
// duration, which is what
// TestReloadFromDiskLocked_DiscoveryDoesNotBlockWriteMu guards against.
// writeMu is taken only for the brief final swap.
//
// reloadMu serialises reload *attempts* against each other (concurrent
// autoReload calls, or an explicit ReloadFromDisk racing autoReload) —
// a job writeMu no longer does now that a reload only holds it briefly.
// autoReload takes reloadMu with TryLock so a reload triggered while
// another is already in flight skips instead of running redundant disk
// I/O and HTTP calls concurrently with it.
type ConfigStore struct {
	config          *Config
	workingDir      string
	resolver        VariableResolver
	globalDataPath  string // ~/.local/share/sennit/sennit.json
	credentialsFile credentialsFileDependency
	processor       RuntimeProcessor
	// workspacePath (.sennit/sennit.json) is read from paths that hold
	// writeMu (updateLocked's staleness refresh) and from paths that hold
	// nothing (ConfigPath's public callers), while reloadFromDisk
	// reassigns it under writeMu. A plain field made the lock-free reads a
	// data race, and taking writeMu inside ConfigPath would deadlock the
	// callers that already hold it — so the value carries its own
	// synchronisation instead.
	workspacePath csync.Value[string]

	// debugOverride is the process's --debug flag, recorded once by Load so
	// that buildConfig can reapply it on every reload. Options.Debug is
	// otherwise sourced only from the "debug" key in a config file, which a
	// reload's fresh disk read would otherwise silently drop back to false.
	// See buildConfig's reapplication of it. Read and written only from
	// Load/reloadFromDisk, both of which already run
	// single-threaded with respect to this field (Load before the store is
	// published; reloadFromDisk under reloadMu), so no separate mutex.
	debugOverride  bool
	loadedPaths    []string // config files that were successfully loaded
	knownProviders []catwalk.Provider
	overrides      RuntimeOverrides

	// staleness owns tracked paths and their on-disk snapshots. It remains
	// separate from writeMu because diagnostics and watcher polling read it
	// without blocking config publication.
	staleness fileStaleness

	// configMu guards the config pointer field against concurrent
	// readers (Config) and the writeMu-serialised swap (setConfig). It
	// protects the pointer word only; the pointed-to Config is treated
	// as immutable once published, since both reloads and typed mutators
	// build a fresh Config rather than mutating the live one.
	configMu          sync.RWMutex
	version           atomic.Uint64
	credentialVersion atomic.Uint64
	mcpMutationEpochs map[string]uint64

	// file serialises config file writes (both the in-process mutex and
	// the cross-process flock); see configFile's doc comment for how it
	// nests under writeMu.
	file configFile

	writeMu  sync.RWMutex // serialises in-memory config production (mutators + the reload swap); RLock for readers
	reloadMu sync.Mutex   // serialises reload attempts against each other; see the ConfigStore doc comment

	// watcher owns polling configuration, callback, and agent-directory
	// snapshots. It does not own reload or config publication.
	watcher         externalChangeWatcher
	inheritedAgents map[string]Agent
}

// Config returns the pure-data config struct (read-only after load).
//
// The pointer read is guarded by configMu so it can never tear against
// the reload swap in reloadFromDisk. Reloads build a brand-new
// Config and swap it in rather than mutating the live one, so holding the
// returned pointer stays safe even across a concurrent reload — the reader
// keeps reading its (now immutable) snapshot.
func (s *ConfigStore) Config() *Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

// setConfig atomically swaps the active config pointer under configMu.
// Used by the reload path; in-place field mutators leave the pointer
// untouched and run under mu instead.
//
// Freezing Providers and RuntimeProviders here, before the pointer becomes
// visible to readers, is what makes "a published Config is never mutated in
// place" (see cloneForWrite's doc comment) a property enforced by the type
// rather than a convention every caller has to remember: any further
// Set/Del/Reset/Take on either map now panics. cloneForWrite always hands
// back fresh, unfrozen *csync.Maps, so the next mutation cycle's clone is
// writable right up until it too is published through here.
func (s *ConfigStore) setConfig(cfg *Config) {
	if cfg != nil && cfg.Providers != nil {
		cfg.Providers.Freeze()
	}
	if cfg != nil && cfg.RuntimeProviders != nil {
		cfg.RuntimeProviders.Freeze()
	}
	s.configMu.Lock()
	s.config = cfg
	s.version.Add(1)
	s.configMu.Unlock()
}

func (s *ConfigStore) Version() uint64 {
	return s.version.Load()
}

// RuntimeSnapshot is one published runtime configuration generation.
type RuntimeSnapshot struct {
	Config      *Config
	Resolver    VariableResolver
	WorkingDir  string
	Overrides   RuntimeOverrides
	LoadedPaths []string
	Staleness   StalenessResult
}

// RuntimeSnapshot captures config, resolver, and publication metadata under
// the lock that serializes every in-memory publication.
func (s *ConfigStore) RuntimeSnapshot() RuntimeSnapshot {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()

	cfg := s.Config()
	return RuntimeSnapshot{
		Config:      cfg,
		Resolver:    cfg.RuntimeResolver(),
		WorkingDir:  s.workingDir,
		Overrides:   cloneRuntimeOverrides(s.overrides),
		LoadedPaths: slices.Clone(s.loadedPaths),
		Staleness:   s.ConfigStaleness(),
	}
}

func (s *ConfigStore) CredentialVersion() uint64 {
	return s.credentialVersion.Load()
}

// WorkingDir returns the current working directory.
func (s *ConfigStore) WorkingDir() string {
	return s.workingDir
}

// Resolver returns the variable resolver.
func (s *ConfigStore) Resolver() VariableResolver {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.resolver
}

// Resolve resolves a variable reference using the configured resolver.
func (s *ConfigStore) Resolve(key string) (string, error) {
	s.writeMu.RLock()
	r := s.resolver
	s.writeMu.RUnlock()
	if r == nil {
		return "", fmt.Errorf("no variable resolver configured")
	}
	return r.ResolveValue(key)
}

// KnownProviders returns the list of known providers.
func (s *ConfigStore) KnownProviders() []catwalk.Provider {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.knownProviders
}

// SetupAgents configures the coder and task agents on the config.
//
// This method is intended for use during store construction (Load) and for
// test/bootstrap setup. Production runtime must not call it on a store
// whose Config is already accessible to other goroutines, since it mutates
// the Config in place.
func (s *ConfigStore) SetupAgents() {
	s.Config().SetupAgents()
}

// SetupAgentsWithInherited configures agents during bootstrap with inherited
// user-defined definitions as the lowest-priority source.
func (s *ConfigStore) SetupAgentsWithInherited(inherited map[string]Agent) {
	s.inheritedAgents = cloneAgents(inherited)
	s.Config().SetupAgentsWithInherited(s.inheritedAgents)
}

// ReplaceInheritedAgents swaps the inherited user-defined agents of a
// running store — the parent workspace re-discovered its agents (an edited
// .sennit/agents/*.md) and is pushing the new set into a live thread. It
// publishes a fresh Config snapshot with the agents rebuilt, bumping the
// store version so a runtime compiled against the old set (the thread's
// coder and its per-agent delegation tools, each pinned to the model the
// agent named when it was built) is recompiled before its next turn.
// Without this a thread kept the agents it was born with until it
// finished, so an agent whose model was changed in the parent kept
// delegating on the old one — while the parent's own config, and anything
// reading it, already showed the new.
//
// Unlike SetupAgentsWithInherited (bootstrap, before the Config is shared)
// this never mutates the published Config in place: it clones, rebuilds
// agents on the clone, and swaps under writeMu like every other runtime
// mutator, so concurrent readers keep their immutable snapshot. The
// inherited set is also stored for the thread's own later reloads, which
// re-apply it as the lowest-priority source.
func (s *ConfigStore) ReplaceInheritedAgents(inherited map[string]Agent) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.inheritedAgents = cloneAgents(inherited)
	// cloneForWrite copies Problems, which setupAgents rewrites in place.
	nc := s.Config().cloneForWrite()
	nc.SetupAgentsWithInherited(s.inheritedAgents)
	s.setConfig(nc)
}

// Overrides returns a copy of the runtime overrides for this store. It is a
// value, not a pointer: s.overrides is mutated under writeMu by concurrent
// setters (SetSkipPermissionRequests, SetEnabledChannels,
// pinPreferredModelLocked) and by reloadFromDisk, so handing out a pointer
// let callers write fields with no lock at all. Use the Set* methods to
// mutate.
func (s *ConfigStore) Overrides() RuntimeOverrides {
	return s.snapshotOverrides()
}

// SetSkipPermissionRequests sets the SkipPermissionRequests runtime override
// under writeMu so it cannot race a concurrent reload's swap or another
// mutator.
func (s *ConfigStore) SetSkipPermissionRequests(skip bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.overrides.SkipPermissionRequests = skip
}

// SetEnabledChannels sets the EnabledChannels runtime override under
// writeMu; see SetSkipPermissionRequests.
func (s *ConfigStore) SetEnabledChannels(channels []string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.overrides.EnabledChannels = slices.Clone(channels)
}

// LoadedPaths returns the config file paths that were successfully loaded.
func (s *ConfigStore) LoadedPaths() []string {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return slices.Clone(s.loadedPaths)
}

// atomicWrite handles the lock-read-transform-write-unlock cycle for
// config file mutations. The fn callback receives the current file
// contents (raw bytes, or {} if the file is missing) and must return the
// new contents. fn must be pure — no I/O, no network calls.
//
// Callers that need to do I/O between reading and writing (e.g. an HTTP
// token exchange) must resolve the path via ConfigPath and lock it with
// s.file.lock explicitly rather than going through atomicWrite.
func (s *ConfigStore) atomicWrite(scope Scope, fn func(current []byte) ([]byte, error)) error {
	ctx, cancel := configWriteContext()
	defer cancel()
	return s.atomicWriteContext(ctx, scope, fn)
}

func (s *ConfigStore) atomicWriteContext(ctx context.Context, scope Scope, fn func(current []byte) ([]byte, error)) error {
	path, err := s.ConfigPath(scope)
	if err != nil {
		return err
	}
	return s.file.atomicWrite(ctx, path, fn)
}

// RemoveRuntimeConfigField deletes key from the runtime config files that
// apply to scope.
//
// ScopeGlobal deletes from every global layer, not just the one
// ConfigPath would resolve: a stale key (an expired provider credential,
// say) can sit in any of them, and leaving a copy behind in a
// higher-priority layer would resurrect it on the next load. Any other
// scope resolves its single file the same way every other mutator does,
// through ConfigPath.
//
// Like SetConfigFields, the writes and the staleness-snapshot refresh happen
// under one fileStaleness mutex section so a concurrent ConfigStaleness() cannot
// mistake one of these writes for an external change. See SetConfigFields
// for the full rationale. Unlike SetConfigFields, this deliberately skips
// autoReload: these are "runtime" writes callers do not want reflected back
// into in-memory config.
func (s *ConfigStore) RemoveRuntimeConfigField(scope Scope, key string) {
	paths := globalConfigPaths()
	if scope != ScopeGlobal {
		path, err := s.ConfigPath(scope)
		if err != nil {
			slog.Warn("Failed to remove runtime config field: no config file for this scope",
				"key", key, "error", err)
			return
		}
		paths = []string{path}
	}
	// The writes and the staleness-snapshot refresh happen under one
	// fileStaleness.withWrite section; see its doc comment for why.
	_ = s.staleness.withWrite(func() (bool, error) {
		var wrote bool
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				continue
			}
			ctx, cancel := configWriteContext()
			err := s.file.atomicWrite(ctx, path, func(data []byte) ([]byte, error) {
				if !gjson.GetBytes(data, key).Exists() {
					return nil, errAtomicWriteNoop
				}
				value, err := sjson.Delete(string(data), key)
				if err != nil {
					return nil, fmt.Errorf("failed to delete config field %s: %w", key, err)
				}
				return []byte(value), nil
			})
			cancel()
			if err != nil {
				slog.Warn("Failed to remove runtime config field", "key", key, "path", path, "error", err)
			} else {
				wrote = true
			}
		}
		return wrote, nil
	})
}

// WriteRuntimeConfigFields writes fields to the config file for the given
// scope without reloading in-memory state. See RemoveRuntimeConfigField for
// why the staleness snapshot is still refreshed under fileStaleness mutex.
//
// Not part of the RuntimeStore interface: providerload, its only would-be
// caller, only ever removes fields. This method is kept on *ConfigStore
// anyway because TestWatchForExternalChanges_IgnoresOwnRuntimeConfigWrites
// (watch_test.go) is the only test proving that a runtime write — one
// that, unlike SetConfigFields, skips autoReload — still refreshes the
// staleness snapshot before releasing the write lock; without that, a
// watcher poll landing right after could misread this process's own
// runtime write as an external change.
func (s *ConfigStore) WriteRuntimeConfigFields(scope Scope, fields map[string]any) {
	// The write and the staleness-snapshot refresh happen under one
	// fileStaleness.withWrite section; see its doc comment for why.
	err := s.staleness.withWrite(func() (bool, error) {
		err := s.writeConfigFields(scope, fields)
		return err == nil, err
	})
	if err != nil {
		slog.Warn("Failed to write runtime config fields", "error", err)
	}
}

// ConfigPath returns the file path for the given scope.
func (s *ConfigStore) ConfigPath(scope Scope) (string, error) {
	switch scope {
	case ScopeWorkspace:
		if path := s.workspacePath.Get(); path != "" {
			return path, nil
		}
		return "", ErrNoWorkspaceConfig
	default:
		if s.globalDataPath == "" {
			return "", ErrNoGlobalConfig
		}
		return s.globalDataPath, nil
	}
}

// ProviderFieldKey builds a "providers.<id>.<suffix>" gjson/sjson path for a
// dynamic, user-supplied provider ID. Custom provider IDs are free text (a
// domain-style ID like "api.example.com" is common), but sjson/gjson paths
// use '.' (and a handful of other characters) as path separators, so an
// unescaped ID splits into nested path segments instead of naming one
// literal "providers" key — silently writing (and later reading back
// nothing for) the wrong location. gjson.Escape backslash-escapes those
// metacharacters so the ID round-trips as a single literal key.
func ProviderFieldKey(providerID, suffix string) string {
	return fmt.Sprintf("providers.%s.%s", gjson.Escape(providerID), suffix)
}

// HasConfigField checks whether a key exists in the config file for the given
// scope.
func (s *ConfigStore) HasConfigField(scope Scope, key string) bool {
	path, err := s.ConfigPath(scope)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return gjson.Get(string(data), key).Exists()
}

// SetConfigField sets a key/value pair in the config file for the given scope.
// After a successful write, it automatically reloads config to keep in-memory
// state fresh.
func (s *ConfigStore) SetConfigField(scope Scope, key string, value any) error {
	return s.SetConfigFields(scope, map[string]any{key: value})
}

// SetConfigFields sets multiple key/value pairs in the config file for the
// given scope in a single write, then reloads in-memory state from disk.
//
// Use this for arbitrary external edits where the in-memory effect of the
// change is not known ahead of time. The typed mutators (which know exactly
// what changed) go through update instead and skip the reload.
//
// The write is protected by an in-process mutex and a cross-process flock
// to prevent races between concurrent writers in different processes.
func (s *ConfigStore) SetConfigFields(scope Scope, kv map[string]any) error {
	// The write and the staleness-snapshot refresh happen under one
	// fileStaleness.withWrite section; see its doc comment for why the lock
	// must span both halves: splitting them lets a poll land between the
	// write and the snapshot refresh and misread this process's own write
	// as an external change — see TestWatchForExternalChanges_IgnoresOwnWrites
	// and *_TightPoll.
	//
	// The cost of closing the window this way: ConfigStaleness() now
	// waits behind a config write, including its cross-process flock.
	// That wait is bounded by configLockDeadline (5s) and only occurs
	// when another process is mid-write, so the worst case is a
	// delayed watcher poll or a slow sennit_info -- deliberately
	// preferred over reporting this process's own write as external.
	err := s.staleness.withWrite(func() (bool, error) {
		err := s.writeConfigFields(scope, kv)
		return err == nil, err
	})
	if err != nil {
		return err
	}
	// Auto-reload to keep in-memory state fresh after config edits.
	// We use context.Background() since this is an internal operation that
	// shouldn't be cancelled by user context.
	if err := s.autoReload(context.Background()); err != nil {
		// Log warning but don't fail the write - disk is already updated.
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}
	return nil
}

// writeConfigFields persists key/value pairs to the config file. It does not
// touch in-memory config state or the staleness snapshot: callers either
// reload (SetConfigFields, whose reload recaptures the snapshot) or have
// already published an updated clone and capture the snapshot themselves
// (update). Both of those run under writeMu, which is what keeps the
// snapshot map free of concurrent writers.
func (s *ConfigStore) writeConfigFields(scope Scope, kv map[string]any) error {
	// Sort keys for deterministic output regardless of map iteration
	// order. This also ensures consistent results when callers pass
	// overlapping JSONPath keys (e.g. "a" and "a.b").
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return s.atomicWrite(scope, func(data []byte) ([]byte, error) {
		v := string(data)
		for _, key := range keys {
			var sErr error
			if v, sErr = sjson.Set(v, key, kv[key]); sErr != nil {
				return nil, fmt.Errorf("failed to set config field %s: %w", key, sErr)
			}
		}
		return []byte(v), nil
	})
}

// mutateInMemory applies a copy-on-write change to the config without
// persisting. Under writeMu it clones the live config, lets mutate edit the
// clone, and publishes it. This is the single primitive every in-memory
// config change goes through, so a published Config is never mutated in
// place and readers always see a consistent snapshot.
func (s *ConfigStore) mutateInMemory(mutate func(*Config)) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	nc := s.Config().cloneForWrite()
	mutate(nc)
	s.setConfig(nc)
}

// update applies a copy-on-write change and persists the reported fields.
// mutate edits the clone and returns the JSON-path fields to write to disk.
// The clone is published only after persistence succeeds, so a disk error
// leaves the live config and its version unchanged. Returning an empty map
// publishes the clone without a disk write.
func (s *ConfigStore) update(scope Scope, mutate func(*Config) map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.updateLocked(scope, mutate)
}

// updateLocked is the lock-free core of update. Caller must hold writeMu.
func (s *ConfigStore) updateLocked(scope Scope, mutate func(*Config) map[string]any) error {
	return s.updateLockedErr(scope, func(cfg *Config) (map[string]any, error) {
		return mutate(cfg), nil
	})
}

// updateLockedErr is the transactional variant used when preparing a mutation
// can fail. Caller must hold writeMu.
func (s *ConfigStore) updateLockedErr(scope Scope, mutate func(*Config) (map[string]any, error)) error {
	nc := s.Config().cloneForWrite()
	fields, err := mutate(nc)
	if err != nil {
		return err
	}
	// Load returns early — without SetupAgents — when no provider is
	// configured yet (fresh install). The first mutation that makes the
	// config usable (onboarding writing a provider key / preferred model)
	// must therefore build the agents map itself, or InitCoderAgent finds
	// no "coder" agent right after onboarding. Doing it here, on the
	// not-yet-published clone, keeps the published-Config-is-immutable
	// invariant; steady-state mutations skip it (Agents already built).
	// The Options guard skips hand-rolled test fixtures whose Config never
	// went through Load's defaulting (SetupAgents dereferences Options).
	if len(nc.Agents) == 0 && nc.Options != nil && nc.IsConfigured() {
		nc.SetupAgents()
	}
	if len(fields) == 0 {
		s.setConfig(nc)
		return nil
	}
	// The write and the staleness-snapshot refresh happen under one
	// fileStaleness.withWriteAddPath section, exactly as SetConfigFields'
	// withWrite does; see fileStaleness.withWrite for the full rationale.
	//
	// addAndRefreshLocked never narrows the tracked set to just the paths
	// that happened to load: it restats every path Load/reloadFromDisk
	// already tracked (including global layers absent on disk, so a global
	// config appearing for the first time still counts as an external
	// change) and only adds the scope's own path if it is somehow not
	// already a member.
	path, pathErr := s.ConfigPath(scope)
	if pathErr != nil {
		path = ""
	}
	err = s.staleness.withWriteAddPath(path, func() (bool, error) {
		err := s.writeConfigFields(scope, fields)
		return err == nil, err
	})
	if err != nil {
		return err
	}
	s.setConfig(nc)
	return nil
}

// OverridePreferredModel sets the preferred model in memory only, without
// persisting. It is for per-run overrides (such as the non-interactive
// --model flag) that must not be written to the user's config file.
func (s *ConfigStore) OverridePreferredModel(model SelectedModel) {
	s.mutateInMemory(func(c *Config) {
		c.Model = model
		s.pinPreferredModelLocked(model)
	})
}

// pinPreferredModelLocked records the model this instance is running so
// that a later config reload cannot replace it with a choice made
// somewhere else. Several Sennit instances share one global config file, so
// a reload triggered by an unrelated write (a token refresh, say) would
// otherwise import whichever model a sibling instance last selected and
// switch models out from under the user mid-session — including to one
// whose provider this instance does not have, which fails the session
// outright.
//
// Load pins the startup model too, not just explicit selections, so an
// instance quietly running on the config's default is protected as well.
// The pin lives and dies with the process, so the next start still reads
// the file.
//
// Caller must hold writeMu.
func (s *ConfigStore) pinPreferredModelLocked(model SelectedModel) {
	m := model
	s.overrides.Model = &m
}

// RemoveConfigField removes a key from the config file for the given scope.
// After a successful write, it automatically reloads config to keep in-memory
// state fresh.
//
// ScopeGlobal deletes from every global layer, not just the one ConfigPath
// would resolve: a global config is four layers (see globalConfigPaths), and
// a key set by hand in ~/.config/sennit/sennit.json — the documented place
// for it — would otherwise survive a "clear this setting" call made against
// the data file alone, then come back on the next reload. See
// RemoveRuntimeConfigField, which already does this for the same reason; any
// other scope resolves its single file the same way every other mutator
// does, through ConfigPath.
//
// The write is protected by an in-process mutex and a cross-process flock.
//
// Like SetConfigFields, the write(s) and the staleness-snapshot refresh
// happen under one fileStaleness.withWrite section; see its doc comment for
// the full rationale.
func (s *ConfigStore) RemoveConfigField(scope Scope, key string) error {
	if scope != ScopeGlobal {
		err := s.staleness.withWrite(func() (bool, error) {
			err := s.atomicWrite(scope, func(data []byte) ([]byte, error) {
				v, sErr := sjson.Delete(string(data), key)
				if sErr != nil {
					return nil, fmt.Errorf("failed to delete config field %s: %w", key, sErr)
				}
				return []byte(v), nil
			})
			return err == nil, err
		})
		if err != nil {
			return err
		}

		if err := s.autoReload(context.Background()); err != nil {
			slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
		}

		return nil
	}

	// One of the four global layers is a sennitrc sibling — a bash script,
	// not JSON. sjson.Delete would happily mangle it, so every write here
	// goes through the same errAtomicWriteNoop guard
	// RemoveRuntimeConfigField uses: gjson never matches inside a shell
	// file, so that layer is always skipped rather than rewritten.
	//
	// s.globalDataPath is unioned in explicitly, not assumed to already be
	// globalConfigPaths()'s GlobalConfigData() entry: production always
	// sets it to exactly that (see Load), but a store built directly
	// against a stand-in data path (test doubles that never call Load)
	// still deserves ConfigPath(ScopeGlobal)'s own file cleared — that is
	// the one layer this method's doc comment promises is never skipped.
	paths := globalConfigPaths()
	if s.globalDataPath != "" && !slices.Contains(paths, s.globalDataPath) {
		paths = append(paths, s.globalDataPath)
	}
	var wrote bool
	err := s.staleness.withWrite(func() (bool, error) {
		var errs []error
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				continue // layer not present on this machine; nothing to clear
			}
			// atomicWrite returns nil for errAtomicWriteNoop (configfile.go),
			// so a plain err == nil check cannot tell "this layer changed"
			// from "this layer never had the key" — changed is set only on
			// the branch that actually hands back new bytes, and wrote is
			// driven off that instead of off the call's error.
			var changed bool
			ctx, cancel := configWriteContext()
			err := s.file.atomicWrite(ctx, path, func(data []byte) ([]byte, error) {
				if !gjson.GetBytes(data, key).Exists() {
					return nil, errAtomicWriteNoop
				}
				v, sErr := sjson.Delete(string(data), key)
				if sErr != nil {
					return nil, fmt.Errorf("failed to delete config field %s: %w", key, sErr)
				}
				changed = true
				return []byte(v), nil
			})
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
			} else if changed {
				wrote = true
			}
		}
		return wrote, errors.Join(errs...)
	})
	if err != nil {
		return err
	}

	// Only reload when a layer actually changed: every path being a noop
	// (key never set) or absent is not a config change worth an autoReload
	// round trip, including its provider-catalog and discovery work.
	if wrote {
		if err := s.autoReload(context.Background()); err != nil {
			slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
		}
	}

	return nil
}

// UpdatePreferredModel updates the preferred model and persists it to the
// config file at the given scope. The selected model and the recent-models
// list are written together in a single config write.
//
// The write skips the full disk reparse/reload (which would rebuild the
// provider catalog and agents on every model switch and dominate selection
// latency); agents are refreshed separately by the caller (see
// UpdateAgentModel).
func (s *ConfigStore) UpdatePreferredModel(scope Scope, model SelectedModel) error {
	return s.update(scope, func(c *Config) map[string]any {
		return s.updatePreferredModelFields(c, model)
	})
}

// updatePreferredModelFields builds the fields map for persisting a preferred
// model change. Shared between UpdatePreferredModel and direct updateLocked
// callers (e.g. Load). Caller must hold writeMu.
func (s *ConfigStore) updatePreferredModelFields(c *Config, model SelectedModel) map[string]any {
	c.Model = model
	s.pinPreferredModelLocked(model)

	fields := map[string]any{
		"model": model,
	}
	if updated, changed := nextRecentModels(c, model); changed {
		c.RecentModels = updated
		fields["recent_models"] = updated
	}
	return fields
}

// SetCompactMode sets the compact mode setting and persists it.
func (s *ConfigStore) SetCompactMode(scope Scope, enabled bool) error {
	return s.update(scope, func(c *Config) map[string]any {
		c.ensureTUI().CompactMode = enabled
		return map[string]any{"options.tui.compact_mode": enabled}
	})
}

// SetTransparentBackground sets the transparent background setting and persists it.
func (s *ConfigStore) SetTransparentBackground(scope Scope, enabled bool) error {
	return s.update(scope, func(c *Config) map[string]any {
		c.ensureTUI().Transparent = &enabled
		return map[string]any{"options.tui.transparent": enabled}
	})
}

// nextRecentModels computes the recent-models list after recording the
// supplied model at the front, operating on the provided config without
// persisting anything. It returns the new slice and whether it differs from
// cfg's current list. Callers fold the result into a clone they are about
// to publish.
func nextRecentModels(cfg *Config, model SelectedModel) ([]SelectedModel, bool) {
	if model.Provider == "" || model.Model == "" {
		return nil, false
	}

	// Identity is the provider/model pair: one entry per model, however
	// many times it is picked or re-tuned.
	sameModel := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}
	// The list is also where a model's last-used reasoning effort is kept
	// (see Config.RememberedReasoningEffort), so re-tuning the model in
	// place is a change worth persisting even though the order is
	// untouched.
	same := func(a, b SelectedModel) bool {
		return sameModel(a, b) && a.ReasoningEffort == b.ReasoningEffort
	}

	entry := SelectedModel{
		Provider:        model.Provider,
		Model:           model.Model,
		ReasoningEffort: model.ReasoningEffort,
	}

	current := cfg.RecentModels
	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return sameModel(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModels {
		updated = updated[:maxRecentModels]
	}

	if slices.EqualFunc(current, updated, same) {
		return current, false
	}

	return updated, true
}
