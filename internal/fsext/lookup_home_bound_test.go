package fsext

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/home"
	"github.com/stretchr/testify/require"
)

// $HOME is documented as an outer bound on the upward walk, but the check
// sat *after* the probe, so it only ever stopped the walk when the target
// happened to exist at home. With nothing there the walk carried on above
// home and would happily adopt a match from outside the user's tree.
//
// homedir is a package-level var in internal/home with no test seam, so
// rather than fake it these drive lookupClosest's walk directly — that
// callback is exactly where the ordering bug lived.
func TestLookupClosest_StopsAtHomeEvenWhenTargetIsAbsentThere(t *testing.T) {
	t.Parallel()

	// A directory "above home" holding the target. Reaching it at all is
	// the bug.
	aboveHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(aboveHome, "target.txt"), []byte("x"), 0o644))

	var visited []string
	walk := func(walkFn func(cwd string, owner int) error) error {
		// The order a real traverseUp would produce: a directory under
		// home, then home itself (no target in either), then what lies
		// beyond it.
		for _, dir := range []string{filepath.Join(home.Dir(), "project"), home.Dir(), aboveHome} {
			visited = append(visited, dir)
			if err := walkFn(dir, -1); err != nil {
				if errors.Is(err, filepath.SkipAll) {
					return nil
				}
				return err
			}
		}
		return nil
	}

	found, ok := lookupClosest("target.txt", walk)
	require.False(t, ok, "a match above $HOME must not be adopted")
	require.Empty(t, found)
	require.NotContains(t, visited, aboveHome, "the walk must stop at $HOME, not step past it")
}

// The bound must not cost a legitimate match below home.
func TestLookupClosest_StillFindsBelowHome(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "target.txt"), []byte("x"), 0o644))

	walk := func(walkFn func(cwd string, owner int) error) error {
		for _, d := range []string{dir, home.Dir()} {
			if err := walkFn(d, -1); err != nil {
				if errors.Is(err, filepath.SkipAll) {
					return nil
				}
				return err
			}
		}
		return nil
	}

	found, ok := lookupClosest("target.txt", walk)
	require.True(t, ok)
	require.Equal(t, filepath.Join(dir, "target.txt"), found)
}
