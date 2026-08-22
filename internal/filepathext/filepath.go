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
// regression-tested from any host.
//
// It deliberately does not call filepath.IsAbs: that function's own
// judgment follows the *build's* GOOS, not the goos argument, so on an
// actual Windows run "smartIsAbs("linux", ...)" would still get Windows
// rules for the base check. isAbsFor below reimplements the per-platform
// rule so the goos argument, not the host, decides.
func smartIsAbs(goos, path string) bool {
	if isAbsFor(goos, path) {
		return true
	}
	if goos != "windows" {
		return false
	}
	return strings.HasPrefix(strings.ReplaceAll(path, `\`, "/"), "/")
}

// isAbsFor is filepath.IsAbs's rule for goos, computed without relying on
// the host actually being that OS.
//
// When goos is the host, defer to filepath.IsAbs itself rather than to the
// approximation below: the standard library handles cases this does not
// (reserved device names like NUL, degenerate UNC prefixes, \\?\ paths),
// and production must keep exactly the stdlib's answer. The hand-rolled
// rule exists only so a test can ask what the *other* platform would say.
func isAbsFor(goos, path string) bool {
	if goos == runtime.GOOS {
		return filepath.IsAbs(path)
	}
	if goos != "windows" {
		return strings.HasPrefix(path, "/")
	}
	// Drive-letter form ("C:\..." or "C:/...").
	if len(path) >= 3 && isDriveLetter(path[0]) && path[1] == ':' && isWindowsSep(path[2]) {
		return true
	}
	// UNC form ("\\server\share" or "//server/share").
	return len(path) >= 2 && isWindowsSep(path[0]) && isWindowsSep(path[1])
}

func isDriveLetter(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

func isWindowsSep(b byte) bool {
	return b == '/' || b == '\\'
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
