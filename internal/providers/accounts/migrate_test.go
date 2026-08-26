package accounts

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/oauth"
)

func TestMigrate_EmptyStore(t *testing.T) {
	t.Parallel()
	s := NewFileStore(filepath.Join(t.TempDir(), "accounts.json"))

	got, ok, err := Migrate(s, "codex", LegacyCredential{
		Token:     &oauth.Token{AccessToken: "at"},
		AccountID: "chatgpt-123",
		Email:     "me@example.com",
		ProxyURL:  "http://proxy:8080",
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "acc_chatgpt-123", got.ID)
	require.Equal(t, "me@example.com", got.Label)
	require.Equal(t, "chatgpt-123", got.AccountID)
	require.Equal(t, "me@example.com", got.Email)
	require.Equal(t, "http://proxy:8080", got.ProxyURL)
	require.Equal(t, &oauth.Token{AccessToken: "at"}, got.Token)

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, got, list[0])
}

func TestMigrate_AlreadyMigratedIsNoop(t *testing.T) {
	t.Parallel()
	s := NewFileStore(filepath.Join(t.TempDir(), "accounts.json"))

	_, ok, err := Migrate(s, "codex", LegacyCredential{APIKey: "$KEY"})
	require.NoError(t, err)
	require.True(t, ok)

	// Simulate the user having edited the migrated account since.
	require.NoError(t, s.Upsert("codex", Account{ID: "acc_1", APIKey: "$KEY", Label: "Edited"}))

	got, ok, err := Migrate(s, "codex", LegacyCredential{APIKey: "$OTHER_KEY"})
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, Account{}, got)

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "Edited", list[0].Label)
	require.Equal(t, "$KEY", list[0].APIKey)
}

func TestMigrate_EmptyCredentialIsNoop(t *testing.T) {
	t.Parallel()
	s := NewFileStore(filepath.Join(t.TempDir(), "accounts.json"))

	got, ok, err := Migrate(s, "codex", LegacyCredential{})
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, Account{}, got)

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestMigrate_BothCredentialsIsError(t *testing.T) {
	t.Parallel()
	s := NewFileStore(filepath.Join(t.TempDir(), "accounts.json"))

	_, ok, err := Migrate(s, "codex", LegacyCredential{
		APIKey: "$KEY",
		Token:  &oauth.Token{AccessToken: "at"},
	})
	require.Error(t, err)
	require.False(t, ok)
}

func TestMigrate_OAuthUsesAccountIDAndEmail(t *testing.T) {
	t.Parallel()
	s := NewFileStore(filepath.Join(t.TempDir(), "accounts.json"))

	got, ok, err := Migrate(s, "codex", LegacyCredential{
		Token:     &oauth.Token{AccessToken: "at"},
		AccountID: "chatgpt-123",
		Email:     "me@example.com",
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "acc_chatgpt-123", got.ID)
	require.Equal(t, "me@example.com", got.Label)
}

func TestMigrate_APIKeyWithoutAccountIDUsesDefaultLabel(t *testing.T) {
	t.Parallel()
	s := NewFileStore(filepath.Join(t.TempDir(), "accounts.json"))

	got, ok, err := Migrate(s, "openai", LegacyCredential{APIKey: "$OPENAI_KEY"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "acc_1", got.ID)
	require.Equal(t, "Default", got.Label)
}

func TestMigrate_ProxyURLNoneIsPreserved(t *testing.T) {
	t.Parallel()
	s := NewFileStore(filepath.Join(t.TempDir(), "accounts.json"))

	got, ok, err := Migrate(s, "openai", LegacyCredential{APIKey: "$KEY", ProxyURL: "none"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "none", got.ProxyURL)
}
