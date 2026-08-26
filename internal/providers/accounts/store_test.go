package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/oauth"
)

func TestFileStore_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := NewFileStore(path)

	want := Account{
		ID:        "acct-1",
		Label:     "Work",
		AccountID: "chatgpt-123",
		Email:     "me@example.com",
		Token: &oauth.Token{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresIn:    3600,
			ExpiresAt:    1234567890,
		},
		Usage: Usage{
			Plan: "pro",
			Primary: UsageWindow{
				UsedPercent:   42,
				WindowMinutes: 60,
				ResetsAt:      time.Unix(1700000000, 0),
			},
			CapturedAt: time.Unix(1699999999, 0),
		},
	}
	require.NoError(t, s.Upsert("codex", want))

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, want.ID, list[0].ID)
	require.Equal(t, want.Token, list[0].Token)
	require.True(t, want.Usage.CapturedAt.Equal(list[0].Usage.CapturedAt))
	require.True(t, want.Usage.Primary.ResetsAt.Equal(list[0].Usage.Primary.ResetsAt))
	require.Equal(t, want.Usage.Plan, list[0].Usage.Plan)
	require.Equal(t, want.Usage.Primary.UsedPercent, list[0].Usage.Primary.UsedPercent)

	got, ok, err := s.Get("codex", "acct-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want.Label, got.Label)
	require.Equal(t, want.AccountID, got.AccountID)
	require.Equal(t, want.Email, got.Email)
}

func TestFileStore_MissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := NewFileStore(path)

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Empty(t, list)

	_, ok, err := s.Get("codex", "nope")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestFileStore_CorruptJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	s := NewFileStore(path)
	_, err := s.List("codex")
	require.Error(t, err)
}

func TestFileStore_UnknownVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	data, err := json.Marshal(fileFormat{Version: storeVersion + 1, Accounts: map[string][]Account{}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	s := NewFileStore(path)
	_, err = s.List("codex")
	require.Error(t, err)
}

func TestFileStore_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix file permissions")
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := NewFileStore(path)
	require.NoError(t, s.Upsert("codex", Account{ID: "a1", APIKey: "$KEY"}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFileStore_ConcurrentUpsertsAcrossStores(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")

	const n = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := NewFileStore(path)
			id := "acct-" + string(rune('a'+i))
			if err := s.Upsert("codex", Account{ID: id, APIKey: "$KEY"}); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	require.Empty(t, errs)

	final := NewFileStore(path)
	list, err := final.List("codex")
	require.NoError(t, err)
	require.Len(t, list, n)
}

func TestFileStore_CreatesParentDir(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "dir", "accounts.json")
	s := NewFileStore(path)

	require.NoError(t, s.Upsert("codex", Account{ID: "a1", APIKey: "$KEY"}))

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "a1", list[0].ID)
}

func TestFileStore_ListReturnsCopy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := NewFileStore(path)

	require.NoError(t, s.Upsert("codex", Account{ID: "a1", APIKey: "$KEY", Label: "Original"}))

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Len(t, list, 1)
	list[0].Label = "Mutated"

	again, err := s.List("codex")
	require.NoError(t, err)
	require.Equal(t, "Original", again[0].Label)
}

func TestFileStore_UpsertUpdatesInPlace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := NewFileStore(path)

	require.NoError(t, s.Upsert("codex", Account{ID: "a1", APIKey: "$KEY1", Label: "First"}))
	require.NoError(t, s.Upsert("codex", Account{ID: "a2", APIKey: "$KEY2", Label: "Second"}))
	require.NoError(t, s.Upsert("codex", Account{ID: "a1", APIKey: "$KEY1", Label: "First Updated"}))

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "a1", list[0].ID)
	require.Equal(t, "First Updated", list[0].Label)
	require.Equal(t, "a2", list[1].ID)
}

func TestFileStore_RemoveIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := NewFileStore(path)

	require.NoError(t, s.Upsert("codex", Account{ID: "a1", APIKey: "$KEY"}))
	require.NoError(t, s.Remove("codex", "a1"))
	require.NoError(t, s.Remove("codex", "a1")) // second removal: not an error

	list, err := s.List("codex")
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestFileStore_RecordUsageMissingAccount(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := NewFileStore(path)

	err := s.RecordUsage("codex", "nope", Usage{Plan: "pro"})
	require.Error(t, err)
}

func TestFileStore_RecordUsage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := NewFileStore(path)

	require.NoError(t, s.Upsert("codex", Account{ID: "a1", APIKey: "$KEY"}))
	require.NoError(t, s.RecordUsage("codex", "a1", Usage{Plan: "pro", Primary: UsageWindow{UsedPercent: 10, WindowMinutes: 60}}))

	got, ok, err := s.Get("codex", "a1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "pro", got.Usage.Plan)
	require.Equal(t, 10, got.Usage.Primary.UsedPercent)
}
