package config

import (
	"path/filepath"
	"testing"
)

// TestConfigPathInvariant_MatchesReloadReadSet guards against "where we write"
// (configPath) and "where reload reads" (lookupConfigs, plus the
// separately-read workspace file) drifting apart. If configPath(scope) ever
// pointed somewhere reloadFromDiskLocked does not also read, a write through
// that scope would silently vanish on the next reload -- exactly the kind of
// bug that made the refresh_singleflight_test.go fixtures non-hermetic (see
// TECHDEBT.md): they wrote to a tmp file that reload never looked at, so the
// reload silently fell back to the real ~/.config/braid/braid.json.
//
// The check runs for several independent working directories so it is not
// an artifact of one particular temp-dir layout.
func TestConfigPathInvariant_MatchesReloadReadSet(t *testing.T) {
	for range 3 {
		workingDir := t.TempDir()

		envDir := t.TempDir()
		t.Setenv("BRAID_GLOBAL_CONFIG", envDir)
		t.Setenv("BRAID_GLOBAL_DATA", envDir)
		resetProviderState()
		t.Cleanup(resetProviderState)

		store, err := Load(workingDir, "", false)
		if err != nil {
			t.Fatalf("Load(%q): %v", workingDir, err)
		}

		// reloadFromDiskLocked's read set: lookupConfigs(workingDir), plus the
		// workspace config it reads directly via os.ReadFile (store.go, and
		// mirrored in Load itself). Both compute the workspace path the same
		// way: DataDirectory/braid.json.
		readSet := absSet(t, lookupConfigs(workingDir))
		workspaceRead := filepath.Join(store.Config().Options.DataDirectory, appName+".json")
		for p := range absSet(t, []string{workspaceRead}) {
			readSet[p] = struct{}{}
		}

		for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
			path, err := store.configPath(scope)
			if err != nil {
				t.Fatalf("configPath(%s): %v", scope, err)
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				t.Fatalf("filepath.Abs(%q): %v", path, err)
			}
			if _, ok := readSet[abs]; !ok {
				t.Errorf("configPath(%s) = %q is not in lookupConfigs(workingDir) ∪ {workspace path}; "+
					"a write to this scope would be lost on the next reload", scope, abs)
			}
		}
	}
}

// absSet resolves each path to its absolute form and returns the set.
func absSet(t *testing.T, paths []string) map[string]struct{} {
	t.Helper()
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("filepath.Abs(%q): %v", p, err)
		}
		set[abs] = struct{}{}
	}
	return set
}
