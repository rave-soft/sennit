// Package home provides utilities for dealing with the user's home directory.
package home

import (
	"cmp"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

var homedir, homedirErr = os.UserHomeDir()

func init() {
	if homedirErr != nil {
		slog.Error("Failed to get user home directory", "error", homedirErr)
	}
}

// Dir returns the user home directory.
func Dir() string {
	return homedir
}

// Config returns the user config directory.
func Config() string {
	return cmp.Or(
		os.Getenv("XDG_CONFIG_HOME"),
		filepath.Join(Dir(), ".config"),
	)
}

// Short replaces the actual home path from [Dir] with `~`.
func Short(p string) string {
	return ShortWithHome(homedir, p)
}

// ShortWithHome is [Short] parameterized on the home directory to shorten
// against, rather than this process's own. A path that names a directory
// on another machine — the server's Workspace.WorkingDir(), once Sennit
// runs against a remote daemon — must be shortened against that machine's
// home, not this process's; see workspace.FrontendConfig.ServerHome and
// internal/ui/common.PrettyServerPath, its caller.
//
// home and p both describe the machine that produced them, which need not
// be this one: a remote daemon's ServerHome and the paths it reports keep
// whatever separator that machine's OS uses, regardless of this process's
// own GOOS. So the separator check and the join below must accept either
// convention and must not run the result through filepath.Join, which
// would normalize it to this process's native separator (e.g. turning a
// Linux daemon's "/proj/a.go" into "\proj\a.go" on a Windows client).
func ShortWithHome(home, p string) string {
	if home == "" || !strings.HasPrefix(p, home) {
		return p
	}
	// A bare prefix match also fires for an unrelated sibling directory
	// that happens to start with the same characters (home "/home/bob"
	// matching "/home/bobby"), so the byte right after the prefix must be
	// a separator (or the prefix must be the whole string) before this
	// counts as "inside home".
	rest := p[len(home):]
	if rest != "" && !isPathSeparator(rest[0]) {
		return p
	}
	if rest == "" {
		return "~"
	}
	return "~" + rest
}

// isPathSeparator reports whether b is a path separator under either the
// POSIX or the Windows convention. Unlike os.IsPathSeparator, this does not
// depend on the running process's GOOS: the string being tested may
// describe a different machine's filesystem (see ShortWithHome).
func isPathSeparator(b byte) bool {
	return b == '/' || b == '\\'
}

// Long replaces the `~` with actual home path from [Dir]. Only a bare
// `~` and `~/...` are expanded; `~user/...` (another user's home
// directory) is left untouched rather than mangled into homedir+"user"+
// the rest, since this package has no way to resolve another user's
// home directory anyway.
func Long(p string) string {
	if homedir == "" {
		return p
	}
	if p == "~" {
		return homedir
	}
	// os.IsPathSeparator, not a literal "~/": callers write "~/foo" with a
	// forward slash on every platform, but a path that has already been
	// through filepath.FromSlash arrives as "~\foo" on Windows, and both
	// name this user's home. On Unix a backslash is an ordinary filename
	// character, and IsPathSeparator says so, so "~\foo" stays untouched
	// there. Anything else after the tilde is another user's home, which
	// this package cannot resolve and must not mangle into homedir+"user".
	if len(p) < 2 || p[0] != '~' || !os.IsPathSeparator(p[1]) {
		return p
	}
	// Callers write "~/foo" with a literal forward slash regardless of
	// platform. homedir already uses the native separator (from
	// os.UserHomeDir), so the suffix needs the same treatment or the
	// result mixes "/" and "\" on Windows and no longer matches a path
	// built with filepath.Join.
	return homedir + filepath.FromSlash(strings.TrimPrefix(p, "~"))
}
