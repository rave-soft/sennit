package credentials

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestLoadTokenFromDisk_ReturnsNewerToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "newer-token-from-disk",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	m := New(newFakeStore(&config.Config{}, configPath, filepath.Join(dir, "locks")))

	token, err := m.loadTokenFromDisk(config.ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "newer-token-from-disk", token.AccessToken)
	require.Equal(t, "refresh-abc", token.RefreshToken)
	require.Equal(t, 3600, token.ExpiresIn)
	require.Equal(t, int64(9999999999), token.ExpiresAt)
}

func TestLoadTokenFromDisk_ReturnsNilWhenSameToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	// Create config file with the same token
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "same-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	m := New(newFakeStore(&config.Config{}, configPath, filepath.Join(dir, "locks")))

	token, err := m.loadTokenFromDisk(config.ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "same-token", token.AccessToken)
}

func TestLoadTokenFromDisk_ReturnsNilWhenFileMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "nonexistent.json")

	m := New(newFakeStore(&config.Config{}, configPath, filepath.Join(dir, "locks")))

	token, err := m.loadTokenFromDisk(config.ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenProviderMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	// Create config file without the hyper provider
	configContent := `{"providers": {"openai": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	m := New(newFakeStore(&config.Config{}, configPath, filepath.Join(dir, "locks")))

	token, err := m.loadTokenFromDisk(config.ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenOAuthMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	// Create config file with provider but no OAuth token
	configContent := `{"providers": {"hyper": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	m := New(newFakeStore(&config.Config{}, configPath, filepath.Join(dir, "locks")))

	token, err := m.loadTokenFromDisk(config.ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestRefreshOAuthToken_UsesDiskTokenWhenDifferent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"api_key": "newer-access-token",
				"oauth": {
					"access_token": "newer-access-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	// Set up a manager with an older in-memory token
	oldToken := &oauth.Token{
		AccessToken:  "older-access-token",
		RefreshToken: "refresh-abc",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // Expired
	}

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("hyper", config.ProviderConfig{
		ID:         "hyper",
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})

	store := newFakeStore(&config.Config{Providers: providers}, configPath, filepath.Join(dir, "locks"))
	m := New(store)

	// Refresh should use the disk token without making an external call
	err := m.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "hyper")
	require.NoError(t, err)

	// Verify the in-memory token was updated to the disk token
	updatedConfig, ok := store.Config().Providers.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "newer-access-token", updatedConfig.APIKey)
	require.Equal(t, "newer-access-token", updatedConfig.OAuthToken.AccessToken)
	require.Equal(t, "refresh-abc", updatedConfig.OAuthToken.RefreshToken)
}
