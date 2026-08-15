package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/env"
	"github.com/rave-soft/braid/internal/lock"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// configLockDeadline bounds how long lockConfig waits for the
// cross-process flock before giving up. A few seconds is plenty for
// honest contention; longer suggests something is wedged.
const configLockDeadline = 5 * time.Second

var errAtomicWriteNoop = errors.New("config mutation is a no-op")

// credentialWriteLockDeadline bounds how long a credential write (e.g.
// storing the token from a fresh interactive login) waits for the
// per-provider refresh lock. It is deliberately shorter than
// credentials.Manager's own refresh-lock deadline (45s) because a user is
// watching: if a peer is wedged we would rather write and risk a rare
// clobber than hang the UI.
const credentialWriteLockDeadline = 10 * time.Second

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
// RemoveConfigField, PersistRefreshedToken) to prevent both in-process
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
	configMu          sync.RWMutex
	version           atomic.Uint64
	credentialVersion atomic.Uint64
	mcpMutationEpochs map[string]uint64

	mu       sync.Mutex   // serialises config file writes
	writeMu  sync.RWMutex // serialises in-memory config production (mutators + the reload swap); RLock for readers
	reloadMu sync.Mutex   // serialises reload attempts against each other; see the ConfigStore doc comment

	// onExternalChangeMu guards onExternalChange, the callback
	// WatchForExternalChanges runs after a reload it triggered itself
	// (as opposed to one caused by this process's own writes). See
	// watch.go.
	onExternalChangeMu sync.Mutex
	onExternalChange   func()

	// agentSnapshotMu guards agentFileSnapshot, the last-seen state of
	// every *.md file under agentDirs (agents_markdown.go) plus the global
	// agents directory. Unlike trackedConfigPaths, this tracks directory
	// membership rather than a fixed path list, so agent files can be
	// added or removed, not just edited, between polls. See watch.go.
	agentSnapshotMu   sync.Mutex
	agentFileSnapshot map[string]fileSnapshot
	inheritedAgents   map[string]Agent
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
	s.config = cfg
	s.version.Add(1)
	s.configMu.Unlock()
}

func (s *ConfigStore) Version() uint64 {
	return s.version.Load()
}

func (s *ConfigStore) CredentialVersion() uint64 {
	return s.credentialVersion.Load()
}

// UpdateProviderCredentials publishes a credential-only provider update. It
// is the sole runtime mutation path for provider credentials: callers must not
// mutate Config().Providers directly, because a runtime compiled before the
// mutation would otherwise remain cache-valid and continue using old clients.
// The provider's model identity is preserved by the caller; incrementing the
// store version forces future runtime compilation to construct a provider with
// the newly published credentials.
func (s *ConfigStore) UpdateProviderCredentials(providerID, apiKey string, token *oauth.Token) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	current := s.Config()
	if current == nil || current.Providers == nil {
		return fmt.Errorf("provider %s not found", providerID)
	}
	cfg := current.cloneForWrite()
	provider, ok := cfg.Providers.Get(providerID)
	if !ok {
		return fmt.Errorf("provider %s not found", providerID)
	}
	provider.APIKey = apiKey
	provider.OAuthToken = token
	if providerID == string(catwalk.InferenceProviderCopilot) {
		provider.SetupGitHubCopilot()
	}
	cfg.Providers.Set(providerID, provider)
	s.credentialVersion.Add(1)
	s.setConfig(cfg)
	return nil
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
	path, err := s.ConfigPath(scope)
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

	path, err := s.ConfigPath(scope)
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
	if errors.Is(err, errAtomicWriteNoop) {
		return nil
	}
	if err != nil {
		return err
	}

	return atomicWriteFile(path, newData, 0o600)
}

// ConfigPath returns the file path for the given scope.
func (s *ConfigStore) ConfigPath(scope Scope) (string, error) {
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
	if path, err := s.ConfigPath(scope); err == nil {
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

	persist := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		err := s.updateLocked(scope, func(cfg *Config) map[string]any {
			current, ok := cfg.Providers.Get(providerID)
			if !ok {
				current = providerConfig
			}
			current.APIKey = providerConfig.APIKey
			current.OAuthToken = providerConfig.OAuthToken
			if providerID == string(catwalk.InferenceProviderCopilot) {
				current.SetupGitHubCopilot()
			}
			cfg.Providers.Set(providerID, current)
			return fields
		})
		if err == nil {
			s.credentialVersion.Add(1)
		}
		return err
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

// PersistRefreshedToken writes a refreshed OAuth token's credential
// fields to disk and republishes the in-memory config. Called by
// credentials.Manager after a successful refresh; kept on ConfigStore
// because it needs isCatalogProvider and update, both store internals
// that credentials must not reach into directly.
func (s *ConfigStore) PersistRefreshedToken(scope Scope, providerID string, cfg ProviderConfig, token *oauth.Token) error {
	fields := map[string]any{
		ProviderFieldKey(providerID, "api_key"): token.AccessToken,
		ProviderFieldKey(providerID, "oauth"):   token,
	}
	// Persist identity fields for providers outside the embedded catalog —
	// see isCatalogProvider. Without this, a custom OAuth provider's
	// type/base_url/name live only in memory: the next reload rebuilds
	// Config from disk (configureProviders), finds no base_url for this
	// provider, and drops it as an invalid custom provider.
	if !s.isCatalogProvider(providerID) {
		fields[ProviderFieldKey(providerID, "type")] = cfg.Type
		fields[ProviderFieldKey(providerID, "base_url")] = cfg.BaseURL
		fields[ProviderFieldKey(providerID, "name")] = cfg.Name
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

// withRefreshLock runs fn while holding the per-provider cross-process
// refresh lock, so a credential write cannot interleave with a peer's
// token exchange. Acquisition is best effort: when the lock cannot be
// taken in time, fn runs anyway rather than blocking a write the user is
// waiting on.
func (s *ConfigStore) withRefreshLock(providerID string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), credentialWriteLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, s.RefreshLockPath(providerID))
	if err != nil {
		slog.Warn("Writing credentials without the refresh lock", "provider", providerID, "error", err)
		return fn()
	}
	defer release()
	return fn()
}

// RefreshLockPath returns the path to the per-provider cross-process refresh
// lock file. Lock files live under a dedicated locks/ subdirectory of the
// data dir so they do not clutter the config directory. The file is created
// on demand by lock.File and is never removed (flock keys on inode, not
// path).
func (s *ConfigStore) RefreshLockPath(providerID string) string {
	dir := filepath.Join(filepath.Dir(s.globalDataPath), "locks")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, fmt.Sprintf("%s.refresh.lock", providerID))
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
