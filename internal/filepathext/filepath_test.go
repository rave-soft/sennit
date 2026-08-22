package filepathext

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSmartIsAbs_DelegatesToHostGOOS pins that the exported SmartIsAbs is
// nothing but smartIsAbs pinned to the host's own runtime.GOOS. The two
// tests below cover the actual per-platform rules through the unexported
// core; this one only guards the thin wiring between them.
func TestSmartIsAbs_DelegatesToHostGOOS(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/var/tmp/sennit-threads", `\var\tmp\sennit-threads`, "sennit-threads"} {
		require.Equal(t, smartIsAbs(runtime.GOOS, path), SmartIsAbs(path))
	}
}

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

// TestIsAbsFor_WindowsRules pins the hand-rolled Windows rule that
// isAbsFor falls back to when goos is not the host. That rule is the
// whole reason the seam exists -- it is what a Linux run consults when
// asked what Windows would say -- so it needs its own coverage rather
// than being exercised only incidentally through smartIsAbs. Without
// this, the UNC branch was reached by no test at all: a mutation that
// disabled it left the suite green.
func TestIsAbsFor_WindowsRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		// isAbsFor defers to filepath.IsAbs when goos is the host, so on
		// Windows this would test the standard library, not the rule below.
		t.Skip("on Windows isAbsFor delegates to filepath.IsAbs; nothing hand-rolled to pin")
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{`C:\Users\x`, true},
		{`C:/Users/x`, true},
		{`c:\users\x`, true},
		{`\\server\share`, true},
		{`//server/share`, true},
		{`C:foo`, false}, // drive-relative, not absolute
		{`C:`, false},
		{`foo\bar`, false},
		{`/var/tmp`, false}, // absolute for Unix, not under Windows' own rule
		{``, false},
	} {
		require.Equal(t, tc.want, isAbsFor("windows", tc.path), "isAbsFor(windows, %q)", tc.path)
	}
}
