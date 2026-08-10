package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/env"
	"github.com/rave-soft/braid/internal/lock"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/rave-soft/braid/internal/oauth/copilot"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

// configLockDeadline bounds how long lockConfig waits for the
// cross-process flock before giving up. A few seconds is plenty for
// honest contention; longer suggests something is wedged.
const configLockDeadline = 5 * time.Second

// refreshLockDeadline bounds how long RefreshOAuthToken waits for the
// per-provider cross-process refresh lock. It must exceed the token
// exchange HTTP timeout (30s) so that a peer mid-exchange is given time
// to finish and publish its result, which we then adopt instead of
// running our own exchange. Running our own would reuse an
// already-rotated refresh token and trip the provider's reuse detection,
// revoking the whole token family.
const refreshLockDeadline = 45 * time.Second

// credentialWriteLockDeadline bounds how long a credential write (e.g.
// storing the token from a fresh interactive login) waits for the
// per-provider refresh lock. It is deliberately shorter than
// refreshLockDeadline because a user is watching: if a peer is wedged we
// would rather write and risk a rare clobber than hang the UI.
const credentialWriteLockDeadline = 10 * time.Second

// fileSnapshot captures metadata about a config file at a point in time.
type fileSnapshot struct {
	Path    string
	Exists  bool
	Size    int64
	ModTime int64 // UnixNano
}

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
	// Model records the model choice made in this instance, whether
	// persisted or not. It is reapplied after a config reload so that a
	// selection made here always outranks whatever the shared config file
	// happens to hold — see pinPreferredModelLocked. Nil means this instance
	// never chose a model, so a reload should follow whatever is on disk.
	Model *SelectedModel
}

// ConfigStore is the single entry point for all config access. It owns the
// pure-data Config, runtime state (working directory, resolver, known
// providers), and persistence to both global and workspace config files.
//
// mu serialises all config file mutations (SetConfigFields,
// RemoveConfigField, RefreshOAuthToken) to prevent both in-process
// goroutine races and, together with the shared lock.File, cross-process
// races on the config file.
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
	config             *Config
	workingDir         string
	resolver           VariableResolver
	globalDataPath     string   // ~/.local/share/braid/braid.json
	workspacePath      string   // .braid/braid.json
	loadedPaths        []string // config files that were successfully loaded
	knownProviders     []catwalk.Provider
	overrides          RuntimeOverrides
	trackedConfigPaths []string                // unique, normalized config file paths
	snapshots          map[string]fileSnapshot // path -> snapshot at last capture

	// stalenessMu guards trackedConfigPaths and snapshots. Writers
	// (CaptureStalenessSnapshot, RefreshStalenessSnapshot) already run
	// under writeMu via updateLocked/reloadFromDisk, but ConfigStaleness
	// is a read-only diagnostic called from other goroutines without
	// writeMu (the braid_info tool, and WatchForExternalChanges' poll
	// loop in watch.go) — a separate mutex, rather than reusing writeMu,
	// keeps that read cheap and avoids adding staleness bookkeeping to
	// writeMu's contention.
	stalenessMu sync.Mutex

	// configMu guards the config pointer field against concurrent
	// readers (Config) and the writeMu-serialised swap (setConfig). It
	// protects the pointer word only; the pointed-to Config is treated
	// as immutable once published, since both reloads and typed mutators
	// build a fresh Config rather than mutating the live one.
	configMu sync.RWMutex

	mu       sync.Mutex   // serialises config file writes
	writeMu  sync.RWMutex // serialises in-memory config production (mutators + the reload swap); RLock for readers
	reloadMu sync.Mutex   // serialises reload attempts against each other; see the ConfigStore doc comment

	// refreshSF collapses concurrent in-process OAuth refreshes for the
	// same provider into a single attempt. Combined with the per-provider
	// cross-process refresh lock, it ensures only one token exchange runs
	// at a time. See RefreshOAuthToken.
	refreshSF singleflight.Group

	// exchangeToken performs the provider-specific OAuth token exchange.
	// It is a field so tests can substitute a fake exchange without making
	// real network calls. Production code leaves it nil, and exchange falls
	// back to the real provider clients.
	exchangeToken func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)

	// authSignalMu guards authSignals, which maps provider IDs to
	// channels that WaitForTokenChange blocks on. SignalAuthComplete
	// closes the channel to unblock waiters; a new channel is created
	// on the next wait.
	authSignalMu sync.Mutex
	authSignals  map[string]chan struct{}

	// onExternalChangeMu guards onExternalChange, the callback
	// WatchForExternalChanges runs after a reload it triggered itself
	// (as opposed to one caused by this process's own writes). See
	// watch.go.
	onExternalChangeMu sync.Mutex
	onExternalChange   func()
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
func (s *ConfigStore) setConfig(cfg *Config) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = cfg
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
func (s *ConfigStore) SetupAgents() {
	s.Config().SetupAgents()
}

// Overrides returns the runtime overrides for this store.
func (s *ConfigStore) Overrides() *RuntimeOverrides {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return &s.overrides
}

// LoadedPaths returns the config file paths that were successfully loaded.
func (s *ConfigStore) LoadedPaths() []string {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return slices.Clone(s.loadedPaths)
}

// lockConfig acquires both the in-process mutex and a cross-process flock
// on the config file for the given scope. Callers that need to do I/O
// between reading and writing (e.g. an HTTP token exchange) must use
// lockConfig explicitly rather than atomicWrite.
//
// The returned release function drops both locks. Callers must call it
// as soon as the file access is complete — no I/O should be performed
// while the lock is held.
func (s *ConfigStore) lockConfig(scope Scope) (func(), error) {
	s.mu.Lock()
	path, err := s.configPath(scope)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), configLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("acquire config lock: %w", err)
	}
	return func() {
		release()
		s.mu.Unlock()
	}, nil
}

// atomicWrite handles the lock-read-transform-write-unlock cycle for
// config file mutations. The fn callback receives the current file
// contents (raw bytes, or {} if the file is missing) and must return the
// new contents. fn must be pure — no I/O, no network calls.
func (s *ConfigStore) atomicWrite(scope Scope, fn func(current []byte) ([]byte, error)) error {
	unlock, err := s.lockConfig(scope)
	if err != nil {
		return err
	}
	defer unlock()

	path, err := s.configPath(scope)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte("{}")
		} else {
			return fmt.Errorf("read config file: %w", err)
		}
	}

	newData, err := fn(data)
	if err != nil {
		return err
	}

	return atomicWriteFile(path, newData, 0o600)
}

// configPath returns the file path for the given scope.
func (s *ConfigStore) configPath(scope Scope) (string, error) {
	switch scope {
	case ScopeWorkspace:
		if s.workspacePath == "" {
			return "", ErrNoWorkspaceConfig
		}
		return s.workspacePath, nil
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
	path, err := s.configPath(scope)
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
	if err := s.writeConfigFields(scope, kv); err != nil {
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
// mutate edits the clone and returns the JSON-path fields to write to disk;
// because the clone already reflects the change, no reload is needed.
// Returning an empty map publishes the clone without a disk write.
func (s *ConfigStore) update(scope Scope, mutate func(*Config) map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.updateLocked(scope, mutate)
}

// updateLocked is the lock-free core of update. Caller must hold writeMu.
func (s *ConfigStore) updateLocked(scope Scope, mutate func(*Config) map[string]any) error {
	nc := s.Config().cloneForWrite()
	fields := mutate(nc)
	s.setConfig(nc)
	if len(fields) == 0 {
		return nil
	}
	if err := s.writeConfigFields(scope, fields); err != nil {
		return err
	}
	// Refresh the staleness snapshot so the file watcher does not treat
	// our own write as an external change. Safe to touch the snapshot map
	// here because we hold writeMu.
	if path, err := s.configPath(scope); err == nil {
		s.captureStalenessSnapshot(append(slices.Clone(s.loadedPaths), path))
	}
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

// pinPreferredModelLocked records a model choice made in this instance so
// that a later config reload cannot replace it with a choice made
// somewhere else. Several Braid instances share one global config file, so
// a reload triggered by an unrelated write (a token refresh, say) would
// otherwise import whichever model a sibling instance last selected and
// switch models out from under the user mid-session.
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
// The write is protected by an in-process mutex and a cross-process flock.
func (s *ConfigStore) RemoveConfigField(scope Scope, key string) error {
	err := s.atomicWrite(scope, func(data []byte) ([]byte, error) {
		v, sErr := sjson.Delete(string(data), key)
		if sErr != nil {
			return nil, fmt.Errorf("failed to delete config field %s: %w", key, sErr)
		}
		return []byte(v), nil
	})
	if err != nil {
		return err
	}

	if err := s.autoReload(context.Background()); err != nil {
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
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

// isCatalogProvider reports whether providerID is present in the embedded
// provider catalog. Only providers outside the catalog need their identity
// fields (type/base_url/name) persisted alongside credentials: a catalog
// provider's identity is reconstructed from the embedded list on every
// reload (see configureProviders), and pinning its base_url in the user's
// config file would freeze it against future catalog updates.
func (s *ConfigStore) isCatalogProvider(providerID string) bool {
	return s.findKnownProvider(providerID) != nil
}

// findKnownProvider looks up providerID in the embedded provider catalog,
// returning nil when the provider is not catalog-known (e.g. a custom
// OAuth provider the user configured by hand).
func (s *ConfigStore) findKnownProvider(providerID string) *catwalk.Provider {
	for _, p := range s.knownProviders {
		if string(p.ID) == providerID {
			return &p
		}
	}
	return nil
}

// SetProviderAPIKey sets the API key for a provider and persists it.
//
// Validation and the full providerConfig are assembled entirely in memory
// before anything is written to disk, so an unknown provider ID leaves no
// trace on disk (previously the api_key/oauth write for the string/token
// case happened first, and only then did the provider lookup that could
// fail).
func (s *ConfigStore) SetProviderAPIKey(scope Scope, providerID string, apiKey any) error {
	cfg := s.Config()
	providerConfig, exists := cfg.Providers.Get(providerID)
	if !exists {
		foundProvider := s.findKnownProvider(providerID)
		if foundProvider == nil {
			return fmt.Errorf("provider with ID %s not found in known providers", providerID)
		}
		providerConfig = ProviderConfig{
			ID:           providerID,
			Name:         foundProvider.Name,
			BaseURL:      foundProvider.APIEndpoint,
			Type:         foundProvider.Type,
			Disable:      false,
			ExtraHeaders: make(map[string]string),
			ExtraParams:  make(map[string]string),
			Models:       foundProvider.Models,
		}
	}

	fields := map[string]any{}
	var isToken bool

	switch v := apiKey.(type) {
	case string:
		providerConfig.APIKey = v
		fields[ProviderFieldKey(providerID, "api_key")] = v
	case *oauth.Token:
		isToken = true
		providerConfig.APIKey = v.AccessToken
		providerConfig.OAuthToken = v
		if providerID == string(catwalk.InferenceProviderCopilot) {
			providerConfig.SetupGitHubCopilot()
		}
		fields[ProviderFieldKey(providerID, "api_key")] = v.AccessToken
		fields[ProviderFieldKey(providerID, "oauth")] = v
	default:
		return fmt.Errorf("unsupported credential type %T for provider %s", apiKey, providerID)
	}

	// Custom providers outside the embedded catalog have nothing else to
	// reconstruct their identity from on reload, so persist it alongside
	// the credential. Catalog providers get it from the embedded list.
	if !s.isCatalogProvider(providerID) {
		fields[ProviderFieldKey(providerID, "type")] = providerConfig.Type
		fields[ProviderFieldKey(providerID, "base_url")] = providerConfig.BaseURL
		fields[ProviderFieldKey(providerID, "name")] = providerConfig.Name
	}

	// Providers is a shared, thread-safe map (csync.Map) referenced by
	// every Config generation, so setting it here is immediately visible
	// without going through the clone-and-swap mutators. update() below
	// only needs to persist fields, not restate the in-memory change.
	persist := func() error {
		cfg.Providers.Set(providerID, providerConfig)
		return s.update(scope, func(*Config) map[string]any { return fields })
	}

	if isToken {
		// Hold the refresh lock across the write so a peer's in-flight
		// token exchange cannot land on top of a credential the user just
		// obtained interactively — which would silently invalidate the
		// login they only just completed.
		if err := s.withRefreshLock(providerID, persist); err != nil {
			return fmt.Errorf("failed to save credentials to config file: %w", err)
		}
		return nil
	}

	if err := persist(); err != nil {
		return fmt.Errorf("failed to save api key to config file: %w", err)
	}
	return nil
}

// RefreshOAuthToken refreshes the OAuth token for the given provider.
//
// Providers like Hyper rotate refresh tokens: each exchange consumes the
// caller's refresh token, issues a new pair, and revokes the old one. If
// two braid instances (or two goroutines) refresh concurrently with the
// same stored refresh token, the second exchange reuses an already-revoked
// token, trips the provider's reuse detection, and revokes the entire
// token family — leaving both with dead tokens even though each refresh
// "succeeded".
//
// To prevent that, refreshes are single-flighted at two levels:
//
//   - In-process: refreshSF collapses concurrent goroutines for the same
//     provider into one attempt.
//   - Cross-process: a per-provider advisory lock is held across the whole
//     read-decide-exchange-write cycle, so only one process exchanges at a
//     time. A process that acquires the lock after a peer rotated finds the
//     peer's fresh token on disk and adopts it instead of exchanging.
func (s *ConfigStore) RefreshOAuthToken(ctx context.Context, scope Scope, providerID string) error {
	key := fmt.Sprintf("%d\x00%s", scope, providerID)
	_, err, _ := s.refreshSF.Do(key, func() (any, error) {
		return nil, s.refreshOAuthTokenLocked(ctx, scope, providerID)
	})
	return err
}

// refreshOAuthTokenLocked performs the cross-process single-flighted
// refresh. It is invoked through refreshSF, so at most one goroutine per
// provider runs it at a time within this process.
func (s *ConfigStore) refreshOAuthTokenLocked(ctx context.Context, scope Scope, providerID string) error {
	cfg := s.Config()
	providerConfig, exists := cfg.Providers.Get(providerID)
	if !exists {
		return fmt.Errorf("provider %s not found", providerID)
	}
	if providerConfig.OAuthToken == nil {
		return fmt.Errorf("provider %s does not have an OAuth token", providerID)
	}
	entryToken := providerConfig.OAuthToken

	// Acquire the per-provider cross-process refresh lock. This is a
	// dedicated lock file, not the config-write lock, and it does not take
	// s.mu — so the network exchange below cannot stall unrelated config
	// operations. The deadline exceeds the exchange timeout so that a peer
	// mid-exchange has time to publish a token we can adopt. Lock ordering:
	// the refresh lock is always taken before the config-write lock (via
	// update, which takes writeMu then s.mu — the same nesting order
	// configureProviders already uses when it calls RemoveConfigField
	// during a reload), never the reverse, so no deadlock is possible.
	lockCtx, cancel := context.WithTimeout(ctx, refreshLockDeadline)
	defer cancel()
	release, lockErr := lock.File(lockCtx, s.refreshLockPath(providerID))
	if lockErr != nil {
		// Could not acquire the lock (peer wedged or deadline hit). Prefer a
		// usable token already on disk over forcing our own exchange, which
		// would risk reusing a rotated refresh token.
		if diskToken := s.usableDiskToken(scope, providerID, entryToken); diskToken != nil {
			slog.Warn("Refresh lock unavailable; adopting token from disk", "provider", providerID, "error", lockErr)
			s.applyToken(providerConfig, diskToken, providerID)
			return nil
		}
		return fmt.Errorf("acquire refresh lock for provider %s: %w", providerID, lockErr)
	}
	defer release()

	// Now that we hold the lock, disk is the authority on which credential
	// is current: a peer may have rotated ours away while we waited. Adopt
	// a newer token outright when it is still usable, and otherwise switch
	// to its refresh token for the exchange below. Presenting our own
	// already-rotated refresh token would trip the provider's reuse
	// detection and revoke the whole family, forcing an interactive login.
	if diskToken := s.newerDiskToken(scope, providerID, entryToken); diskToken != nil {
		if !diskToken.IsExpired() {
			slog.Info("Adopting token refreshed by another session", "provider", providerID)
			s.applyToken(providerConfig, diskToken, providerID)
			return nil
		}
		slog.Info("Exchanging with refresh token rotated by another session", "provider", providerID)
		entryToken = diskToken
	}

	// Disk still holds our token (or no newer peer token exists) and we hold
	// the lock, so we are the sole exchanger. Perform the exchange.
	refreshedToken, refreshErr := s.exchange(ctx, providerID, entryToken.RefreshToken)
	if refreshErr != nil {
		// The exchange may have failed because a peer rotated the refresh
		// token in a window we did not cover. Re-check disk: adopt a usable
		// token, or retry once with the peer's newer refresh token.
		if diskToken := s.newerDiskToken(scope, providerID, entryToken); diskToken != nil {
			if !diskToken.IsExpired() {
				slog.Info("Adopting token refreshed by another session after exchange failure", "provider", providerID)
				s.applyToken(providerConfig, diskToken, providerID)
				return nil
			}
			slog.Info("Retrying exchange with refresh token rotated by another session", "provider", providerID)
			refreshedToken, refreshErr = s.exchange(ctx, providerID, diskToken.RefreshToken)
		}
	}
	if refreshErr != nil {
		return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", providerID, refreshErr)
	}

	slog.Info("Successfully refreshed OAuth token", "provider", providerID)
	s.applyToken(providerConfig, refreshedToken, providerID)

	fields := map[string]any{
		ProviderFieldKey(providerID, "api_key"): refreshedToken.AccessToken,
		ProviderFieldKey(providerID, "oauth"):   refreshedToken,
	}
	// Persist identity fields for providers outside the embedded catalog —
	// see isCatalogProvider. Without this, a custom OAuth provider's
	// type/base_url/name live only in memory: the next reload rebuilds
	// Config from disk (configureProviders), finds no base_url for this
	// provider, and drops it as an invalid custom provider.
	if !s.isCatalogProvider(providerID) {
		fields[ProviderFieldKey(providerID, "type")] = providerConfig.Type
		fields[ProviderFieldKey(providerID, "base_url")] = providerConfig.BaseURL
		fields[ProviderFieldKey(providerID, "name")] = providerConfig.Name
	}

	// Use update() rather than SetConfigFields: applyToken already
	// published the refreshed token in memory (Providers is a shared
	// csync.Map, visible without a clone-and-swap), so the full
	// disk-reparse-and-reload that SetConfigFields triggers is unnecessary
	// here and would discard anything else this refresh doesn't own.
	if err := s.update(scope, func(*Config) map[string]any { return fields }); err != nil {
		return fmt.Errorf("failed to persist refreshed token: %w", err)
	}
	return nil
}

// WaitForTokenChange blocks until SignalAuthComplete is called for the
// given provider or the context is cancelled. It is used by OnAuthRefresh
// callbacks to wait for interactive re-authentication to complete before
// retrying a failed request. The channel is created atomically with the
// wait registration so a concurrent SignalAuthComplete cannot miss it.
func (s *ConfigStore) WaitForTokenChange(ctx context.Context, providerID string) error {
	s.authSignalMu.Lock()
	ch, ok := s.authSignals[providerID]
	if !ok {
		ch = make(chan struct{})
		if s.authSignals == nil {
			s.authSignals = make(map[string]chan struct{})
		}
		s.authSignals[providerID] = ch
	}
	s.authSignalMu.Unlock()

	select {
	case <-ch:
		// Remove the consumed signal so a subsequent
		// SignalAuthComplete does not close an already-closed
		// channel.
		s.authSignalMu.Lock()
		if s.authSignals[providerID] == ch {
			delete(s.authSignals, providerID)
		}
		s.authSignalMu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SignalAuthComplete unblocks any goroutine waiting in WaitForTokenChange
// for the given provider. If no waiter exists yet, it pre-creates and
// immediately closes the channel so a subsequent WaitForTokenChange
// returns without blocking. This eliminates the race where the signal
// fires before the waiter registers.
func (s *ConfigStore) SignalAuthComplete(providerID string) {
	s.authSignalMu.Lock()
	defer s.authSignalMu.Unlock()
	if ch, ok := s.authSignals[providerID]; ok {
		delete(s.authSignals, providerID)
		select {
		case <-ch:
			// Already closed by a previous signal; nothing to do.
		default:
			close(ch)
		}
	} else {
		// No waiter yet. Pre-create a closed channel so the next
		// WaitForTokenChange returns immediately.
		if s.authSignals == nil {
			s.authSignals = make(map[string]chan struct{})
		}
		ch := make(chan struct{})
		close(ch)
		s.authSignals[providerID] = ch
	}
}

// newerDiskToken returns the on-disk token for the provider when it is
// newer than entryToken — i.e. another session (possibly in another
// process) has already rotated the credential. It returns nil when disk
// holds nothing newer than what we started with.
//
// Newness is judged by expiry as well as identity, so a config file that
// somehow holds an older token cannot drag us backwards. The result may
// itself be expired: providers that rotate refresh tokens invalidate ours
// the moment a peer refreshes, so the peer's refresh token is the only one
// the provider will still accept even after its access token ages out.
// Callers decide whether to adopt the token wholesale or merely borrow its
// refresh token.
func (s *ConfigStore) newerDiskToken(scope Scope, providerID string, entryToken *oauth.Token) *oauth.Token {
	diskToken, err := s.loadTokenFromDisk(scope, providerID)
	if err != nil {
		slog.Warn("Failed to read token from config file", "provider", providerID, "error", err)
		return nil
	}
	if diskToken == nil {
		return nil
	}
	if diskToken.AccessToken == entryToken.AccessToken {
		// Same token we started with; nobody rotated since.
		return nil
	}
	if diskToken.RefreshToken == "" && entryToken.RefreshToken != "" {
		// Adopting would thread us with no way to refresh later, and
		// there is nothing to borrow for an exchange.
		return nil
	}
	if diskToken.ExpiresAt < entryToken.ExpiresAt {
		// Older than ours; nothing to gain from adopting it.
		return nil
	}
	return diskToken
}

// usableDiskToken returns the on-disk token only when it is both newer
// than entryToken and still valid, meaning it can be adopted as-is with
// no exchange at all.
func (s *ConfigStore) usableDiskToken(scope Scope, providerID string, entryToken *oauth.Token) *oauth.Token {
	diskToken := s.newerDiskToken(scope, providerID, entryToken)
	if diskToken == nil || diskToken.IsExpired() {
		return nil
	}
	return diskToken
}

// exchange performs the provider-specific OAuth token exchange. Tests may
// override it via the exchangeToken field; production uses the real
// provider clients.
func (s *ConfigStore) exchange(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error) {
	if s.exchangeToken != nil {
		return s.exchangeToken(ctx, providerID, refreshToken)
	}
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		return copilot.RefreshToken(ctx, refreshToken)
	default:
		return nil, fmt.Errorf("OAuth refresh not supported for provider %s", providerID)
	}
}

// withRefreshLock runs fn while holding the per-provider cross-process
// refresh lock, so a credential write cannot interleave with a peer's
// token exchange. Acquisition is best effort: when the lock cannot be
// taken in time, fn runs anyway rather than blocking a write the user is
// waiting on.
func (s *ConfigStore) withRefreshLock(providerID string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), credentialWriteLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, s.refreshLockPath(providerID))
	if err != nil {
		slog.Warn("Writing credentials without the refresh lock", "provider", providerID, "error", err)
		return fn()
	}
	defer release()
	return fn()
}

// refreshLockPath returns the path to the per-provider cross-process refresh
// lock file. Lock files live under a dedicated locks/ subdirectory of the
// data dir so they do not clutter the config directory. The file is created
// on demand by lock.File and is never removed (flock keys on inode, not
// path).
func (s *ConfigStore) refreshLockPath(providerID string) string {
	dir := filepath.Join(filepath.Dir(s.globalDataPath), "locks")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, fmt.Sprintf("%s.refresh.lock", providerID))
}

// applyToken updates the in-memory provider config with the given token.
func (s *ConfigStore) applyToken(providerConfig ProviderConfig, token *oauth.Token, providerID string) {
	providerConfig.OAuthToken = token
	providerConfig.APIKey = token.AccessToken
	if providerID == string(catwalk.InferenceProviderCopilot) {
		providerConfig.SetupGitHubCopilot()
	}
	s.Config().Providers.Set(providerID, providerConfig)
}

// loadTokenFromDisk reads the OAuth token for the given provider from the
// config file on disk. Returns nil if the token is not found or matches the
// current in-memory token.
func (s *ConfigStore) loadTokenFromDisk(scope Scope, providerID string) (*oauth.Token, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	oauthKey := ProviderFieldKey(providerID, "oauth")
	oauthResult := gjson.Get(string(data), oauthKey)
	if !oauthResult.Exists() {
		return nil, nil
	}

	var token oauth.Token
	if err := json.Unmarshal([]byte(oauthResult.Raw), &token); err != nil {
		return nil, err
	}

	if token.AccessToken == "" {
		return nil, nil
	}

	return &token, nil
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

	eq := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}

	entry := SelectedModel{
		Provider: model.Provider,
		Model:    model.Model,
	}

	current := cfg.RecentModels
	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return eq(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModels {
		updated = updated[:maxRecentModels]
	}

	if slices.EqualFunc(current, updated, eq) {
		return current, false
	}

	return updated, true
}

// NewTestStore creates a ConfigStore for testing purposes.
func NewTestStore(cfg *Config, loadedPaths ...string) *ConfigStore {
	return &ConfigStore{
		config:      cfg,
		loadedPaths: loadedPaths,
		resolver:    NewShellVariableResolver(env.New()),
	}
}

// ImportCopilot attempts to import a GitHub Copilot token from disk.
func (s *ConfigStore) ImportCopilot() (*oauth.Token, bool) {
	if s.HasConfigField(ScopeGlobal, "providers.copilot.api_key") || s.HasConfigField(ScopeGlobal, "providers.copilot.oauth") {
		return nil, false
	}

	diskToken, hasDiskToken := copilot.RefreshTokenFromDisk()
	if !hasDiskToken {
		return nil, false
	}

	slog.Info("Found existing GitHub Copilot token on disk. Authenticating...")
	token, err := copilot.RefreshToken(context.TODO(), diskToken)
	if err != nil {
		slog.Error("Unable to import GitHub Copilot token", "error", err)
		return nil, false
	}

	// SetProviderAPIKey both applies the token in memory and persists it,
	// so a second explicit write of the same keys is unnecessary.
	if err := s.SetProviderAPIKey(ScopeGlobal, string(catwalk.InferenceProviderCopilot), token); err != nil {
		slog.Error("Unable to save GitHub Copilot token to disk", "error", err)
		return token, false
	}

	slog.Info("GitHub Copilot successfully imported")
	return token, true
}

// StalenessResult contains the result of a staleness check.
type StalenessResult struct {
	Dirty   bool
	Changed []string
	Missing []string
	Errors  map[string]error // stat errors by path
}

// ConfigStaleness checks whether any tracked config files have changed on disk
// since the last snapshot. Returns dirty=true if any files changed or went
// missing, along with sorted lists of affected paths. Stat errors are
// captured in Errors map but still treated as non-existence for dirty detection.
func (s *ConfigStore) ConfigStaleness() StalenessResult {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()

	var result StalenessResult
	result.Errors = make(map[string]error)

	for _, path := range s.trackedConfigPaths {
		snapshot, hadSnapshot := s.snapshots[path]

		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		if err != nil && !os.IsNotExist(err) {
			// Capture permission/IO errors separately from non-existence
			result.Errors[path] = err
			result.Dirty = true
		}

		if !exists {
			if hadSnapshot && snapshot.Exists {
				// File existed before but now missing
				result.Missing = append(result.Missing, path)
				result.Dirty = true
			}
			continue
		}

		// File exists now
		if !hadSnapshot || !snapshot.Exists {
			// File didn't exist before but does now
			result.Changed = append(result.Changed, path)
			result.Dirty = true
			continue
		}

		// Check for content or metadata changes
		if snapshot.Size != info.Size() || snapshot.ModTime != info.ModTime().UnixNano() {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
		}
	}

	// Sort for deterministic output
	slices.Sort(result.Changed)
	slices.Sort(result.Missing)

	return result
}

// RefreshStalenessSnapshot captures fresh snapshots of all tracked config files.
// Call this after reloading config to clear dirty state.
func (s *ConfigStore) RefreshStalenessSnapshot() error {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()
	s.refreshStalenessSnapshotLocked()
	return nil
}

// refreshStalenessSnapshotLocked is the lock-free core of
// RefreshStalenessSnapshot. Caller must hold stalenessMu.
func (s *ConfigStore) refreshStalenessSnapshotLocked() {
	if s.snapshots == nil {
		s.snapshots = make(map[string]fileSnapshot)
	}

	for _, path := range s.trackedConfigPaths {
		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		snapshot := fileSnapshot{
			Path:   path,
			Exists: exists,
		}

		if exists {
			snapshot.Size = info.Size()
			snapshot.ModTime = info.ModTime().UnixNano()
		}

		s.snapshots[path] = snapshot
	}
}

// CaptureStalenessSnapshot captures snapshots for the given paths, building the
// tracked config paths list. Paths are deduplicated and normalized.
func (s *ConfigStore) CaptureStalenessSnapshot(paths []string) {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()

	// Build unique set of normalized paths
	seen := make(map[string]struct{})
	for _, p := range paths {
		if p == "" {
			continue
		}
		// Normalize path
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		seen[abs] = struct{}{}
	}

	// Also track workspace and global config paths if set
	if s.workspacePath != "" {
		abs, err := filepath.Abs(s.workspacePath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}
	if s.globalDataPath != "" {
		abs, err := filepath.Abs(s.globalDataPath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}

	// Build sorted list for deterministic ordering
	s.trackedConfigPaths = make([]string, 0, len(seen))
	for p := range seen {
		s.trackedConfigPaths = append(s.trackedConfigPaths, p)
	}
	slices.Sort(s.trackedConfigPaths)

	// Capture initial snapshots
	s.refreshStalenessSnapshotLocked()
}

// captureStalenessSnapshot is an alias for CaptureStalenessSnapshot for internal use.
func (s *ConfigStore) captureStalenessSnapshot(paths []string) {
	s.CaptureStalenessSnapshot(paths)
}

// trackedConfigPathSet returns a copy of the currently tracked config paths
// as a set, for callers (externalChangeDetected in watch.go) that need to
// check membership without racing CaptureStalenessSnapshot/reloadFromDisk.
func (s *ConfigStore) trackedConfigPathSet() map[string]struct{} {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()
	set := make(map[string]struct{}, len(s.trackedConfigPaths))
	for _, p := range s.trackedConfigPaths {
		set[p] = struct{}{}
	}
	return set
}

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

	// Merge workspace config if present
	workspacePath := filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName))
	if wsData, err := os.ReadFile(workspacePath); err == nil && len(wsData) > 0 {
		if !json.Valid(wsData) {
			return fmt.Errorf("invalid JSON in config file %s", workspacePath)
		}
		merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, wsData))
		if mergeErr == nil {
			dataDir := cfg.Options.DataDirectory
			*cfg = *merged
			cfg.setDefaults(s.workingDir, dataDir)
			loadedPaths = append(loadedPaths, workspacePath)
		}
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

	s.setConfig(cfg)
	s.loadedPaths = loadedPaths
	s.resolver = resolver
	s.knownProviders = providers
	s.overrides = overrides
	s.workspacePath = workspacePath

	if configured {
		// Note: unlike Load, a fallback model correction (resolved.*Fallback)
		// is not persisted to disk here, only applied in memory. This
		// matches reloadFromDiskLocked's pre-existing behavior; persisting
		// via updateLocked here would need its own failure handling (Load
		// can simply discard the whole store on error, but a reload has
		// already published a config other goroutines may be reading).
		// Left as-is rather than risked as part of this refactor.
		s.SetupAgents()
	}

	// Rebuild staleness tracking. Track every discovered config path, not
	// just the ones that loaded, so a config file created after this reload
	// is detected as a change on the next staleness check.
	s.captureStalenessSnapshot(append(slices.Clone(configPaths), loadedPaths...))

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
