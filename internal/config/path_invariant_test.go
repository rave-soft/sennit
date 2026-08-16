package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigPathInvariant_MatchesReloadReadSet guards against "where we write"
// (configPath) and "where reload reads" (lookupConfigs, plus the
// separately-read workspace file) drifting apart. If configPath(scope) ever
// pointed somewhere reloadFromDisk does not also read, a write through
// that scope would silently vanish on the next reload -- exactly the kind of
// bug that made the refresh_singleflight_test.go fixtures non-hermetic:
// they wrote to a tmp file that reload never looked at, so the
// reload silently fell back to the real ~/.config/sennit/sennit.json.
//
// The check runs for several independent working directories so it is not
// an artifact of one particular temp-dir layout.
func TestConfigPathInvariant_MatchesReloadReadSet(t *testing.T) {
	for range 3 {
		workingDir := t.TempDir()

		envDir := t.TempDir()
		t.Setenv("SENNIT_GLOBAL_CONFIG", envDir)
		t.Setenv("SENNIT_GLOBAL_DATA", envDir)

		store, err := Load(workingDir, "", false)
		if err != nil {
			t.Fatalf("Load(%q): %v", workingDir, err)
		}

		// reloadFromDisk's read set: lookupConfigs(workingDir), plus the
		// workspace config it reads directly via os.ReadFile (store.go, and
		// mirrored in Load itself). Both compute the workspace path the same
		// way: DataDirectory/sennit.json.
		readSet := absSet(t, lookupConfigs(workingDir))
		workspaceRead := filepath.Join(store.Config().Options.DataDirectory, appName+".json")
		for p := range absSet(t, []string{workspaceRead}) {
			readSet[p] = struct{}{}
		}

		for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
			path, err := store.ConfigPath(scope)
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

// TestConfigPathInvariant_WorkspaceScopeReadableViaLookupConfigs guards the
// project-scope write target added for .sennit/sennit.json: configPath
// (ScopeWorkspace) must land inside lookupConfigs' own result set, not just
// the separately-read workspace file Load/reloadFromDisk layer on top. Before
// .sennit/sennit.json and .sennit/sennitrc were added as literal candidates in
// lookupConfigs' configNames, this only held via the extra "plus workspace
// path" union in TestConfigPathInvariant_MatchesReloadReadSet above; now
// that .sennit/sennit.json is a first-class candidate, the default-DataDirectory
// case should not need that union at all.
func TestConfigPathInvariant_WorkspaceScopeReadableViaLookupConfigs(t *testing.T) {
	workingDir := t.TempDir()

	envDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", envDir)
	t.Setenv("SENNIT_GLOBAL_DATA", envDir)

	// lookupConfigs only lists candidates that exist on disk, so create the
	// workspace file up front -- the invariant is about where reads and
	// writes land once the file is there, not about a file that has never
	// been written.
	if err := os.MkdirAll(filepath.Join(workingDir, ".sennit"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".sennit", "sennit.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store, err := Load(workingDir, "", false)
	if err != nil {
		t.Fatalf("Load(%q): %v", workingDir, err)
	}

	path, err := store.ConfigPath(ScopeWorkspace)
	if err != nil {
		t.Fatalf("configPath(ScopeWorkspace): %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", path, err)
	}

	readSet := absSet(t, lookupConfigs(workingDir))
	if _, ok := readSet[abs]; !ok {
		t.Errorf("configPath(ScopeWorkspace) = %q is not among lookupConfigs(workingDir) candidates; "+
			"expected .sennit/sennit.json to be a literal candidate there", abs)
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
