package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRefreshLockPath_OrdinaryProviderIDUnchanged pins the fallback: a
// provider ID made only of letters, digits, '.', '-', and '_' — the
// ordinary case — must still resolve to "<id>.refresh.lock" in the locks
// directory, so the escaping added for hostile IDs does not move the lock
// file for any provider ID already in use.
func TestRefreshLockPath_OrdinaryProviderIDUnchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &ConfigStore{globalDataPath: filepath.Join(dir, "sennit.json")}

	got := store.RefreshLockPath("api.example.com")
	want := filepath.Join(dir, "locks", "api.example.com.refresh.lock")
	require.Equal(t, want, got)
}

// TestRefreshLockPath_HostileProviderIDStaysInsideLocksDir is the
// regression test for the path-escape bug: RefreshLockPath used to
// interpolate providerID directly into a filename, so a custom provider ID
// containing '/' or '..' could walk the result outside the locks
// directory. A provider ID is free text — see ProviderFieldKey's doc
// comment — so nothing upstream stops one from containing either.
func TestRefreshLockPath_HostileProviderIDStaysInsideLocksDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &ConfigStore{globalDataPath: filepath.Join(dir, "sennit.json")}
	locksDir := filepath.Join(dir, "locks")

	for _, providerID := range []string{
		"../../evil",
		"../../../etc/cron.d/evil",
		"../escape",
		"a/../../b",
	} {
		got := store.RefreshLockPath(providerID)
		// The result must be a single filename directly inside locksDir: no
		// path separator surviving into providerID's contribution can turn
		// this into a nested or escaping path, so the parent directory of
		// the returned path must be exactly locksDir, byte for byte.
		require.Equal(t, locksDir, filepath.Dir(got),
			"provider ID %q escaped the locks directory: got %q", providerID, got)
	}
}
