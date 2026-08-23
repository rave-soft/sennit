package fsext

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rave-soft/sennit/internal/home"
)

// Lookup searches for a target files or directories starting from dir
// and walking up the directory tree until filesystem root is reached.
// It also checks the ownership of files to ensure that the search does
// not cross ownership boundaries. It skips ownership mismatches without
// errors.
// Returns full paths to fount targets.
// The search includes the starting directory itself.
func Lookup(dir string, targets ...string) ([]string, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	return lookupAll(targets, func(walkFn func(cwd string, owner int) error) error {
		return traverseUp(dir, walkFn)
	})
}

// LookupClosest searches for a target file or directory starting from dir
// and walking up the directory tree until found or root or home is reached.
// It also checks the ownership of files to ensure that the search does
// not cross ownership boundaries.
// Returns the full path to the target if found, empty string and false otherwise.
// The search includes the starting directory itself.
func LookupClosest(dir, target string) (string, bool) {
	return lookupClosest(target, func(walkFn func(cwd string, owner int) error) error {
		return traverseUp(dir, walkFn)
	})
}

// LookupClosestBounded behaves like LookupClosest but constrains the
// upward search to stopDir. The walk inspects dir, then each ancestor up
// to and including stopDir, then terminates regardless of whether the
// target was found. Use this when the caller wants to avoid adopting
// matches from outside a project boundary (for example a sibling
// worktree or a parent project).
//
// If stopDir is empty, only dir itself is searched. If stopDir is not an
// ancestor of dir, the walk still terminates at the filesystem root.
// The $HOME and ownership safeguards from LookupClosest are preserved
// as outer bounds.
func LookupClosestBounded(dir, stopDir, target string) (string, bool) {
	return lookupClosest(target, func(walkFn func(cwd string, owner int) error) error {
		return traverseUpBounded(dir, stopDir, walkFn)
	})
}

// lookupClosest is the shared body of [LookupClosest] and
// [LookupClosestBounded]: walk drives the upward traversal (unbounded or
// stopDir-bounded), and this looks for target at each step, stopping at
// the first match or at $HOME, whichever comes first.
func lookupClosest(target string, walk func(walkFn func(cwd string, owner int) error) error) (string, bool) {
	var found string

	err := walk(func(cwd string, owner int) error {
		fpath := filepath.Join(cwd, target)

		err := probeEnt(fpath, owner)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("error probing file %s: %w", fpath, err)
		}

		if Canonical(cwd) == Canonical(home.Dir()) {
			return filepath.SkipAll
		}

		found = fpath
		return filepath.SkipAll
	})

	return found, err == nil && found != ""
}

// LookupBounded behaves like Lookup but constrains the upward search to
// stopDir. The walk inspects dir, then each ancestor up to and including
// stopDir, then terminates. If stopDir is empty, only dir itself is
// searched.
func LookupBounded(dir, stopDir string, targets ...string) ([]string, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	return lookupAll(targets, func(walkFn func(cwd string, owner int) error) error {
		return traverseUpBounded(dir, stopDir, walkFn)
	})
}

// lookupAll is the shared body of [Lookup] and [LookupBounded]: walk
// drives the upward traversal (unbounded or stopDir-bounded), and this
// probes every target at each step, collecting every hit rather than
// stopping at the first (unlike [lookupClosest]).
func lookupAll(targets []string, walk func(walkFn func(cwd string, owner int) error) error) ([]string, error) {
	var found []string

	err := walk(func(cwd string, owner int) error {
		for _, target := range targets {
			fpath := filepath.Join(cwd, target)
			err := probeEnt(fpath, owner)

			// skip to the next file on permission denied
			if errors.Is(err, os.ErrNotExist) ||
				errors.Is(err, os.ErrPermission) {
				continue
			}

			if err != nil {
				return fmt.Errorf("error probing file %s: %w", fpath, err)
			}

			found = append(found, fpath)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return found, nil
}

// traverseUp walks up from given directory up until filesystem root reached.
// It passes absolute path of current directory and staring directory owner ID
// to callback function. It is up to user to check ownership.
func traverseUp(dir string, walkFn func(dir string, owner int) error) error {
	cwd, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("cannot convert CWD to absolute path: %w", err)
	}

	owner, err := Owner(dir)
	if err != nil {
		return fmt.Errorf("cannot get ownership: %w", err)
	}

	for {
		err := walkFn(cwd, owner)
		if err == nil || errors.Is(err, filepath.SkipDir) {
			parent := filepath.Dir(cwd)
			if parent == cwd {
				return nil
			}

			cwd = parent
			continue
		}

		if errors.Is(err, filepath.SkipAll) {
			return nil
		}

		return err
	}
}

// traverseUpBounded walks up from dir, visiting each ancestor up to and
// including stopDir, then terminates. If stopDir is empty, only dir
// itself is visited; callers that want an unbounded walk should use
// traverseUp instead. If stopDir is set but is not an ancestor of dir
// the walk still stops at the filesystem root, so callers cannot
// accidentally produce an infinite walk by passing a sibling path.
//
// Boundary comparison is performed against symlink-resolved paths so
// that callers passing logically equivalent paths (a symlinked /var vs
// the underlying /private/var, for example) still terminate at the
// expected directory.
func traverseUpBounded(dir, stopDir string, walkFn func(dir string, owner int) error) error {
	cwd, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("cannot convert CWD to absolute path: %w", err)
	}

	stop := cwd
	if stopDir != "" {
		stop, err = filepath.Abs(stopDir)
		if err != nil {
			return fmt.Errorf("cannot convert stop dir to absolute path: %w", err)
		}
	}
	canonStop := Canonical(stop)

	owner, err := Owner(dir)
	if err != nil {
		return fmt.Errorf("cannot get ownership: %w", err)
	}

	for {
		err := walkFn(cwd, owner)
		if err == nil || errors.Is(err, filepath.SkipDir) {
			if Canonical(cwd) == canonStop {
				return nil
			}

			parent := filepath.Dir(cwd)
			if parent == cwd {
				return nil
			}

			cwd = parent
			continue
		}

		if errors.Is(err, filepath.SkipAll) {
			return nil
		}

		return err
	}
}

// Canonical returns a path spelling that two logically identical paths
// share, so callers can compare paths with == instead of a raw string
// compare. It first makes path absolute — two relative spellings of the
// same directory (e.g. from different working directories) must not stay
// distinguishable — then resolves it with filepath.EvalSymlinks, which
// does the rest of the real work: besides resolving symlinks (a macOS
// /tmp vs /private/tmp, say), on Windows it also queries the filesystem
// for each component's on-disk spelling, which collapses the two path
// forms Windows APIs hand back inconsistently — an 8.3 short name (as
// t.TempDir returns) and the long form (as os.Getwd or `git rev-parse`
// return) — into one.
//
// If path itself does not exist — the common case is a worktree or file
// that has just been deleted — EvalSymlinks cannot resolve it at all, even
// though an aliased ancestor further up is exactly what needs resolving
// (macOS's /var, or a Windows short name, are both properties of a
// directory, not of the missing leaf). So on failure this walks up to the
// nearest ancestor that does exist, resolves that, and rejoins the
// non-existent tail onto it, before falling back to a plain Clean (and, on
// Windows, case-folding, since Windows paths are case-insensitive even
// before anything is created at them) if nothing above path resolves
// either.
func Canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	clean := canonicalMissing(abs)
	if runtime.GOOS == "windows" {
		return strings.ToLower(clean)
	}
	return clean
}

// canonicalMissing resolves the nearest existing ancestor of abs (an
// absolute, cleaned path) and rejoins the components below it that
// EvalSymlinks could not reach because they do not exist on disk. abs
// itself is included in the walk, so a directory that exists but whose
// EvalSymlinks call failed for some other reason still gets a chance to
// resolve one level up.
//
// filepath.Dir reaches the filesystem root (or, on Windows, a drive root)
// in a bounded number of steps and then repeats it, which is the loop's
// only termination check — so this always halts, even when no ancestor of
// abs exists at all (a filesystem error, an unmounted drive) — it just
// falls back to a clean of abs in that case.
func canonicalMissing(abs string) string {
	var tail []string
	dir := abs
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(abs)
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
	}
}

// probeEnt checks if entity at given path exists and belongs to given owner
func probeEnt(fspath string, owner int) error {
	_, err := os.Stat(fspath)
	if err != nil {
		return fmt.Errorf("cannot stat %s: %w", fspath, err)
	}

	// special case for ownership check bypass
	if owner == -1 {
		return nil
	}

	fowner, err := Owner(fspath)
	if err != nil {
		return fmt.Errorf("cannot get ownership for %s: %w", fspath, err)
	}

	if fowner != owner {
		return os.ErrPermission
	}

	return nil
}
