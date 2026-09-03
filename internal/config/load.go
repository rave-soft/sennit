package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qjebbs/go-jsons"
	"github.com/rave-soft/sennit/internal/config/migrate"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/shellconfig"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type credentialsFileDependency struct {
	homeDir string
	stat    func(string) (os.FileInfo, error)
}

// LoadData loads configuration without provider runtime orchestration.
//
// Production boots exclusively through LoadWithProcessor, which requires a
// RuntimeProcessor; this entry point exists so tests can build a real,
// disk-backed store through the same pipeline without one. That makes it
// unreachable from main and so a permanent fixture of `deadcode` output —
// it is not a caller that went missing. See internal/config/configtest,
// whose package doc names it, and its ~20 callers across config,
// workspace/appws, agent and app tests.
func LoadData(workingDir, dataDir string, debug bool) (*ConfigStore, error) {
	return load(workingDir, dataDir, debug, credentialsFileDependency{homeDir: home.Dir(), stat: os.Stat}, nil)
}

func LoadWithProcessor(workingDir, dataDir string, debug bool, processor RuntimeProcessor) (*ConfigStore, error) {
	if processor == nil {
		return nil, fmt.Errorf("runtime processor is required")
	}
	return load(workingDir, dataDir, debug, credentialsFileDependency{homeDir: home.Dir(), stat: os.Stat}, processor)
}

func load(workingDir, dataDir string, debug bool, credentialsFile credentialsFileDependency, processor RuntimeProcessor) (*ConfigStore, error) {
	// Migrate deprecated disable_notifications before loading config.
	migrateDisableNotifications()

	store := &ConfigStore{
		workingDir:      workingDir,
		globalDataPath:  GlobalConfigData(),
		watcher:         externalChangeWatcher{pollInterval: externalChangePollInterval},
		debugOverride:   debug,
		credentialsFile: credentialsFile,
		processor:       processor,
	}

	built, err := buildConfig(store, buildConfigOptions{
		ctx:               context.Background(),
		workingDir:        workingDir,
		dataDir:           dataDir,
		migrateModelCache: true,
		persistFallback:   true,
		credentialsFile:   credentialsFile,
		processor:         processor,
	})
	if err != nil {
		return nil, err
	}

	// The store is not published anywhere until Load returns, so nothing
	// can race these field assignments; writeMu is taken further down only
	// because updateLocked/SetupAgents document it as a precondition.
	store.config = built.cfg
	store.workspacePath.Set(filepath.Join(built.cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName)))
	store.loadedPaths = built.loadedPaths
	store.knownProviders = built.providers
	store.resolver = built.resolver

	if !built.configured {
		slog.Warn("No providers configured")
		// Capture the staleness snapshot even on this early return.
		// Without it, tracked path set stays empty and a background
		// watcher (WatchForExternalChanges) would treat every discovered
		// config path as "new" on every poll, reloading in a busy loop
		// until a provider gets configured.
		store.CaptureStalenessSnapshot(append(slices.Clone(built.configPaths), built.loadedPaths...))
		store.watcher.captureAgentFiles(store.workingDir)
		return store, nil
	}

	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	// Pin the model this instance started with, so a reload cannot swap it
	// for one a sibling instance chose. Several instances share the global
	// config file, and any of them writing to it (a model switch, an OAuth
	// refresh, a provider being added) reloads it in all the others —
	// which without this pin would hand every running session whichever
	// model was selected last, somewhere else. Worse, that model may name
	// a provider this instance has no idea about, and the session breaks
	// mid-run over a change made in another project.
	//
	// Pinning at startup rather than at first selection keeps the other
	// half of the contract working: the file is read fresh here, so a new
	// instance still starts on whatever default was chosen most recently.
	store.pinPreferredModelLocked(built.resolved.Model)

	// Persist any fallback correction.
	if built.persistFallback {
		if err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
			return store.updatePreferredModelFields(c, built.resolved.Model)
		}); err != nil {
			return nil, fmt.Errorf("failed to update preferred model: %w", err)
		}
	}
	store.SetupAgents()

	// Capture initial staleness snapshot. Track every discovered config path,
	// not just the ones that loaded, so a config file created after startup
	// (e.g. a sennitrc added mid-session) is detected as a change.
	store.CaptureStalenessSnapshot(append(slices.Clone(built.configPaths), built.loadedPaths...))
	store.watcher.captureAgentFiles(store.workingDir)

	return store, nil
}

func applyWorkspaceConfig(cfg *Config, workingDir string, loadedPaths *[]string) error {
	workspacePath := filepath.Clean(filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName)))
	// The default data directory (.sennit) is also a project config layer
	// found by lookupConfigs, so with default settings this is the same file
	// loadFromConfigPaths already merged in. Merging it again here would
	// double every array (go-jsons.Merge concatenates slices) and record the
	// same file twice in loadedPaths.
	if slices.Contains(*loadedPaths, workspacePath) {
		return nil
	}
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
	// The workspace config is project-scoped: provider and model settings in
	// it are ignored the same way they are in a project sennit.json/sennitrc.
	workspaceData, globalOnly := dropGlobalOnlyKeys(workspaceData, workspacePath)

	merged, err := loadFromBytes([][]byte{mustMarshalConfig(cfg), workspaceData})
	if err != nil {
		slog.Warn("Failed to merge workspace config", "path", workspacePath, "error", err)
		return nil
	}

	dataDir := cfg.Options.DataDirectory
	merged.jsonAgentsBlockDetected = merged.jsonAgentsBlockDetected || cfg.jsonAgentsBlockDetected
	// Problems do not survive the JSON round trip above (they are not
	// serialized), so carry the ones collected so far onto the merged config.
	merged.Problems = append(slices.Clone(cfg.Problems), merged.Problems...)
	recordGlobalOnlyProblems(merged, globalOnly)
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

func loadFromConfigPaths(ctx context.Context, configPaths []string, projectTrusted bool) (*Config, []string, error) {
	allowProject := projectTrusted
	var configs [][]byte
	var loaded []string

	// Track directories that have both sennit.json and sennitrc to warn
	// about potential confusion, along with the top-level keys each
	// defines so we can report conflicts.
	jsonDirKeys := make(map[string]map[string]bool)
	shDirKeys := make(map[string]map[string]bool)

	// Problems recorded for global-only keys (providers, model, ...) found in
	// a project-scoped layer. Collected here and attached once the Config
	// exists, since dropping happens before it is built.
	var globalOnly []Problem

	for _, path := range configPaths {
		if path == "" {
			continue
		}
		if !allowProject && !isGlobalConfigPath(path) {
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
				if !isGlobalConfigPath(path) {
					var dropped []Problem
					jsonBytes, dropped = dropGlobalOnlyKeys(jsonBytes, path)
					globalOnly = append(globalOnly, dropped...)
				}
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
			if !isGlobalConfigPath(path) {
				var dropped []Problem
				data, dropped = dropGlobalOnlyKeys(data, path)
				globalOnly = append(globalOnly, dropped...)
			}
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
	recordGlobalOnlyProblems(cfg, globalOnly)
	return cfg, loaded, nil
}

// recordGlobalOnlyProblems attaches the problems collected while stripping
// global-only keys from project layers, and logs each one so the ignore is
// visible without opening the doctor.
func recordGlobalOnlyProblems(cfg *Config, problems []Problem) {
	for _, p := range problems {
		slog.Warn("Ignoring global-only setting in a project config", "path", p.Subject, "detail", p.Message)
		cfg.addProblem(p)
	}
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

// loadFromBytes is the single choke point every JSON config layer passes
// through before landing in a Config. It strips a top-level "agents" key
// rather than letting it decode into Config.Agents: unlike a normal
// deprecated-key rename (see migrate.MigrateDeprecatedKey), a JSON agents block has
// no in-place replacement to migrate to — subagents are defined exclusively
// as .sennit/agents/*.md files now, so the block is simply discarded, with
// jsonAgentsBlockDetected left behind for SetupAgents to turn into a doctor
// Problem instead of silently ignoring it forever.
func applyLayerTombstones(accumulated, incoming map[string]any, masked map[string]map[string]bool) error {
	for _, sectionName := range []string{"mcp", "lsp", "providers"} {
		section, ok := incoming[sectionName].(map[string]any)
		if !ok {
			continue
		}
		for name, entry := range section {
			tombstone, isTombstone, err := shellconfig.ParseTombstone(entry, sectionName, name)
			if err != nil {
				return err
			}
			if isTombstone {
				if current, ok := accumulated[sectionName].(map[string]any); ok {
					delete(current, name)
				}
				if tombstone.Replacement == nil {
					delete(section, name)
					masked[sectionName][name] = true
				} else {
					section[name] = tombstone.Replacement
					delete(masked[sectionName], name)
				}
				continue
			}
			if !masked[sectionName][name] {
				continue
			}
			if sectionName == "mcp" && isMCPTokenOverlay(entry) {
				delete(section, name)
				continue
			}
			delete(masked[sectionName], name)
		}
	}
	return nil
}

func isMCPTokenOverlay(value any) bool {
	entry, ok := value.(map[string]any)
	if !ok || len(entry) != 1 {
		return false
	}
	_, ok = entry["oauth_token"]
	return ok
}

func loadFromBytes(configs [][]byte) (*Config, error) {
	if len(configs) == 0 {
		return &Config{}, nil
	}

	data := []byte(`{}`)
	masked := map[string]map[string]bool{"mcp": {}, "lsp": {}, "providers": {}}
	for _, layer := range configs {
		var accumulated map[string]any
		if err := json.Unmarshal(data, &accumulated); err != nil {
			return nil, err
		}
		var incoming map[string]any
		if err := json.Unmarshal(layer, &incoming); err != nil {
			return nil, err
		}
		if err := applyLayerTombstones(accumulated, incoming, masked); err != nil {
			return nil, err
		}
		base, err := json.Marshal(accumulated)
		if err != nil {
			return nil, err
		}
		next, err := json.Marshal(incoming)
		if err != nil {
			return nil, err
		}
		data, err = jsons.Merge([][]byte{base, next})
		if err != nil {
			return nil, err
		}
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
	migrate.DisableNotifications(GlobalConfig(), GlobalConfigData(), fsext.AtomicWriteFile)
}

func assignIfNil[T any](ptr **T, val T) {
	if *ptr == nil {
		*ptr = &val
	}
}

func isAppleTerminal() bool { return os.Getenv("TERM_PROGRAM") == "Apple_Terminal" }
