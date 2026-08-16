package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/qjebbs/go-jsons"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config/migrate"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/shellconfig"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Load loads the configuration from the default paths and returns a
// ConfigStore that owns both the pure-data Config and all runtime state.
func Load(workingDir, dataDir string, debug bool) (*ConfigStore, error) {
	// Migrate deprecated disable_notifications before loading config.
	migrateDisableNotifications()

	configPaths := lookupConfigs(workingDir)

	cfg, loadedPaths, err := loadFromConfigPaths(context.Background(), configPaths)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	cfg.setDefaults(workingDir, dataDir)

	store := &ConfigStore{
		config:         cfg,
		workingDir:     workingDir,
		globalDataPath: GlobalConfigData(),
		workspacePath:  filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName)),
		loadedPaths:    loadedPaths,
	}

	if debug {
		cfg.Options.Debug = true
	}

	// Load workspace config last so it has highest priority.
	if err := applyWorkspaceConfig(cfg, workingDir, &store.loadedPaths); err != nil {
		return nil, err
	}

	// Validate hooks after all config merging is complete so workspace
	// hooks also get their matcher regexes compiled.
	if err := cfg.ValidateHooks(); err != nil {
		return nil, fmt.Errorf("invalid hook configuration: %w", err)
	}

	applyEnvironmentDefaults(cfg)

	// Load known providers, this loads the config from catwalk. A failed
	// refresh still yields the cached or embedded catalog, so only an empty
	// list is fatal: starting up without providers is worse than starting
	// up with slightly stale ones.
	providers, err := Providers(cfg)
	if err != nil {
		if len(providers) == 0 {
			return nil, err
		}
		slog.Warn("Continuing with the previously known providers", "error", err)
	}
	store.knownProviders = providers

	// One-time migration: move any auto-discovered models still sitting in
	// the data-dir config file (from before the model-discovery cache
	// existed) into the cache, so the JSON config stops carrying arrays that
	// can run into the thousands of entries for providers with large
	// catalogs. Needs the known-provider list to tell a catalog provider's
	// legitimate override apart from a custom provider's discovery dump.
	migrateBloatedModelCache(store.globalDataPath, providers)

	env := env.New()
	// Configure providers
	valueResolver := NewShellVariableResolver(env)
	store.resolver = valueResolver

	// Apply top-level env vars before configuring providers so variables
	// like AWS_PROFILE are visible to the AWS SDK credential chain.
	cfg.applyEnv(valueResolver)

	// configureProviders may run model-discovery HTTP calls for custom
	// providers (see discoverCustomProviderModels). It runs here, without
	// writeMu held, so the store is never seen mid-lock by anything for the
	// full duration of a slow discovery round trip. The store is not
	// published anywhere until Load returns, so nothing can race this
	// section regardless; writeMu is taken further down only because
	// updateLocked/SetupAgents document it as a precondition.
	if err := cfg.configureProviders(context.Background(), store, env, valueResolver, store.knownProviders); err != nil {
		return nil, fmt.Errorf("failed to configure providers: %w", err)
	}

	if !cfg.IsConfigured() {
		slog.Warn("No providers configured")
		// Capture the staleness snapshot even on this early return.
		// Without it, trackedConfigPaths stays empty and a background
		// watcher (WatchForExternalChanges) would treat every discovered
		// config path as "new" on every poll, reloading in a busy loop
		// until a provider gets configured.
		store.captureStalenessSnapshot(append(slices.Clone(configPaths), loadedPaths...))
		store.captureAgentFileSnapshot()
		return store, nil
	}

	resolved, err := resolveSelectedModel(cfg, store.knownProviders)
	if err != nil {
		return nil, fmt.Errorf("failed to configure selected models: %w", err)
	}
	cfg.Model = resolved.Model

	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	// Persist any fallback correction.
	if resolved.Fallback {
		if err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
			return store.updatePreferredModelFields(c, resolved.Model)
		}); err != nil {
			return nil, fmt.Errorf("failed to update preferred model: %w", err)
		}
	}
	store.SetupAgents()

	// Capture initial staleness snapshot. Track every discovered config path,
	// not just the ones that loaded, so a config file created after startup
	// (e.g. a sennitrc added mid-session) is detected as a change.
	store.captureStalenessSnapshot(append(slices.Clone(configPaths), loadedPaths...))
	store.captureAgentFileSnapshot()

	return store, nil
}

func applyWorkspaceConfig(cfg *Config, workingDir string, loadedPaths *[]string) error {
	workspacePath := filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName))
	workspaceData, err := os.ReadFile(workspacePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read workspace config %s: %w", workspacePath, err)
	}
	if len(workspaceData) == 0 {
		return nil
	}
	if !json.Valid(workspaceData) {
		return fmt.Errorf("invalid JSON in config file %s", workspacePath)
	}

	workspaceData = migrate.DropIncompatibleRecentModels(workspaceData, workspacePath)
	merged, err := loadFromBytes([][]byte{mustMarshalConfig(cfg), workspaceData})
	if err != nil {
		slog.Warn("Failed to merge workspace config", "path", workspacePath, "error", err)
		return nil
	}

	dataDir := cfg.Options.DataDirectory
	merged.jsonAgentsBlockDetected = merged.jsonAgentsBlockDetected || cfg.jsonAgentsBlockDetected
	*cfg = *merged
	cfg.setDefaults(workingDir, dataDir)
	*loadedPaths = append(*loadedPaths, workspacePath)
	return nil
}

// mustMarshalConfig marshals the config to JSON bytes, returning empty JSON on
// error.
func mustMarshalConfig(cfg *Config) []byte {
	data, err := json.Marshal(cfg)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func PushPopEnvOverrides() func() {
	var found []string
	for _, ev := range os.Environ() {
		if strings.HasPrefix(ev, brand.EnvPrefix) {
			pair := strings.SplitN(ev, "=", 2)
			if len(pair) != 2 {
				continue
			}
			found = append(found, strings.TrimPrefix(pair[0], brand.EnvPrefix))
		}
	}
	backups := make(map[string]string)
	for _, ev := range found {
		backups[ev] = os.Getenv(ev)
	}

	for _, ev := range found {
		if err := os.Setenv(ev, os.Getenv(brand.EnvPrefix+ev)); err != nil {
			slog.Warn("Failed to set env var from SENNIT_ override", "key", ev, "error", err)
		}
	}

	restore := func() {
		for k, v := range backups {
			if err := os.Setenv(k, v); err != nil {
				slog.Warn("Failed to restore env var", "key", k, "error", err)
			}
		}
	}
	return restore
}

// applyEnv sets top-level env vars from the config. Keys are sorted for
// deterministic ordering so that vars referencing other vars via the
// value resolver produce consistent results.
func (c *Config) applyEnv(resolver VariableResolver) {
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		resolved, err := resolver.ResolveValue(c.Env[k])
		if err != nil {
			slog.Warn("Skipping env var due to resolution failure.", "key", k, "value", c.Env[k], "error", err)
			continue
		}
		if err := os.Setenv(k, resolved); err != nil {
			slog.Warn("Failed to set env var", "key", k, "error", err)
		}
	}
}

func loadFromConfigPaths(ctx context.Context, configPaths []string) (*Config, []string, error) {
	var configs [][]byte
	var loaded []string

	// Track directories that have both sennit.json and sennitrc to warn
	// about potential confusion, along with the top-level keys each
	// defines so we can report conflicts.
	jsonDirKeys := make(map[string]map[string]bool)
	shDirKeys := make(map[string]map[string]bool)

	for _, path := range configPaths {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("failed to open config file %s: %w", path, err)
		}
		if len(data) == 0 {
			continue
		}

		dir := filepath.Dir(path)
		if isShellConfig(path) {
			jsonBytes, err := shellconfig.LoadShellConfig(ctx, path, data)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to load shell config %s: %w", path, err)
			}
			if len(jsonBytes) > 0 {
				if !json.Valid(jsonBytes) {
					return nil, nil, fmt.Errorf("shell config %s produced invalid JSON", path)
				}
				jsonBytes = migrate.MigrateDeprecatedKey(jsonBytes, "options.strands", "options.threads", path)
				jsonBytes = migrate.DropIncompatibleRecentModels(jsonBytes, path)
				addTopLevelKeys(shDirKeys, dir, jsonBytes)
				configs = append(configs, jsonBytes)
				loaded = append(loaded, path)
			}
		} else {
			if !json.Valid(data) {
				return nil, nil, fmt.Errorf("invalid JSON in config file %s", path)
			}
			data = migrate.MigrateDeprecatedKey(data, "options.strands", "options.threads", path)
			data = migrate.DropIncompatibleRecentModels(data, path)
			addTopLevelKeys(jsonDirKeys, dir, data)
			configs = append(configs, data)
			loaded = append(loaded, path)
		}
	}

	// Warn if both a JSON config and a sennitrc exist in the same directory
	// and define overlapping top-level keys. Disjoint coexistence is
	// intentional and not worth warning about.
	for dir, jKeys := range jsonDirKeys {
		sKeys, ok := shDirKeys[dir]
		if !ok {
			continue
		}
		var conflicts []string
		for k := range jKeys {
			if sKeys[k] {
				conflicts = append(conflicts, k)
			}
		}
		if len(conflicts) > 0 {
			slices.Sort(conflicts)
			slog.Warn("Found both a JSON config and a sennitrc in the same directory; merging with sennitrc taking precedence",
				"dir", dir, "conflicting_keys", strings.Join(conflicts, ", "))
		}
	}

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, err
	}
	return cfg, loaded, nil
}

// addTopLevelKeys records the top-level JSON keys present in data into the
// set for dir.
func addTopLevelKeys(m map[string]map[string]bool, dir string, data []byte) {
	keys := m[dir]
	if keys == nil {
		keys = make(map[string]bool)
		m[dir] = keys
	}
	gjson.ParseBytes(data).ForEach(func(key, _ gjson.Result) bool {
		keys[key.String()] = true
		return true
	})
}

// migrateBloatedModelCache is a one-time, idempotent migration that moves
// auto-discovered models still sitting in the data-dir config file (from
// before the model-discovery cache existed) into the cache, and strips them
// out of the JSON. The actual migration logic lives in the config/migrate
// package; this wrapper wires in config's model-cache persistence
// (saveCachedModelsWithError) and atomic file writer.
func migrateBloatedModelCache(globalDataPath string, knownProviders []catwalk.Provider) {
	migrate.BloatedModelCache(globalDataPath, knownProviders, saveCachedModelsWithError, atomicWriteFile)
}

// loadFromBytes is the single choke point every JSON config layer passes
// through before landing in a Config. It strips a top-level "agents" key
// rather than letting it decode into Config.Agents: unlike a normal
// deprecated-key rename (see migrate.MigrateDeprecatedKey), a JSON agents block has
// no in-place replacement to migrate to — subagents are defined exclusively
// as .sennit/agents/*.md files now, so the block is simply discarded, with
// jsonAgentsBlockDetected left behind for SetupAgents to turn into a doctor
// Problem instead of silently ignoring it forever.
func loadFromBytes(configs [][]byte) (*Config, error) {
	if len(configs) == 0 {
		return &Config{}, nil
	}

	data, err := jsons.Merge(configs)
	if err != nil {
		return nil, err
	}

	hadAgentsBlock := gjson.GetBytes(data, "agents").Exists()
	if hadAgentsBlock {
		if out, err := sjson.DeleteBytes(data, "agents"); err == nil {
			data = out
		}
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	config.jsonAgentsBlockDetected = hadAgentsBlock
	return &config, nil
}

func hasAWSCredentials(env env.Env) bool {
	if env.Get("AWS_BEARER_TOKEN_BEDROCK") != "" {
		return true
	}

	if env.Get("AWS_ACCESS_KEY_ID") != "" && env.Get("AWS_SECRET_ACCESS_KEY") != "" {
		return true
	}

	if env.Get("AWS_PROFILE") != "" || env.Get("AWS_DEFAULT_PROFILE") != "" {
		return true
	}

	if env.Get("AWS_REGION") != "" || env.Get("AWS_DEFAULT_REGION") != "" {
		return true
	}

	if env.Get("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" ||
		env.Get("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return true
	}

	// File-based credential discovery requires filesystem stats, so do it
	// last and skip it under test. Checking testing.Testing() before the
	// os.Stat call (rather than after, in the && tail) ensures the syscall
	// is never issued during tests, where it otherwise ran unconditionally
	// and only had its result discarded.
	if testing.Testing() {
		return false
	}
	if _, err := os.Stat(filepath.Join(home.Dir(), ".aws/credentials")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(home.Dir(), ".aws/login")); err == nil {
		return true
	}

	return false
}

// migrateDisableNotifications migrates the deprecated disable_notifications
// and notification_style fields to the unified notifications field. It checks
// both the user config (~/.config) and data config (~/.local) files. If
// disable_notifications is true, it sets notifications to "disabled" in the
// data file. If notification_style is set, it moves the value to
// notifications. Regardless of value, it removes the deprecated fields from
// any file that contains them. The actual migration logic lives in the
// config/migrate package; this wrapper wires in config's global path
// resolution and atomic file writer.
func migrateDisableNotifications() {
	migrate.DisableNotifications(GlobalConfig(), GlobalConfigData(), atomicWriteFile)
}

func assignIfNil[T any](ptr **T, val T) {
	if *ptr == nil {
		*ptr = &val
	}
}

func isAppleTerminal() bool { return os.Getenv("TERM_PROGRAM") == "Apple_Terminal" }

// normalizeHookEvent maps user-provided event names to their canonical
// form. Matching is case-insensitive and accepts snake_case variants
// (e.g. "pre_tool_use" → "PreToolUse").
func normalizeHookEvent(name string) string {
	switch strings.ToLower(strings.ReplaceAll(name, "_", "")) {
	case "pretooluse":
		return "PreToolUse"
	default:
		return name
	}
}

// ValidateHooks normalizes event names and checks that every configured
// hook has a command and a syntactically valid matcher regex. Matcher
// compilation used for matching is owned by hooks.Runner; this function
// only validates up front so the user sees config errors at load time
// rather than on the first tool call.
func (c *Config) ValidateHooks() error {
	// Normalize event name keys.
	for event, eventHooks := range c.Hooks {
		canonical := normalizeHookEvent(event)
		if canonical != event {
			c.Hooks[canonical] = append(c.Hooks[canonical], eventHooks...)
			delete(c.Hooks, event)
		}
	}

	for event, eventHooks := range c.Hooks {
		for i, h := range eventHooks {
			if h.Command == "" {
				return fmt.Errorf("hook %s[%d]: command is required", event, i)
			}
			if h.Matcher == "" {
				continue
			}
			if _, err := regexp.Compile(h.Matcher); err != nil {
				return fmt.Errorf("hook %s[%d]: invalid matcher regex %q: %w", event, i, h.Matcher, err)
			}
		}
	}
	return nil
}
