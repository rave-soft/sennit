package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config/migrate"
	"github.com/rave-soft/braid/internal/lock"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMigrateBloatedModelCache_RereadsAfterLockAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "braid.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(bloatedModelConfig(`"before": true`)), 0o600))

	release, err := lock.File(context.Background(), path+".lock")
	require.NoError(t, err)
	finished := make(chan struct{})
	go func() {
		migrateBloatedModelCache(path, nil)
		close(finished)
	}()

	select {
	case <-finished:
		t.Fatal("migration did not wait for the config lock")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, os.WriteFile(path, []byte(bloatedModelConfig(`"concurrent": true`)), 0o600))
	release()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("migration did not finish after the config lock was released")
	}

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(data, "concurrent").Bool())
	require.False(t, gjson.GetBytes(data, "providers.custom.models").Exists())
	cached, ok := loadCachedModels(path, "custom")
	require.True(t, ok)
	require.Len(t, cached, migrate.ModelCacheMigrationThreshold+1)

	beforeRetry := string(data)
	migrateBloatedModelCache(path, nil)
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeRetry, string(data))
}

func TestMigrateBloatedModelCache_CacheWriteFailureRetriesPerProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "braid.json")
	config := fmt.Sprintf(
		`{"concurrent":{"preserved":true},"providers":{"failed":{"type":"openai","models":%s},"saved":{"type":"openai","models":%s}}}`,
		bloatedModelsJSON("failed"),
		bloatedModelsJSON("saved"),
	)
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))

	failure := errors.New("cache write failed")
	migrate.BloatedModelCache(path, nil, func(globalDataPath, providerID string, models []catwalk.Model) error {
		if providerID == "failed" {
			return failure
		}
		return saveCachedModelsWithError(globalDataPath, providerID, models)
	}, atomicWriteFile)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(data, "concurrent.preserved").Bool())
	require.Equal(t, "openai", gjson.GetBytes(data, "providers.failed.type").String())
	require.True(t, gjson.GetBytes(data, "providers.failed.models").Exists())
	require.Equal(t, "openai", gjson.GetBytes(data, "providers.saved.type").String())
	require.False(t, gjson.GetBytes(data, "providers.saved.models").Exists())
	_, ok := loadCachedModels(path, "failed")
	require.False(t, ok)
	saved, ok := loadCachedModels(path, "saved")
	require.True(t, ok)
	require.Len(t, saved, migrate.ModelCacheMigrationThreshold+1)

	migrateBloatedModelCache(path, nil)

	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(data, "concurrent.preserved").Bool())
	require.Equal(t, "openai", gjson.GetBytes(data, "providers.failed.type").String())
	require.False(t, gjson.GetBytes(data, "providers.failed.models").Exists())
	require.Equal(t, "openai", gjson.GetBytes(data, "providers.saved.type").String())
	require.False(t, gjson.GetBytes(data, "providers.saved.models").Exists())
	failed, ok := loadCachedModels(path, "failed")
	require.True(t, ok)
	require.Len(t, failed, migrate.ModelCacheMigrationThreshold+1)
}

func TestMigrateDisableNotifications_RereadsBothFilesAndPreservesNotifications(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)
	globalPath := GlobalConfig()
	dataPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(dataPath), 0o755))
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"options":{"notification_style":"bell"}}`), 0o600))
	require.NoError(t, os.WriteFile(dataPath, []byte(`{"options":{"disable_notifications":true}}`), 0o600))

	release, err := lock.File(context.Background(), dataPath+".lock")
	require.NoError(t, err)
	finished := make(chan struct{})
	go func() {
		migrateDisableNotifications()
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("migration did not wait for the data config lock")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, os.WriteFile(dataPath, []byte(`{"options":{"disable_notifications":true,"notifications":"osc"},"concurrent":true}`), 0o600))
	release()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("migration did not finish after the config lock was released")
	}

	globalData, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	dataData, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(globalData, "options.notification_style").Exists())
	require.False(t, gjson.GetBytes(dataData, "options.disable_notifications").Exists())
	require.Equal(t, "osc", gjson.GetBytes(dataData, "options.notifications").String())
	require.True(t, gjson.GetBytes(dataData, "concurrent").Bool())

	globalBefore, dataBefore := string(globalData), string(dataData)
	migrateDisableNotifications()
	globalData, err = os.ReadFile(globalPath)
	require.NoError(t, err)
	dataData, err = os.ReadFile(dataPath)
	require.NoError(t, err)
	require.Equal(t, globalBefore, string(globalData))
	require.Equal(t, dataBefore, string(dataData))
}

func TestMigrateDisableNotifications_DestinationWriteFailureRetriesWithoutLosingLegacy(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)
	globalPath := GlobalConfig()
	dataPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"options":{"notification_style":"bell"}}`), 0o600))

	migrate.DisableNotifications(globalPath, dataPath, func(path string, data []byte, perm os.FileMode) error {
		if path == dataPath {
			return errors.New("destination write failed")
		}
		return atomicWriteFile(path, data, perm)
	})
	globalData, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(globalData, "options.notification_style").Exists())
	_, err = os.Stat(dataPath)
	require.True(t, os.IsNotExist(err))

	migrateDisableNotifications()
	globalData, err = os.ReadFile(globalPath)
	require.NoError(t, err)
	dataData, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(globalData, "options.notification_style").Exists())
	require.Equal(t, "bell", gjson.GetBytes(dataData, "options.notifications").String())
	require.False(t, gjson.GetBytes(dataData, "options.notification_style").Exists())
}

func TestMigrateDisableNotifications_CleanupFailureRetriesWithExistingNotifications(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)
	globalPath := GlobalConfig()
	dataPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(dataPath), 0o755))
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"options":{"notification_style":"bell"}}`), 0o600))
	require.NoError(t, os.WriteFile(dataPath, []byte(`{"options":{"notifications":"osc","disable_notifications":true}}`), 0o600))

	migrate.DisableNotifications(globalPath, dataPath, func(path string, data []byte, perm os.FileMode) error {
		if path == globalPath {
			return errors.New("cleanup write failed")
		}
		return atomicWriteFile(path, data, perm)
	})
	globalData, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	dataData, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(globalData, "options.notification_style").Exists())
	require.Equal(t, "osc", gjson.GetBytes(dataData, "options.notifications").String())
	require.False(t, gjson.GetBytes(dataData, "options.disable_notifications").Exists())

	migrateDisableNotifications()
	globalData, err = os.ReadFile(globalPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(globalData, "options.notification_style").Exists())
	dataData, err = os.ReadFile(dataPath)
	require.NoError(t, err)
	require.Equal(t, "osc", gjson.GetBytes(dataData, "options.notifications").String())
}

func TestMigrateDisableNotifications_MigratesDataWithoutGlobalConfig(t *testing.T) {
	for name, legacy := range map[string]string{
		"notification style":    `"notification_style":"bell"`,
		"disable notifications": `"disable_notifications":true`,
	} {
		t.Run(name, func(t *testing.T) {
			globalDir := t.TempDir()
			dataDir := t.TempDir()
			t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
			t.Setenv("BRAID_GLOBAL_DATA", dataDir)
			globalPath := GlobalConfig()
			dataPath := GlobalConfigData()
			require.NoError(t, os.MkdirAll(filepath.Dir(dataPath), 0o755))
			require.NoError(t, os.WriteFile(dataPath, []byte(`{"options":{`+legacy+`},"preserved":true}`), 0o600))

			migrateDisableNotifications()

			data, err := os.ReadFile(dataPath)
			require.NoError(t, err)
			require.True(t, gjson.GetBytes(data, "preserved").Bool())
			require.False(t, gjson.GetBytes(data, "options.notification_style").Exists())
			require.False(t, gjson.GetBytes(data, "options.disable_notifications").Exists())
			if name == "notification style" {
				require.Equal(t, "bell", gjson.GetBytes(data, "options.notifications").String())
			} else {
				require.Equal(t, "disabled", gjson.GetBytes(data, "options.notifications").String())
			}
			_, err = os.Stat(globalPath)
			require.True(t, os.IsNotExist(err))

			beforeRetry := string(data)
			migrateDisableNotifications()
			data, err = os.ReadFile(dataPath)
			require.NoError(t, err)
			require.Equal(t, beforeRetry, string(data))
			_, err = os.Stat(globalPath)
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestMigrateDisableNotifications_MalformedInputIsNotChanged(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)
	globalPath := GlobalConfig()
	dataPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(dataPath), 0o755))
	globalData := []byte(`{"options":{"notification_style":"bell"}}`)
	dataData := []byte(`{"options":`)
	require.NoError(t, os.WriteFile(globalPath, globalData, 0o600))
	require.NoError(t, os.WriteFile(dataPath, dataData, 0o600))

	migrateDisableNotifications()

	actualGlobal, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	actualData, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	require.Equal(t, globalData, actualGlobal)
	require.Equal(t, dataData, actualData)
}

func TestLockConfigMigrationFiles_UsesFixedOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.json")
	second := filepath.Join(dir, "b.json")
	held := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		unlock, err := migrate.LockFiles(second, first)
		if err != nil {
			firstDone <- err
			return
		}
		close(held)
		<-releaseFirst
		unlock()
		firstDone <- nil
	}()
	<-held

	secondDone := make(chan error, 1)
	go func() {
		unlock, err := migrate.LockFiles(first, second)
		if err == nil {
			unlock()
		}
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("opposite path order did not contend: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	require.NoError(t, <-firstDone)
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("opposite path order deadlocked")
	}
}

func bloatedModelConfig(field string) string {
	return fmt.Sprintf(`{%s,"providers":{"custom":{"models":%s}}}`, field, bloatedModelsJSON("model"))
}

func bloatedModelsJSON(prefix string) string {
	models := "["
	for i := 0; i <= migrate.ModelCacheMigrationThreshold; i++ {
		if i > 0 {
			models += ","
		}
		models += fmt.Sprintf(`{"id":"%s-%d"}`, prefix, i)
	}
	return models + "]"
}
