package config

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	powernapConfig "github.com/charmbracelet/x/powernap/pkg/config"
	"github.com/qjebbs/go-jsons"
	"github.com/rave-soft/braid/internal/config/migrate"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/env"
	"github.com/rave-soft/braid/internal/filepathext"
	"github.com/rave-soft/braid/internal/fsext"
	"github.com/rave-soft/braid/internal/home"
	"github.com/rave-soft/braid/internal/shellconfig"
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
	// (e.g. a braidrc added mid-session) is detected as a change.
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

// applyEnvironmentDefaults applies defaults that depend on the process
// environment and git status rather than on config file contents: reduced
// file-walk limits when not inside a git worktree, and transparent
// background under Apple Terminal. Shared by Load and reloadFromDisk so a
// reload does not silently drop a default that was only ever applied at
// startup — see TestLoad_AppleTerminalDefaultSurvivesReload.
func applyEnvironmentDefaults(cfg *Config) {
	if !isInsideWorktree() {
		const depth = 2
		const items = 100
		slog.Warn("No git repository detected in working directory, will limit file walk operations", "depth", depth, "items", items)
		assignIfNil(&cfg.Tools.Ls.MaxDepth, depth)
		assignIfNil(&cfg.Tools.Ls.MaxItems, items)
		assignIfNil(&cfg.Options.TUI.Completions.MaxDepth, depth)
		assignIfNil(&cfg.Options.TUI.Completions.MaxItems, items)
	}

	if isAppleTerminal() {
		slog.Warn("Detected Apple Terminal, enabling transparent mode")
		assignIfNil(&cfg.Options.TUI.Transparent, true)
	}
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

func PushPopBraidEnv() func() {
	var found []string
	for _, ev := range os.Environ() {
		if strings.HasPrefix(ev, "BRAID_") {
			pair := strings.SplitN(ev, "=", 2)
			if len(pair) != 2 {
				continue
			}
			found = append(found, strings.TrimPrefix(pair[0], "BRAID_"))
		}
	}
	backups := make(map[string]string)
	for _, ev := range found {
		backups[ev] = os.Getenv(ev)
	}

	for _, ev := range found {
		if err := os.Setenv(ev, os.Getenv("BRAID_"+ev)); err != nil {
			slog.Warn("Failed to set env var from BRAID_ override", "key", ev, "error", err)
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

func (c *Config) setDefaults(workingDir, dataDir string) {
	c.workingDir = workingDir
	if c.Options == nil {
		c.Options = &Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &TUIOptions{}
	}
	if len(c.Options.GlobalContextPaths) == 0 {
		braidConfigDir := filepath.Dir(GlobalConfig())
		c.Options.GlobalContextPaths = []string{
			filepath.Join(braidConfigDir, "BRAID.md"),
			filepath.Join(filepath.Dir(braidConfigDir), "AGENTS.md"),
		}
	}
	slices.Sort(c.Options.GlobalContextPaths)
	c.Options.GlobalContextPaths = slices.Compact(c.Options.GlobalContextPaths)

	if dataDir != "" {
		c.Options.DataDirectory = dataDir
	} else if c.Options.DataDirectory == "" {
		if path, ok := fsext.LookupClosestBounded(workingDir, projectBoundary(workingDir), defaultDataDirectory); ok {
			c.Options.DataDirectory = path
		} else {
			c.Options.DataDirectory = filepath.Join(workingDir, defaultDataDirectory)
		}
	}
	c.Options.DataDirectory = filepath.Clean(filepathext.SmartJoin(workingDir, c.Options.DataDirectory))
	// Tool-name lists come from user-authored files that predate any
	// rename, so fold legacy names onto current ones before anything
	// matches against them. See [CanonicalToolName].
	c.Options.DisabledTools = canonicalToolNames(c.Options.DisabledTools)
	if c.Permissions != nil {
		c.Permissions.AllowedTools = canonicalToolNames(c.Permissions.AllowedTools)
	}
	if c.Providers == nil {
		c.Providers = csync.NewMap[string, ProviderConfig]()
	}
	if c.MCP == nil {
		c.MCP = make(map[string]MCPConfig)
	}
	// Drop orphaned OAuth token entries left behind when a user removes
	// an MCP from braid.json. See MCPConfig.isOrphanedToken.
	for name, m := range c.MCP {
		if m.isOrphanedToken() {
			delete(c.MCP, name)
		}
	}
	if c.LSP == nil {
		c.LSP = make(map[string]LSPConfig)
	}

	// Apply defaults to LSP configurations
	c.applyLSPDefaults()

	// Add the default context paths if they are not already present
	c.Options.ContextPaths = append(slices.Clone(defaultContextPaths), c.Options.ContextPaths...)

	slices.Sort(c.Options.ContextPaths)
	c.Options.ContextPaths = slices.Compact(c.Options.ContextPaths)

	// Add the default skills directories if not already present.
	for _, dir := range GlobalSkillsDirs() {
		if !slices.Contains(c.Options.SkillsPaths, dir) {
			c.Options.SkillsPaths = append(c.Options.SkillsPaths, dir)
		}
	}

	// Project specific skills dirs.
	c.Options.SkillsPaths = append(c.Options.SkillsPaths, ProjectSkillsDir(workingDir)...)

	if str, ok := os.LookupEnv("BRAID_DISABLE_DEFAULT_PROVIDERS"); ok {
		c.Options.DisableDefaultProviders, _ = strconv.ParseBool(str)
	}

	if c.Options.Attribution == nil {
		c.Options.Attribution = &Attribution{
			TrailerStyle:  TrailerStyleAssistedBy,
			GeneratedWith: true,
		}
	} else if c.Options.Attribution.TrailerStyle == "" {
		// Migrate deprecated co_authored_by or apply default
		if c.Options.Attribution.CoAuthoredBy != nil {
			if *c.Options.Attribution.CoAuthoredBy {
				c.Options.Attribution.TrailerStyle = TrailerStyleCoAuthoredBy
			} else {
				c.Options.Attribution.TrailerStyle = TrailerStyleNone
			}
		} else {
			c.Options.Attribution.TrailerStyle = TrailerStyleAssistedBy
		}
	}

	c.Options.InitializeAs = cmp.Or(c.Options.InitializeAs, defaultInitializeAs)
}

// powernapDefaults caches the powernap default LSP server catalog. The
// catalog is static and immutable for the life of the process, but
// building it (NewManager + LoadDefaults) is expensive and was previously
// repeated on every config reload. We load it once and only ever read from
// it via GetServer, so a shared instance is safe.
var (
	powernapDefaultsOnce sync.Once
	powernapDefaults     *powernapConfig.Manager
)

func lspDefaultsManager() *powernapConfig.Manager {
	powernapDefaultsOnce.Do(func() {
		m := powernapConfig.NewManager()
		// LoadDefaults only fails on malformed embedded defaults, which
		// would be a build-time bug; treat the manager as usable either
		// way so a transient error never wedges config loading.
		_ = m.LoadDefaults()
		powernapDefaults = m
	})
	return powernapDefaults
}

// applyLSPDefaults applies default values from powernap to LSP configurations
func (c *Config) applyLSPDefaults() {
	// Reuse the process-wide default catalog; building it per reload was a
	// significant chunk of reload latency.
	configManager := lspDefaultsManager()

	// Apply defaults to each LSP configuration
	for name, cfg := range c.LSP {
		// Try to get defaults from powernap based on name or command name.
		base, ok := configManager.GetServer(name)
		if !ok {
			base, ok = configManager.GetServer(cfg.Command)
			if !ok {
				continue
			}
		}
		if cfg.Options == nil {
			cfg.Options = base.Settings
		}
		if cfg.InitOptions == nil {
			cfg.InitOptions = base.InitOptions
		}
		if len(cfg.FileTypes) == 0 {
			cfg.FileTypes = base.FileTypes
		}
		if len(cfg.RootMarkers) == 0 {
			cfg.RootMarkers = base.RootMarkers
		}
		cfg.Command = cmp.Or(cfg.Command, base.Command)
		if len(cfg.Args) == 0 {
			cfg.Args = base.Args
		}
		if len(cfg.Env) == 0 {
			cfg.Env = base.Environment
		}
		// Update the config in the map
		c.LSP[name] = cfg
	}
}

func (c *Config) defaultModelSelection(knownProviders []catwalk.Provider) (model SelectedModel, err error) {
	if len(knownProviders) == 0 && c.Providers.Len() == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return model, err
	}

	// Use the first provider enabled based on the known providers order
	// if no provider found that is known use the first provider configured
	for _, p := range knownProviders {
		providerConfig, ok := c.Providers.Get(string(p.ID))
		if !ok || providerConfig.Disable {
			continue
		}
		defaultModel := c.GetModel(string(p.ID), p.DefaultLargeModelID)
		if defaultModel == nil {
			slog.Warn("Default model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			if len(providerConfig.Models) == 0 {
				return model, fmt.Errorf("default model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			}
			defaultModel = &providerConfig.Models[0]
		}
		model = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultModel.ID,
			MaxTokens:       defaultModel.DefaultMaxTokens,
			ReasoningEffort: defaultModel.DefaultReasoningEffort,
		}
		return model, err
	}

	enabledProviders := c.EnabledProviders()
	slices.SortFunc(enabledProviders, func(a, b ProviderConfig) int {
		return strings.Compare(a.ID, b.ID)
	})

	if len(enabledProviders) == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return model, err
	}

	providerConfig := enabledProviders[0]
	if len(providerConfig.Models) == 0 {
		err = fmt.Errorf("provider %s has no models configured", providerConfig.ID)
		return model, err
	}
	defaultModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	model = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultModel.ID,
		MaxTokens: defaultModel.DefaultMaxTokens,
	}
	return model, err
}

// resolvedModel holds the result of resolving the user-configured model
// selection against the provider catalog.
type resolvedModel struct {
	Model    SelectedModel
	Fallback bool // true if Model was corrected to a default
}

// resolveSelectedModel validates the user's configured model selection
// against the provider catalog, falling back to a default when the model ID
// is invalid. It is pure resolution logic: it does not mutate the store or
// touch disk. The caller assigns the result to cfg.Model and persists any
// fallback correction as appropriate.
func resolveSelectedModel(cfg *Config, knownProviders []catwalk.Provider) (resolvedModel, error) {
	var result resolvedModel
	def, err := cfg.defaultModelSelection(knownProviders)
	if err != nil {
		return result, fmt.Errorf("failed to select default model: %w", err)
	}
	selected := def

	modelSelected := cfg.Model
	// The zero SelectedModel{} is the "unset" sentinel (Config.Model has no
	// map to check key presence against anymore), so any field the user set
	// — even just max_tokens, with provider/model left to inherit the
	// default — marks the model as configured.
	modelConfigured := !reflect.DeepEqual(modelSelected, SelectedModel{})
	if modelConfigured {
		if modelSelected.Model != "" {
			selected.Model = modelSelected.Model
		}
		if modelSelected.Provider != "" {
			selected.Provider = modelSelected.Provider
		}
		model := cfg.GetModel(selected.Provider, selected.Model)
		if model == nil {
			cfg.addProblem(Problem{
				Severity: SeverityError,
				Area:     AreaModel,
				Subject:  modelSelected.Provider + "/" + modelSelected.Model,
				Message: fmt.Sprintf(
					"configured main model %s/%s not found — falling back to %s/%s",
					modelSelected.Provider, modelSelected.Model, def.Provider, def.Model,
				),
				Hint: "run 'braid models' to see available provider/model pairs",
			})
			selected = def
			result.Fallback = true
		} else {
			if modelSelected.MaxTokens > 0 {
				selected.MaxTokens = modelSelected.MaxTokens
			} else {
				selected.MaxTokens = model.DefaultMaxTokens
			}
			if modelSelected.ReasoningEffort != "" {
				selected.ReasoningEffort = modelSelected.ReasoningEffort
			} else {
				selected.ReasoningEffort = model.DefaultReasoningEffort
			}
			selected.Think = modelSelected.Think
			if modelSelected.Temperature != nil {
				selected.Temperature = modelSelected.Temperature
			}
			if modelSelected.TopP != nil {
				selected.TopP = modelSelected.TopP
			}
			if modelSelected.TopK != nil {
				selected.TopK = modelSelected.TopK
			}
			if modelSelected.FrequencyPenalty != nil {
				selected.FrequencyPenalty = modelSelected.FrequencyPenalty
			}
			if modelSelected.PresencePenalty != nil {
				selected.PresencePenalty = modelSelected.PresencePenalty
			}
			if modelSelected.ProviderOptions != nil {
				selected.ProviderOptions = maps.Clone(modelSelected.ProviderOptions)
			}
		}
	}

	result.Model = selected
	return result, nil
}

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated braid.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	// Prepend global user config and machine-owned data JSON. Only the user
	// config directory contributes a braidrc; the data directory is writable
	// machine state and must never be executed as Bash. Missing files are
	// skipped when loaded.
	configPaths := []string{
		systemConfigPath,
		GlobalConfig(),
		shellConfigSibling(GlobalConfig()),
		GlobalConfigData(),
	}

	// Ordered high-to-low priority within a directory. LookupBounded returns
	// matches in this order, and the later reverse + merge make the earliest
	// listed name win on conflict. So: the .braid/ subdirectory variants beat
	// their root-level counterparts, .braidrc beats braidrc, both beat the
	// JSON configs, and .braid.json beats braid.json.
	//
	// The .braid/ variants are looked up as literal names — ".braid" here is
	// not options.data_directory (which is configurable and resolved
	// separately, see workspacePath in Load/reloadFromDisk); it is the
	// project's canonical config subdirectory, checked at every directory in
	// the upward walk just like the other names. defaultDataDirectory holds
	// the same literal (".braid") so the two don't drift.
	configNames := []string{
		filepath.Join(defaultDataDirectory, appName+"rc"),
		"." + appName + "rc",
		appName + "rc",
		filepath.Join(defaultDataDirectory, appName+".json"),
		"." + appName + ".json",
		appName + ".json",
	}

	foundConfigs, err := fsext.LookupBounded(cwd, projectBoundary(cwd), configNames...)
	if err != nil {
		// returns at least default configs
		return configPaths
	}

	// reverse order so last config has more priority
	slices.Reverse(foundConfigs)

	return append(configPaths, foundConfigs...)
}

func loadFromConfigPaths(ctx context.Context, configPaths []string) (*Config, []string, error) {
	var configs [][]byte
	var loaded []string

	// Track directories that have both braid.json and braidrc to warn
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

	// Warn if both a JSON config and a braidrc exist in the same directory
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
			slog.Warn("Found both a JSON config and a braidrc in the same directory; merging with braidrc taking precedence",
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
// as .braid/agents/*.md files now, so the block is simply discarded, with
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

// GlobalConfig returns the global configuration file path for the application.
func GlobalConfig() string {
	if braidGlobal := os.Getenv("BRAID_GLOBAL_CONFIG"); braidGlobal != "" {
		return filepath.Join(braidGlobal, fmt.Sprintf("%s.json", appName))
	}
	return filepath.Join(home.Config(), appName, fmt.Sprintf("%s.json", appName))
}

// GlobalDBDir returns the directory holding the single SQLite database
// shared by every project, ~/.config/braid by default (or
// BRAID_GLOBAL_CONFIG's directory when set). Every workspace connects to
// the same braid.db; rows are scoped by project_path.
func GlobalDBDir() string {
	return filepath.Dir(GlobalConfig())
}

// GlobalLogFile returns the path to the single log file shared by every
// project, ~/.config/braid/logs/braid.log by default (alongside the
// shared database — see GlobalDBDir).
func GlobalLogFile() string {
	return filepath.Join(GlobalDBDir(), "logs", "braid.log")
}

// shellConfigSibling returns the braidrc path that sits alongside a given
// braid.json path (same directory). Used so global config locations pick up a
// shell config, not just JSON.
func shellConfigSibling(jsonPath string) string {
	return filepath.Join(filepath.Dir(jsonPath), appName+"rc")
}

// isShellConfig reports whether a config path is a shell config (braidrc or
// the hidden .braidrc), as opposed to a JSON config.
func isShellConfig(path string) bool {
	base := filepath.Base(path)
	return base == appName+"rc" || base == "."+appName+"rc"
}

// ProjectConfigs returns list of current project configs paths.
func ProjectConfigs(cwd string) []string {
	return lookupConfigs(cwd)
}

// GlobalConfigData returns the path to the main data directory for the application.
// this config is used when the app overrides configurations instead of updating the global config.
func GlobalConfigData() string {
	if braidData := os.Getenv("BRAID_GLOBAL_DATA"); braidData != "" {
		return filepath.Join(braidData, fmt.Sprintf("%s.json", appName))
	}
	if xdgDataHome := os.Getenv("XDG_DATA_HOME"); xdgDataHome != "" {
		return filepath.Join(xdgDataHome, appName, fmt.Sprintf("%s.json", appName))
	}

	// return the path to the main data directory
	// for windows, it should be in `%LOCALAPPDATA%/braid/`
	// for linux and macOS, it should be in `$HOME/.local/share/braid/`
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, appName, fmt.Sprintf("%s.json", appName))
	}

	return filepath.Join(home.Dir(), ".local", "share", appName, fmt.Sprintf("%s.json", appName))
}

// GlobalWorkspaceDir returns the path to the global server workspace
// directory. This directory acts as a meta-workspace for the server
// process, giving it a real workingDir so that config loading, scoped
// writes, and provider resolution behave identically to project
// workspaces.
func GlobalWorkspaceDir() string {
	return filepath.Dir(GlobalConfigData())
}

func assignIfNil[T any](ptr **T, val T) {
	if *ptr == nil {
		*ptr = &val
	}
}

func isInsideWorktree() bool {
	bts, err := exec.CommandContext(
		context.Background(),
		"git", "rev-parse",
		"--is-inside-work-tree",
	).CombinedOutput()
	return err == nil && strings.TrimSpace(string(bts)) == "true"
}

// worktreeRoot returns the absolute path of the git working tree root for
// dir, or the empty string if dir is not inside a working tree (bare
// repositories, missing git binary, plain directories, or any other
// failure mode). Linked worktrees and submodules each report their own
// top-level, which is what callers want when bounding lookups.
// worktreeRootCache memoizes the git worktree root per directory. The root
// is stable for the life of the process, so we avoid re-shelling out to
// "git rev-parse" on every config reload. Keyed by the requested dir; the
// value is the resolved root ("" when dir is not in a git worktree).
var worktreeRootCache sync.Map // map[string]string

func worktreeRoot(dir string) string {
	if cached, ok := worktreeRootCache.Load(dir); ok {
		return cached.(string)
	}
	root := computeWorktreeRoot(dir)
	worktreeRootCache.Store(dir, root)
	return root
}

func computeWorktreeRoot(dir string) string {
	cmd := exec.CommandContext(
		context.Background(),
		"git", "rev-parse", "--show-toplevel",
	)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	return abs
}

// projectBoundary returns the directory at which an upward configuration
// search rooted at dir should stop. It is the git working tree root when
// one can be detected, otherwise dir itself. Returning dir as a
// fallback keeps Braid from silently adopting state files placed above
// the current project.
func projectBoundary(dir string) string {
	if root := worktreeRoot(dir); root != "" {
		return root
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// GlobalSkillsDirs returns the default directories for Agent Skills.
// Skills in these directories are auto-discovered and their files can be read
// without permission prompts.
//
// Only Braid's own catalog is scanned here. Skills authored for other tools
// (Claude Code, opencode, ...) are not auto-discovered — see `braid import`,
// which copies them into .braid/skills with validation instead of trusting a
// foreign directory implicitly.
func GlobalSkillsDirs() []string {
	if braidSkills := os.Getenv("BRAID_SKILLS_DIR"); braidSkills != "" {
		return []string{braidSkills}
	}

	paths := []string{
		filepath.Join(home.Config(), appName, "skills"),
	}

	// On Windows, also load from app data on top of `$HOME/.config/braid`.
	// This is here mostly for backwards compatibility.
	if runtime.GOOS == "windows" {
		appData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		paths = append(paths, filepath.Join(appData, appName, "skills"))
	}

	return paths
}

// projectSkillSubdirs lists the conventional subdirectories where
// project-level skills are discovered. Shared across working-dir and
// git-root lookups to prevent drift when a new convention is added.
//
// Only .braid/skills is scanned: skills written for other tools are brought
// in explicitly via `braid import`, not auto-discovered from their native
// directories.
var projectSkillSubdirs = []string{
	".braid/skills",
}

// ProjectSkillsDir returns the default project directories for which Braid
// will look for skills. In addition to the working directory, it also
// checks the git working tree root so that monorepo-level skills are
// discovered when the user is inside a subdirectory.
// Working-directory paths come first so local skills take precedence
// over monorepo-level ones.
func ProjectSkillsDir(workingDir string) []string {
	dirs := make([]string, 0, len(projectSkillSubdirs)*2)
	for _, sub := range projectSkillSubdirs {
		dirs = append(dirs, filepath.Join(workingDir, sub))
	}

	// When the working directory is inside a git repository, also look at
	// the repository root so monorepo-level .agents/skills are found.
	if root := worktreeRoot(workingDir); root != "" && root != workingDir {
		for _, sub := range projectSkillSubdirs {
			dirs = append(dirs, filepath.Join(root, sub))
		}
	}

	return dirs
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
