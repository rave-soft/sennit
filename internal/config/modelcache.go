package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/db"
)

// modelCacheDBPath returns the path to the global model-discovery cache,
// which lives next to the global data-dir config rather than inside any
// per-project .braid/braid.db (that one is per-project user data, uses
// goose migrations, and is a different concern entirely).
func modelCacheDBPath(globalDataPath string) string {
	return filepath.Join(filepath.Dir(globalDataPath), "models.db")
}

// withModelCache opens the model-discovery cache, ensures its schema
// exists, runs fn, and closes the connection. Cache reads/writes are rare
// (once per custom provider per config load, plus `braid models refresh`),
// so open-do-close per call is fine; there is no long-lived pool here, only
// braid.db (internal/db.Connect) needs one.
func withModelCache(dbPath string, fn func(*sql.DB) error) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return err
	}

	ctx := context.Background()
	conn, err := db.OpenDB(ctx, dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	const schema = `
CREATE TABLE IF NOT EXISTS provider_models (
	provider_id TEXT PRIMARY KEY,
	models_json TEXT NOT NULL,
	fetched_at INTEGER NOT NULL
);
PRAGMA user_version = 1;
`
	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return err
	}

	return fn(conn)
}

// loadCachedModels returns the models cached for providerID, and whether a
// usable cache entry was found. A missing db/dir, a missing row, or corrupt
// JSON all report ok=false rather than an error: the cache is a best-effort
// optimization, never a hard dependency for config loading.
func loadCachedModels(globalDataPath, providerID string) ([]catwalk.Model, bool) {
	if globalDataPath == "" {
		// No data-dir config path to anchor the cache to (e.g. a
		// hand-built *ConfigStore in a unit test); treat as "no cache"
		// rather than falling back to a relative "models.db" in cwd.
		return nil, false
	}
	dbPath := modelCacheDBPath(globalDataPath)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, false
	}

	var models []catwalk.Model
	found := false
	err := withModelCache(dbPath, func(conn *sql.DB) error {
		var modelsJSON string
		err := conn.QueryRowContext(context.Background(),
			`SELECT models_json FROM provider_models WHERE provider_id = ?`, providerID).Scan(&modelsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if jsonErr := json.Unmarshal([]byte(modelsJSON), &models); jsonErr != nil {
			slog.Warn("Ignoring corrupt model cache entry", "provider", providerID, "error", jsonErr)
			return nil
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, false
	}
	return models, found
}

// mergeCachedModelMetadata fills metadata gaps in freshly discovered models
// from a previously cached list. A plain OpenAI-compatible /models response
// carries no context window, token limits, or pricing, so a refresh would
// otherwise wipe values that were enriched earlier or set by hand in the
// cache. For each fresh model whose numeric metadata is zero, the cached
// entry with the same ID (when present) supplies the value; a non-zero
// fresh value always wins.
func mergeCachedModelMetadata(cached, fresh []catwalk.Model) []catwalk.Model {
	if len(cached) == 0 {
		return fresh
	}
	byID := make(map[string]catwalk.Model, len(cached))
	for _, m := range cached {
		byID[m.ID] = m
	}
	for i := range fresh {
		old, ok := byID[fresh[i].ID]
		if !ok {
			continue
		}
		if fresh[i].ContextWindow == 0 {
			fresh[i].ContextWindow = old.ContextWindow
		}
		if fresh[i].DefaultMaxTokens == 0 {
			fresh[i].DefaultMaxTokens = old.DefaultMaxTokens
		}
		if fresh[i].CostPer1MIn == 0 {
			fresh[i].CostPer1MIn = old.CostPer1MIn
		}
		if fresh[i].CostPer1MOut == 0 {
			fresh[i].CostPer1MOut = old.CostPer1MOut
		}
		if fresh[i].CostPer1MInCached == 0 {
			fresh[i].CostPer1MInCached = old.CostPer1MInCached
		}
		if fresh[i].CostPer1MOutCached == 0 {
			fresh[i].CostPer1MOutCached = old.CostPer1MOutCached
		}
	}
	return fresh
}

// saveCachedModels upserts the discovered models for providerID into the
// global model-discovery cache. This is best-effort persistence, matching
// how applyPendingDiskActions treats config writes: a failure is logged and
// otherwise ignored rather than propagated, since discovery having
// succeeded is the part that matters.
func saveCachedModels(globalDataPath, providerID string, models []catwalk.Model) {
	if globalDataPath == "" {
		// See loadCachedModels: no anchor path means no cache.
		return
	}
	if cached, ok := loadCachedModels(globalDataPath, providerID); ok {
		models = mergeCachedModelMetadata(cached, models)
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		slog.Warn("Failed to marshal models for cache", "provider", providerID, "error", err)
		return
	}

	dbPath := modelCacheDBPath(globalDataPath)
	err = withModelCache(dbPath, func(conn *sql.DB) error {
		_, err := conn.ExecContext(context.Background(), `
			INSERT INTO provider_models (provider_id, models_json, fetched_at)
			VALUES (?, ?, ?)
			ON CONFLICT(provider_id) DO UPDATE SET
				models_json = excluded.models_json,
				fetched_at = excluded.fetched_at
		`, providerID, string(modelsJSON), time.Now().Unix())
		return err
	})
	if err != nil {
		slog.Warn("Failed to save models to cache", "provider", providerID, "error", err)
	}
}

// SaveCachedProviderModels writes freshly discovered models for providerID
// into the global model-discovery cache. Unlike saveCachedModels (used from
// the best-effort background load path), this returns an error so callers
// like `braid models refresh` can report a failure to the user.
func (s *ConfigStore) SaveCachedProviderModels(providerID string, models []catwalk.Model) error {
	if s.globalDataPath == "" {
		return errors.New("no global data path configured for this store")
	}
	if cached, ok := loadCachedModels(s.globalDataPath, providerID); ok {
		models = mergeCachedModelMetadata(cached, models)
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return err
	}

	dbPath := modelCacheDBPath(s.globalDataPath)
	return withModelCache(dbPath, func(conn *sql.DB) error {
		_, err := conn.ExecContext(context.Background(), `
			INSERT INTO provider_models (provider_id, models_json, fetched_at)
			VALUES (?, ?, ?)
			ON CONFLICT(provider_id) DO UPDATE SET
				models_json = excluded.models_json,
				fetched_at = excluded.fetched_at
		`, providerID, string(modelsJSON), time.Now().Unix())
		return err
	})
}
