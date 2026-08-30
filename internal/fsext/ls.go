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

	if matchesFastIgnore(base, isDir, ignorePatterns) {
		return true
	}

	// Don't apply gitignore rules to the root directory itself.
	if path == dl.rootPath {
		return false
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
// semantics as ListDirectory. Its ignore state is an ancestor stack: walking a
// sibling releases the previous subtree's patterns instead of retaining a
// matcher for every directory in a wide tree.
func VisitDirectory(initialPath string, ignorePatterns []string, depth int, visit func(string)) error {
	walker := newDirectoryVisitState(initialPath)
	return filepath.Walk(initialPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(initialPath, path)
		if relErr != nil {
			return nil
		}
		level := 0
		if rel != "." {
			level = len(pathToComponents(rel))
		}
		if depth > 0 && level > depth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		walker.enterParent(filepath.Dir(path))
		isDir := info.IsDir()
		if walker.shouldIgnore(path, ignorePatterns, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if path != initialPath {
			outputPath := path
			if isDir {
				outputPath += string(filepath.Separator)
			}
			visit(outputPath)
		}
		if isDir {
			walker.enter(path)
		}
		return nil
	})
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
	if matchesFastIgnore(base, isDir, ignorePatterns) {
		return true
	}
	if path == s.rootPath {
		return false
	}
	relPath, err := filepath.Rel(s.rootPath, path)
	if err != nil {
		return false
	}
	return gitignore.NewMatcher(s.patterns).Match(pathToComponents(relPath), isDir)
}

// ListDirectory lists files and directories in the specified path.
func ListDirectory(initialPath string, ignorePatterns []string, depth, limit int) ([]string, bool, error) {
	found := csync.NewSlice[string]()
	dl := NewDirectoryLister(initialPath)

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
			return nil // Skip files we don't have permission to access
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
	return matches, truncated, nil
}
