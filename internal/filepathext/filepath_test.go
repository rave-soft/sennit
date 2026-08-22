package filepathext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSmartIsAbs_WindowsAcceptsUnixStylePaths pins the Windows branch of
// SmartIsAbs: a config value written with Unix-style separators (portable
// across platforms) must still be recognized as absolute, even though
// filepath.IsAbs on Windows requires a drive letter or UNC prefix and
// would otherwise reject it. This regressed once already — NewManager
// (internal/thread) called filepath.IsAbs directly instead of going
// through this helper, so a WorktreeDir like "/var/tmp/sennit-threads"
// was silently anchored under the repo root on Windows instead of being
// used as-is. Exercised via the unexported core so it runs on any host.
func TestSmartIsAbs_WindowsAcceptsUnixStylePaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"unix-style absolute, forward slashes", "/var/tmp/sennit-threads", true},
		{"windows-style absolute, backslashes", `\var\tmp\sennit-threads`, true},
		{"relative", "sennit-threads", false},
		{"relative, nested", `sub\dir`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, smartIsAbs("windows", tc.path))
		})
	}
}

// TestSmartIsAbs_NonWindowsIgnoresUnixHeuristic guards the other side: on
// a non-Windows GOOS, only filepath.IsAbs decides — a value that merely
// starts with "\" (a plain filename character there) is not treated as
// absolute.
func TestSmartIsAbs_NonWindowsIgnoresUnixHeuristic(t *testing.T) {
	t.Parallel()

	require.False(t, smartIsAbs("linux", `\var\tmp\sennit-threads`))
	require.True(t, smartIsAbs("linux", "/var/tmp/sennit-threads"))
}

func TestSplitGlobPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern    string
		wantPrefix string
		wantRest   string
	}{
		{"*.go", "", "*.go"},
		{"**/foo.go", "", "**/foo.go"},
		{"internal/agent/*.go", "internal/agent", "*.go"},
		{"internal/**/*.go", "internal", "**/*.go"},
		{"a/b/c/*.txt", "a/b/c", "*.txt"},
		// A fully literal path walks its parent and matches the basename.
		{"internal/agent/glob.go", "internal/agent", "glob.go"},
		{"glob.go", "", "glob.go"},
		// A brace or bracket in the first segment means no literal prefix.
		{"{a,b}/x.go", "", "{a,b}/x.go"},
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()
			gotPrefix, gotRest := SplitGlobPrefix(tc.pattern)
			require.Equal(t, tc.wantPrefix, gotPrefix, "prefix")
			require.Equal(t, tc.wantRest, gotRest, "rest")
		})
	}
}
