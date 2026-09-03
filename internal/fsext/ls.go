package fsext

import (
	"cmp"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charlievieth/fastwalk"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/home"
)

// fastIgnoreDirs is a set of directory names that are always ignored,
// built from the same commonIgnoredDirNames as [SkipHidden] (fileutil.go)
// uses. This provides O(1) lookup for common cases to avoid expensive
// pattern matching.
var fastIgnoreDirs = commonIgnoredDirSet()

// commonIgnorePatterns contains commonly ignored files and directories.
// Note: Exact directory names that are in fastIgnoreDirs are handled there for O(1) lookup.
// This list contains wildcard patterns and file-specific patterns.
var commonIgnorePatterns = sync.OnceValue(func() []gitignore.Pattern {
	patterns := []string{
		// IDE and editor files (wildcards)
		"*.swp",
		"*.swo",
		"*~",
		".DS_Store",
		"Thumbs.db",

		// Build artifacts (non-fastIgnoreDirs)
		"target",
		"build",
		"dist",
		"out",
		"bin",
		"obj",
		"*.o",
		"*.so",
		"*.dylib",
		"*.dll",
		"*.exe",

		// Logs and temporary files (wildcards)
		"*.log",
		"*.tmp",
		"*.temp",

		// Language-specific (wildcards and non-fastIgnoreDirs)
		"*.pyc",
		"*.pyo",
		"vendor",
		"Cargo.lock",
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
	}
	return parsePatterns(patterns, nil)
})

// gitGlobalIgnorePatterns returns patterns from git's global excludes file
// (core.excludesFile), following git's config resolution order.
var gitGlobalIgnorePatterns = sync.OnceValue(func() []gitignore.Pattern {
	cfg, err := gitconfig.LoadConfig(gitconfig.GlobalScope)
	if err != nil {
		slog.Debug("Failed to load global git config", "error", err)
		return nil
	}

	excludesFilePath := cmp.Or(
		cfg.Raw.Section("core").Options.Get("excludesfile"),
		filepath.Join(home.Config(), "git", "ignore"),
	)
	excludesFilePath = home.Long(excludesFilePath)

	bts, err := os.ReadFile(excludesFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("Failed to read git global excludes file", "path", excludesFilePath, "error", err)
		}
		return nil
	}

	return parsePatterns(strings.Split(string(bts), "\n"), nil)
})

// globalIgnorePatterns returns patterns from the user's
// ~/.config/sennit/ignore file.
var globalIgnorePatterns = sync.OnceValue(func() []gitignore.Pattern {
	name := filepath.Join(home.Config(), brand.Slug, "ignore")
	bts, err := os.ReadFile(name)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("Failed to read sennit global ignore file", "path", name, "error", err)
		}
		return nil
	}
	lines := strings.Split(string(bts), "\n")
	return parsePatterns(lines, nil)
})

// parsePatterns parses gitignore pattern strings into Pattern objects.
// domain is the path components where the patterns are defined (nil for global).
func parsePatterns(lines []string, domain []string) []gitignore.Pattern {
	var patterns []gitignore.Pattern
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, gitignore.ParsePattern(line, domain))
	}
	return patterns
}

type directoryLister struct {
	rootPath string
}

func NewDirectoryLister(rootPath string) *directoryLister {
	return &directoryLister{rootPath: rootPath}
}

// pathToComponents splits a path into its components for gitignore matching.
func pathToComponents(path string) []string {
	path = filepath.ToSlash(path)
	if path == "" || path == "." {
		return nil
	}
	return strings.Split(path, "/")
}

// getDirPatterns parses the ignore files for one ancestor. Directory walks do
// not retain per-directory state, so memory stays proportional to the active
// path and the configured page size even for very wide trees.
func (dl *directoryLister) getDirPatterns(dir string) []gitignore.Pattern {
	var allPatterns []gitignore.Pattern
	relPath, _ := filepath.Rel(dl.rootPath, dir)
	var domain []string
	if relPath != "" && relPath != "." {
		domain = pathToComponents(relPath)
	}
	for _, ignoreFile := range []string{".gitignore", brand.IgnoreFile} {
		ignPath := filepath.Join(dir, ignoreFile)
		if content, err := os.ReadFile(ignPath); err == nil {
			allPatterns = append(allPatterns, parsePatterns(strings.Split(string(content), "\n"), domain)...)
		}
	}
	return allPatterns
}

// getCombinedMatcher builds one bounded ancestor view for the current path.
// It deliberately avoids a matcher cache whose retained pattern copies grow
// quadratically with tree depth and without bound with tree width.
func (dl *directoryLister) getCombinedMatcher(dir string) gitignore.Matcher {
	allPatterns := append([]gitignore.Pattern{}, commonIgnorePatterns()...)
	allPatterns = append(allPatterns, gitGlobalIgnorePatterns()...)
	allPatterns = append(allPatterns, globalIgnorePatterns()...)
	relDir, _ := filepath.Rel(dl.rootPath, dir)
	var pathParts []string
	if relDir != "" && relDir != "." {
		pathParts = pathToComponents(relDir)
	}
	currentPath := dl.rootPath
	allPatterns = append(allPatterns, dl.getDirPatterns(currentPath)...)
	for _, part := range pathParts {
		currentPath = filepath.Join(currentPath, part)
		allPatterns = append(allPatterns, dl.getDirPatterns(currentPath)...)
	}
	return gitignore.NewMatcher(allPatterns)
}

// matchesFastIgnore reports whether base should be ignored purely from its
// name — the O(1) common-directory lookup and the caller-supplied glob
// patterns — without consulting any gitignore matcher. Both
// directoryLister.shouldIgnore and directoryVisitState.shouldIgnore start
// with this same check; they diverge only in how they then consult
// gitignore, since each maintains ancestor patterns differently to suit
// its own traversal (directoryLister rebuilds a combined matcher per
// parent directory on demand, directoryVisitState maintains an explicit
// push/pop pattern stack as the walk descends and ascends), so that part
// is not merged.
func matchesFastIgnore(base string, isDir bool, ignorePatterns []string) bool {
	if isDir && fastIgnoreDirs[base] {
		return true
	}
	for _, pattern := range ignorePatterns {
		if matched, err := filepath.Match(pattern, base); err == nil && matched {
			return true
		}
	}
	return false
}

// shouldIgnore checks if a path should be ignored based on gitignore rules.
// This uses a combined matcher containing all ancestor patterns.
func (dl *directoryLister) shouldIgnore(path string, ignorePatterns []string, isDir bool) bool {
	base := filepath.Base(path)

	// Don't apply ignore rules to the root directory itself — including the
	// fast-ignore set. A walk rooted at "vendor" (or "dist", "bin", etc.)
	// was explicitly asked for by name; the ignore set exists to keep such
	// directories out of *recursive* results, not to make them unlistable.
	if path == dl.rootPath {
		return false
	}

	if matchesFastIgnore(base, isDir, ignorePatterns) {
		return true
	}

	relPath, err := filepath.Rel(dl.rootPath, path)
	if err != nil {
		relPath = path
	}

	pathComponents := pathToComponents(relPath)
	if len(pathComponents) == 0 {
		return false
	}

	// Get the combined matcher for the parent directory.
	parentDir := filepath.Dir(path)
	matcher := dl.getCombinedMatcher(parentDir)

	if matcher.Match(pathComponents, isDir) {
		slog.Debug("Ignoring path", "path", relPath)
		return true
	}

	return false
}

// VisitDirectory streams directory entries using the same ignore and depth
// semantics as ListDirectory, and the same symlink rule as its fastwalk pass:
// the root is followed if it is itself a directory symlink, but a symlink
// found while descending is reported as a leaf and never followed. Its ignore
// state is an ancestor stack: walking a sibling releases the previous
// subtree's patterns instead of retaining a matcher for every directory in a
// wide tree.
//
// incomplete reports whether any path was skipped because it could not be
// read — a removed directory, ReadDir failing on a wide tree (EMFILE/ENFILE),
// a transient I/O error on a network mount, not only a permissions denial.
// Such an entry is left out of the results entirely rather than failing the
// whole walk, so callers must surface incomplete themselves or a partially
// read tree looks like a complete, merely small one.
func VisitDirectory(initialPath string, ignorePatterns []string, depth int, visit func(string)) (incomplete bool, err error) {
	walker := newDirectoryVisitState(initialPath)

	// filepath.Walk lstats its root, so a directory symlink root reports
	// IsDir()==false and the walk stops after that single (dropped) entry.
	// Resolve it once up front and walk the target instead, translating
	// every visited path back to initialPath's own namespace below — this
	// is the same "follow the root, not what's inside it" rule ListDirectory
	// gets for free from fastwalk stat-ing (not lstat-ing) its root.
	walkRoot := initialPath
	if lst, lerr := os.Lstat(initialPath); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
		if target, terr := filepath.EvalSymlinks(initialPath); terr == nil {
			if tinfo, serr := os.Stat(target); serr == nil && tinfo.IsDir() {
				walkRoot = target
			}
		}
	}

	walkErr := filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			incomplete = true
			return nil
		}
		rel, relErr := filepath.Rel(walkRoot, path)
		if relErr != nil {
			return nil
		}
		level := 0
		virtualPath := initialPath
		if rel != "." {
			level = len(pathToComponents(rel))
			virtualPath = filepath.Join(initialPath, rel)
		}
		if depth > 0 && level > depth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		walker.enterParent(filepath.Dir(virtualPath))
		isDir := info.IsDir()
		if walker.shouldIgnore(virtualPath, ignorePatterns, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if virtualPath != initialPath {
			outputPath := virtualPath
			if isDir {
				outputPath += string(filepath.Separator)
			}
			visit(outputPath)
		}
		if isDir {
			walker.enter(virtualPath)
		}
		return nil
	})
	return incomplete, walkErr
}

type directoryVisitFrame struct {
	dir          string
	patternCount int
}

type directoryVisitState struct {
	rootPath string
	patterns []gitignore.Pattern
	frames   []directoryVisitFrame
}

func newDirectoryVisitState(rootPath string) *directoryVisitState {
	state := &directoryVisitState{rootPath: rootPath}
	state.patterns = append(state.patterns, commonIgnorePatterns()...)
	state.patterns = append(state.patterns, gitGlobalIgnorePatterns()...)
	state.patterns = append(state.patterns, globalIgnorePatterns()...)
	return state
}

func (s *directoryVisitState) enterParent(parent string) {
	for len(s.frames) > 0 && s.frames[len(s.frames)-1].dir != parent {
		frame := s.frames[len(s.frames)-1]
		s.patterns = s.patterns[:frame.patternCount]
		s.frames = s.frames[:len(s.frames)-1]
	}
}

func (s *directoryVisitState) enter(dir string) {
	before := len(s.patterns)
	dl := directoryLister{rootPath: s.rootPath}
	s.patterns = append(s.patterns, dl.getDirPatterns(dir)...)
	s.frames = append(s.frames, directoryVisitFrame{dir: dir, patternCount: before})
}

func (s *directoryVisitState) shouldIgnore(path string, ignorePatterns []string, isDir bool) bool {
	base := filepath.Base(path)
	// Same root exemption as directoryLister.shouldIgnore: keep this ahead
	// of matchesFastIgnore or a walk rooted at e.g. "vendor" skips itself.
	if path == s.rootPath {
		return false
	}
	if matchesFastIgnore(base, isDir, ignorePatterns) {
		return true
	}
	relPath, err := filepath.Rel(s.rootPath, path)
	if err != nil {
		return false
	}
	return gitignore.NewMatcher(s.patterns).Match(pathToComponents(relPath), isDir)
}

// ListDirectory lists files and directories in the specified path. The
// returned bool is true if the results were cut short — either by the
// limit/depth (the ordinary case) or because part of the tree could not be
// read (see the comment on the walk callback below). ListDirectory has
// callers outside fsext that only look at this single bool, so an unreadable
// subtree is folded into it rather than reported through a separate signal:
// that keeps a partially read tree from ever being reported as a complete,
// merely small one, without requiring every caller to be touched.
func ListDirectory(initialPath string, ignorePatterns []string, depth, limit int) ([]string, bool, error) {
	found := csync.NewSlice[string]()
	dl := NewDirectoryLister(initialPath)
	incomplete := false

	slog.Debug("Listing directory", "path", initialPath, "depth", depth, "limit", limit, "ignorePatterns", ignorePatterns)

	conf := fastwalk.Config{
		// Do not follow symlinks: this must match the glob path
		// (globWithDoubleStar, fileutil.go), which deliberately does not
		// follow them either, so the two entry points agree on what one
		// tree contains. Following would also let the walk escape
		// initialPath (into module caches, the nix store, $HOME, etc.)
		// and chase symlink cycles, which is slow and can hang.
		Follow:   false,
		ToSlash:  fastwalk.DefaultToSlash(),
		Sort:     fastwalk.SortDirsFirst,
		MaxDepth: depth,
	}

	err := fastwalk.Walk(&conf, initialPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// The entry is skipped, not the whole walk: a single unreadable
			// file or directory (removed mid-walk, EMFILE/ENFILE on a wide
			// tree, a permissions denial, a transient I/O error on a
			// network mount) shouldn't fail a listing of everything else.
			// But skipping silently would make a partially read tree look
			// like a complete, merely small one, so record it instead.
			incomplete = true
			return nil
		}

		isDir := d.IsDir()
		if dl.shouldIgnore(path, ignorePatterns, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		if path != initialPath {
			if isDir {
				path = path + string(filepath.Separator)
			}
			found.Append(path)
		}

		// Stop only once an entry beyond limit has actually been collected
		// (>, not >=): stopping the instant found.Len() reaches limit can't
		// distinguish "exactly limit entries total" from "there are more",
		// and unconditionally reporting SkipAll as truncation below made
		// the former look truncated too.
		if limit > 0 && found.Len() > limit {
			return filepath.SkipAll
		}

		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, false, err
	}

	matches, truncated := truncate(slices.Collect(found.Seq()), limit)
	return matches, truncated || incomplete, nil
}
