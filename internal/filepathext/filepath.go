package filepathext

import (
	"path/filepath"
	"runtime"
	"strings"
)

// SmartJoin joins two paths, treating the second path as absolute if it is an
// absolute path.
func SmartJoin(one, two string) string {
	if SmartIsAbs(two) {
		return two
	}
	return filepath.Join(one, two)
}

// SmartIsAbs checks if a path is absolute, considering both OS-specific and
// Unix-style paths.
func SmartIsAbs(path string) bool {
	return smartIsAbs(runtime.GOOS, path)
}

// smartIsAbs is the GOOS-parameterized core of SmartIsAbs. A config value
// (WorktreeDir, a skills path, ...) may be written with Unix-style
// separators for portability even when the process runs on Windows, where
// plain filepath.IsAbs rejects such a path for lacking a drive letter or
// UNC prefix. Splitting out the GOOS check lets that Windows branch be
// regression-tested from any host, since filepath.IsAbs and filepath.ToSlash
// otherwise only behave like Windows when actually built for it.
func smartIsAbs(goos, path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if goos != "windows" {
		return false
	}
	return strings.HasPrefix(strings.ReplaceAll(path, `\`, "/"), "/")
}

// SplitGlobPrefix splits a glob pattern into the longest leading run of
// literal path segments and the remaining pattern. The prefix contains no
// glob metacharacters, so callers can safely use it as a directory to start
// a walk from. For "internal/agent/*.go" it returns ("internal/agent",
// "*.go"); for "**/foo.go" it returns ("", "**/foo.go").
func SplitGlobPrefix(pattern string) (prefix, rest string) {
	pattern = filepath.ToSlash(pattern)
	segments := strings.Split(pattern, "/")
	var literal []string
	for i, seg := range segments {
		if strings.ContainsAny(seg, "*?[{\\") {
			rest = strings.Join(segments[i:], "/")
			return strings.Join(literal, "/"), rest
		}
		literal = append(literal, seg)
	}
	// Whole pattern is literal (a plain path); walk its parent and match
	// the basename so the existing match logic still applies.
	if len(literal) == 0 {
		return "", pattern
	}
	parent := strings.Join(literal[:len(literal)-1], "/")
	return parent, literal[len(literal)-1]
}
