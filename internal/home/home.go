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
	if homedir == "" || !strings.HasPrefix(p, homedir) {
		return p
	}
	// A bare prefix match also fires for an unrelated sibling directory
	// that happens to start with the same characters (homedir
	// "/home/bob" matching "/home/bobby"), so the byte right after the
	// prefix must be a separator (or the prefix must be the whole
	// string) before this counts as "inside home".
	rest := p[len(homedir):]
	if rest != "" && !os.IsPathSeparator(rest[0]) {
		return p
	}
	return filepath.Join("~", rest)
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
