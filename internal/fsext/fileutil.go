package fsext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/charlievieth/fastwalk"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/home"
)

// commonIgnoredDirNames is the single source of truth for directory names
// fsext skips by plain exact match, independent of gitignore pattern
// semantics — [SkipHidden]'s path-segment scan below and ls.go's
// fastIgnoreDirs O(1) short-circuit before it falls through to full
// gitignore matching. It is the union of what those two lists had
// independently drifted to include, minus a few names that turned out
// to be wrong for either list to carry:
//
//   - "generated" and "logs" are deliberately left out. Both are ordinary
//     things a user or the agent looks in on purpose (generated code is
//     still code to read and grep; "check the logs" is a routine ask), so
//     silently vanishing them from ListDirectory/glob results — with no
//     way to tell why — was a bug in the old commonIgnoredDirs, not a
//     feature. Do not add them back.
//   - "OrbStack", ".local", and ".share" are also left out. They only make
//     sense as ignores relative to a user's home directory, but this list
//     is matched against every segment of a workspace-relative path — a
//     project directory literally named "share" (or a vendored one) would
//     be hidden for a reason that has nothing to do with why it was
//     probably added (someone browsing $HOME with the same walk). Nothing
//     in this codebase points a search at $HOME routinely enough to
//     justify that collateral risk.
var commonIgnoredDirNames = []string{
	brand.DataDir,
	".git", ".svn", ".hg", ".bzr",
	".vscode", ".idea",
	"node_modules", "__pycache__", ".pytest_cache",
	".cache", ".tmp", ".Trash", ".Spotlight-V100", ".fseventsd",
	"vendor", "dist", "build", "target", "bin", "obj", "out",
	"coverage",
	"bower_components", "jspm_packages",
}

// commonIgnoredDirSet is commonIgnoredDirNames as a lookup set. Computed
// once (not on every SkipHidden call, which glob's walk drives once per
// candidate path) and reused by ls.go's fastIgnoreDirs as well.
var commonIgnoredDirSet = sync.OnceValue(func() map[string]bool {
	m := make(map[string]bool, len(commonIgnoredDirNames))
	for _, name := range commonIgnoredDirNames {
		m[name] = true
	}
	return m
})

func SkipHidden(path string) bool {
	// Check for hidden files (starting with a dot)
	base := filepath.Base(path)
	if base != "." && strings.HasPrefix(base, ".") {
		return true
	}

	ignored := commonIgnoredDirSet()
	parts := strings.SplitSeq(path, string(os.PathSeparator))
	for part := range parts {
		if ignored[part] {
			return true
		}
	}
	return false
}

// FastGlobWalker provides gitignore-aware file walking with fastwalk
// It uses hierarchical ignore checking like git does, checking .gitignore/.sennitignore
// files in each directory from the root to the target path.
type FastGlobWalker struct {
	directoryLister *directoryLister
}

func NewFastGlobWalker(searchPath string) *FastGlobWalker {
	return &FastGlobWalker{
		directoryLister: NewDirectoryLister(searchPath),
	}
}

// ShouldSkip checks if a file path should be skipped based on hierarchical gitignore,
// sennitignore, and hidden file rules.
func (w *FastGlobWalker) ShouldSkip(path string) bool {
	return w.directoryLister.shouldIgnore(path, nil, false)
}

// ShouldSkipDir checks if a directory path should be skipped based on hierarchical
// gitignore, sennitignore, and hidden file rules.
func (w *FastGlobWalker) ShouldSkipDir(path string) bool {
	return w.directoryLister.shouldIgnore(path, nil, true)
}

// VisitGlobGitignoreAware streams every matching path without retaining the
// result set. Callers that paginate can therefore scan wide trees with memory
// proportional to their page size.
//
// visit is called from several goroutines at once — fastwalk is concurrent —
// so it must guard whatever it touches. The production caller adds to a
// mutex-guarded page scan; a caller collecting into a plain slice will read
// as working and fail only under -race, or lose entries without it.
//
// The modification time comes with the path because the glob tool orders its
// results by it, and the walk is the only place it can be had without a
// second stat of every match. A file whose info cannot be read is still
// visited, with a zero time: the caller wanted to know the path exists, and
// an unreadable mtime is a worse reason to hide it than to sort it last.
func VisitGlobGitignoreAware(ctx context.Context, pattern, searchPath string, visit func(path string, modTime time.Time)) error {
	pattern = filepath.ToSlash(pattern)
	walker := NewFastGlobWalker(searchPath)
	conf := fastwalk.Config{Follow: false, ToSlash: fastwalk.DefaultToSlash(), Sort: fastwalk.SortFilesFirst}
	err := fastwalk.Walk(&conf, searchPath, func(path string, d os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil
		}
		// A directory is both a candidate match and something to descend
		// into, so it is matched here and the walk continues either way.
		// Skipping the match — which this did for a while — makes a
		// pattern naming a directory ("pkg", "**/testdata") return
		// nothing at all, with no way for the caller to tell that from
		// "no such directory".
		if d.IsDir() {
			if walker.ShouldSkipDir(path) {
				return filepath.SkipDir
			}
		} else if walker.ShouldSkip(path) {
			return nil
		}
		relPath, relErr := filepath.Rel(searchPath, path)
		if relErr != nil {
			relPath = path
		}
		matched, matchErr := doublestar.Match(pattern, filepath.ToSlash(relPath))
		if matchErr != nil {
			return matchErr
		}
		if matched {
			var modTime time.Time
			if info, infoErr := d.Info(); infoErr == nil {
				modTime = info.ModTime()
			}
			visit(path, modTime)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fastwalk error: %w", err)
	}
	return nil
}

func ShouldExcludeFile(rootPath, filePath string) bool {
	info, err := os.Stat(filePath)
	isDir := err == nil && info.IsDir()
	return NewDirectoryLister(rootPath).
		shouldIgnore(filePath, nil, isDir)
}

func PrettyPath(path string) string {
	return home.Short(path)
}

func DirTrim(pwd string, lim int) string {
	var (
		out string
		sep = string(filepath.Separator)
	)
	dirs := strings.Split(pwd, sep)
	if lim > len(dirs)-1 || lim <= 0 {
		return pwd
	}
	for i := len(dirs) - 1; i > 0; i-- {
		out = sep + out
		if i == len(dirs)-1 {
			out = dirs[i]
		} else if i >= len(dirs)-lim {
			// Keep the first grapheme cluster, not the first byte: CJK,
			// combining marks, and emoji can span multiple bytes and runes,
			// so a byte or single rune would render the wrong character.
			first, _ := ansi.FirstGraphemeCluster(dirs[i], ansi.GraphemeWidth)
			out = first + out
		} else {
			out = "..." + out
			break
		}
	}
	out = filepath.Join("~", out)
	return out
}

// PathOrPrefix returns the prefix if the path starts with it, or falls back to
// the path otherwise.
func PathOrPrefix(path, prefix string) string {
	if HasPrefix(path, prefix) {
		return prefix
	}
	return path
}

// HasPrefix checks if the given path starts with the specified prefix.
// Uses filepath.Rel to determine if path is within prefix.
func HasPrefix(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	if err != nil {
		return false
	}
	// A real escape is exactly ".." or starts with ".." followed by a
	// separator. A sibling name that merely starts with ".." (e.g. "..foo")
	// must not be mistaken for an escape.
	sep := string(filepath.Separator)
	return rel != ".." && !strings.HasPrefix(rel, ".."+sep)
}

// ToUnixLineEndings converts Windows line endings (CRLF) to Unix line endings (LF).
func ToUnixLineEndings(content string) (string, bool) {
	if strings.Contains(content, "\r\n") {
		return strings.ReplaceAll(content, "\r\n", "\n"), true
	}
	return content, false
}

// ToWindowsLineEndings converts line endings to Windows line endings (CRLF).
// It first normalizes any existing CRLF to LF so mixed input ends up
// consistently CRLF instead of staying mixed. The bool reports whether the
// returned string differs from content.
func ToWindowsLineEndings(content string) (string, bool) {
	converted := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\n", "\r\n")
	return converted, converted != content
}

func truncate[T any](input []T, limit int) ([]T, bool) {
	if limit > 0 && len(input) > limit {
		return input[:limit], true
	}
	return input, false
}
