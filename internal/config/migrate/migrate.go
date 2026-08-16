// Package migrate owns Braid's one-time, idempotent config-file migrations.
//
// It transforms raw config bytes and rewrites on-disk config files while they
// are protected by cross-process file locks. It is deliberately a leaf
// package: it does not import the config package, and every config-specific
// collaborator (model-cache persistence, atomic file writes, global path
// resolution) is passed in by the caller. config depends on migrate, never
// the reverse.
package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/lock"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// lockDeadline bounds how long LockFiles waits for a config lock before
// giving up. It mirrors config's own config write deadline so that
// migrations and the store's regular config writes contend within the same
// honest-contention window.
const lockDeadline = 5 * time.Second

// ModelCacheMigrationThreshold is the minimum array length BloatedModelCache
// treats as "probably an old discovery dump" rather than a hand-written
// list. A user typing out models by hand rarely lists more than a handful;
// the incident that prompted this threshold (see BloatedModelCache) was a
// 3-entry manual list on a llama.cpp provider getting swept up as if it
// were bloat and handed to a refresh that then replaced it with junk.
// Erring on the side of leaving a small list alone is cheap: it just means
// the data-dir file keeps a few extra lines, not that a real router
// provider's thousands of models stay in braid.json.
const ModelCacheMigrationThreshold = 50

// DropIncompatibleRecentModels drops a pre-refactor "recent_models" value
// (an object keyed by "large"/"small") that no longer unmarshals into the
// current flat-array shape. Old configs are not migrated — the field is
// simply dropped and rebuilt from scratch — but a stale shape must not turn
// into a hard json.Unmarshal failure that stops braid from starting.
func DropIncompatibleRecentModels(data []byte, path string) []byte {
	v := gjson.GetBytes(data, "recent_models")
	if !v.Exists() || v.IsArray() {
		return data
	}
	slog.Warn("Ignoring recent_models from a pre-refactor config (large/small slots are gone)", "path", path)
	out, err := sjson.DeleteBytes(data, "recent_models")
	if err != nil {
		return data
	}
	return out
}

// MigrateDeprecatedKey renames a deprecated JSON key (gjson/sjson dotted
// path) to its replacement in-memory so old config files (e.g. an
// "options.strands" key from before the threads rename) keep working
// without editing them on disk. If newKey is already present, the
// deprecated oldKey is dropped in favor of it rather than overwriting the
// value the user already migrated.
func MigrateDeprecatedKey(data []byte, oldKey, newKey, path string) []byte {
	old := gjson.GetBytes(data, oldKey)
	if !old.Exists() {
		return data
	}
	slog.Warn(fmt.Sprintf("Config key %q is deprecated, use %q instead", oldKey, newKey), "path", path)
	if gjson.GetBytes(data, newKey).Exists() {
		out, err := sjson.DeleteBytes(data, oldKey)
		if err != nil {
			return data
		}
		return out
	}
	out, err := sjson.SetRawBytes(data, newKey, []byte(old.Raw))
	if err != nil {
		return data
	}
	out, err = sjson.DeleteBytes(out, oldKey)
	if err != nil {
		return data
	}
	return out
}

// BloatedModelCache is a one-time, idempotent migration that moves
// auto-discovered models still sitting in the data-dir config file (from
// before the model-discovery cache existed) into the cache, and strips them
// out of the JSON. It operates on the raw bytes of globalDataPath directly,
// never on the in-memory cfg and never on ~/.config/braid.json: only the
// data-dir file (machine-owned, writable state) is a candidate, only for
// provider IDs outside the known catalog (a catalog provider's models field
// is a legitimate user override, not a discovery dump), and only for arrays
// bigger than ModelCacheMigrationThreshold — a short list is assumed
// hand-written and is left exactly where the user put it.
//
// This process's already-parsed in-memory cfg keeps whatever models it
// unmarshaled before this runs; that's fine; only the next Load() benefits,
// via resolveCustomProviderModels reading the now-populated cache.
//
// saveModels persists the migrated models into the model-discovery cache and
// writeFile writes the updated data-dir config atomically. Both are injected
// so this package keeps no dependency on config.
func BloatedModelCache(
	globalDataPath string,
	knownProviders []catwalk.Provider,
	saveModels func(string, string, []catwalk.Model) error,
	writeFile func(string, []byte, os.FileMode) error,
) {
	if globalDataPath == "" {
		return
	}

	unlock, err := LockFiles(globalDataPath)
	if err != nil {
		slog.Warn("Failed to acquire model cache migration lock", "path", globalDataPath, "error", err)
		return
	}
	defer unlock()

	data, err := os.ReadFile(globalDataPath)
	if err != nil || len(data) == 0 {
		return
	}

	known := make(map[string]bool, len(knownProviders))
	for _, p := range knownProviders {
		known[string(p.ID)] = true
	}

	updated := string(data)
	changed := false
	gjson.Get(updated, "providers").ForEach(func(id, provider gjson.Result) bool {
		providerID := id.String()
		if known[providerID] {
			return true
		}
		models := provider.Get("models")
		if !models.Exists() || !models.IsArray() || len(models.Array()) <= ModelCacheMigrationThreshold {
			return true
		}

		var parsed []catwalk.Model
		if err := json.Unmarshal([]byte(models.Raw), &parsed); err != nil {
			slog.Warn("Skipping bloated model cache migration for provider with unparseable models", "provider", providerID, "error", err)
			return true
		}

		if err := saveModels(globalDataPath, providerID, parsed); err != nil {
			slog.Warn("Failed to migrate provider models to cache", "provider", providerID, "error", err)
			return true
		}
		if out, err := sjson.Delete(updated, providerModelsKey(providerID)); err == nil {
			updated = out
			changed = true
		}
		return true
	})

	if !changed {
		return
	}
	if err := writeFile(globalDataPath, []byte(updated), 0o600); err != nil {
		slog.Warn("Failed to migrate bloated model cache", "path", globalDataPath, "error", err)
		return
	}
	slog.Info("Migrated auto-discovered provider models out of the data-dir config into the model cache", "path", globalDataPath)
}

// providerModelsKey builds the gjson/sjson path "providers.<id>.models" for a
// dynamic, user-supplied provider ID. It mirrors config.ProviderFieldKey(id,
// "models") but is duplicated here so this package has no dependency on
// config: gjson.Escape backslash-escapes path metacharacters in the ID so it
// round-trips as a single literal key.
func providerModelsKey(providerID string) string {
	return fmt.Sprintf("providers.%s.models", gjson.Escape(providerID))
}

// DisableNotifications migrates the deprecated disable_notifications and
// notification_style fields to the unified notifications field. It checks
// both the user config (~/.config) and data config (~/.local) files. If
// disable_notifications is true, it sets notifications to "disabled" in the
// data file. If notification_style is set, it moves the value to
// notifications. Regardless of value, it removes the deprecated fields from
// any file that contains them.
//
// globalConfig and dataConfig are the two on-disk config paths to migrate,
// and writeFile writes the updated files atomically. Both are injected so
// this package keeps no dependency on config.
func DisableNotifications(globalConfig, dataConfig string, writeFile func(string, []byte, os.FileMode) error) {
	unlock, err := LockFiles(globalConfig, dataConfig)
	if err != nil {
		slog.Warn("Failed to acquire notification migration locks", "error", err)
		return
	}
	defer unlock()

	paths := []string{globalConfig, dataConfig}
	dataByPath := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				dataByPath[path] = []byte(`{}`)
				continue
			}
			slog.Warn("Skipping notification migration after config read failure", "path", path, "error", err)
			return
		}
		if !json.Valid(data) || !gjson.ParseBytes(data).IsObject() {
			slog.Warn("Skipping notification migration for invalid config JSON", "path", path)
			return
		}
		dataByPath[path] = data
	}

	var wasDisabled bool
	var styleValue string
	filesToClean := make([]string, 0, len(paths))
	for _, path := range paths {
		data := dataByPath[path]
		needsClean := false
		if v := gjson.GetBytes(data, "options.disable_notifications"); v.Exists() {
			needsClean = true
			wasDisabled = wasDisabled || v.Bool()
		}
		if v := gjson.GetBytes(data, "options.notification_style"); v.Exists() {
			needsClean = true
			if styleValue == "" {
				styleValue = v.String()
			}
		}
		if needsClean {
			filesToClean = append(filesToClean, path)
		}
	}
	if len(filesToClean) == 0 {
		return
	}

	data := dataByPath[dataConfig]
	updatedData := string(data)
	if !gjson.GetBytes(data, "options.notifications").Exists() {
		migratedValue := styleValue
		if migratedValue == "" && wasDisabled {
			migratedValue = "disabled"
		}
		if migratedValue != "" {
			updatedData, err = sjson.Set(updatedData, "options.notifications", migratedValue)
			if err != nil {
				slog.Warn("Failed to prepare migrated notification settings", "error", err)
				return
			}
			slog.Info("Migrated notification settings to notifications field", "value", migratedValue)
		}
	}
	updatedData, err = sjson.Delete(updatedData, "options.disable_notifications")
	if err != nil {
		slog.Warn("Failed to prepare notification migration cleanup", "path", dataConfig, "error", err)
		return
	}
	updatedData, err = sjson.Delete(updatedData, "options.notification_style")
	if err != nil {
		slog.Warn("Failed to prepare notification migration cleanup", "path", dataConfig, "error", err)
		return
	}
	if err := writeFile(dataConfig, []byte(updatedData), 0o600); err != nil {
		slog.Warn("Failed to write migrated notification settings", "path", dataConfig, "error", err)
		return
	}

	for _, path := range filesToClean {
		if path == dataConfig {
			continue
		}
		updated := string(dataByPath[path])
		updated, err = sjson.Delete(updated, "options.disable_notifications")
		if err != nil {
			slog.Warn("Failed to prepare notification migration cleanup", "path", path, "error", err)
			continue
		}
		updated, err = sjson.Delete(updated, "options.notification_style")
		if err != nil {
			slog.Warn("Failed to prepare notification migration cleanup", "path", path, "error", err)
			continue
		}
		if err := writeFile(path, []byte(updated), 0o600); err != nil {
			slog.Warn("Failed to write migrated config cleanup", "path", path, "error", err)
		}
	}
}

// LockFiles acquires cross-process advisory file locks on the ".lock"
// sibling of each given path, in a deterministic (sorted, deduplicated)
// order so that concurrent migrations never deadlock by locking the same
// set of files in opposite orders. It returns a single release function that
// drops all acquired locks. Empty paths are ignored, and each lock's parent
// directory is created if missing.
func LockFiles(paths ...string) (func(), error) {
	unique := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			unique[filepath.Clean(path)] = struct{}{}
		}
	}
	ordered := slices.Collect(maps.Keys(unique))
	slices.Sort(ordered)
	releases := make([]func(), 0, len(ordered))
	ctx, cancel := context.WithTimeout(context.Background(), lockDeadline)
	defer cancel()
	for _, path := range ordered {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			for _, release := range slices.Backward(releases) {
				release()
			}
			return nil, fmt.Errorf("create config directory: %w", err)
		}
		release, err := lock.File(ctx, path+".lock")
		if err != nil {
			for _, release := range slices.Backward(releases) {
				release()
			}
			return nil, fmt.Errorf("acquire config lock: %w", err)
		}
		releases = append(releases, release)
	}
	return func() {
		for _, release := range slices.Backward(releases) {
			release()
		}
	}, nil
}
