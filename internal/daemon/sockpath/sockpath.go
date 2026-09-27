// Package sockpath computes the unix domain socket path a project's
// sennit daemon listens on. The path is deterministic and keyed on the
// same directory workspacelock locks, so a client that knows a project's
// working directory can find its daemon without reading any state file
// first (CLIENT-SERVER.md, "сокет").
package sockpath

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/fsext"
)

// maxPathLen returns the platform's sockaddr_un.sun_path capacity, the
// hard ceiling on how long a unix socket path may be. darwin ships a
// 104-byte buffer; Linux and Windows's AF_UNIX emulation (afunix, which
// Go's net package has used on Windows 10+ since Go 1.15) both ship 108.
func maxPathLen() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108
}

// runtimeDir returns the directory sockets are created in and the name
// of the environment variable a caller could set to relocate it (used
// only for the length-limit error message).
//
// On Windows there is no XDG_RUNTIME_DIR analogue; %LOCALAPPDATA% is the
// per-user, per-machine directory Windows apps use for exactly this kind
// of local runtime state, so it is preferred over TEMP (which several
// tools sweep periodically and which — unlike LOCALAPPDATA — has no
// convention for owner-only permissions).
func runtimeDir() (dir, envVar string) {
	if runtime.GOOS == "windows" {
		envVar = "LOCALAPPDATA"
		if v := os.Getenv(envVar); v != "" {
			return filepath.Join(v, brand.Slug, "run"), envVar
		}
		return filepath.Join(os.TempDir(), brand.Slug+"-run"), envVar
	}

	envVar = "XDG_RUNTIME_DIR"
	if v := os.Getenv(envVar); v != "" {
		return filepath.Join(v, brand.Slug), envVar
	}
	// os.Getuid is defined on every GOOS (it returns -1 on Windows, which
	// never reaches this branch), so no build tag is needed to call it
	// here for the non-Windows fallback.
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", brand.Slug, os.Getuid())), envVar
}

// Path returns the unix socket path for the daemon serving the workspace
// keyed by dir — the same directory workspacelock locks (a git
// repository's common directory, or the project's .sennit data directory
// otherwise; see app.WorkspaceLockDir, which callers should use to
// compute dir rather than recomputing the git/data-dir choice
// themselves).
//
// The path is $runtimeDir/<hex(sha256(canonical dir))[:16]>.sock, so the
// same project always resolves to the same socket and unrelated projects
// never collide. Path creates runtimeDir (0700) if it does not exist, and
// refuses a path longer than the platform's sockaddr_un.sun_path limit,
// naming both the offending path and the environment variable
// (XDG_RUNTIME_DIR, or LOCALAPPDATA on Windows) that would shorten it.
func Path(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("sockpath: resolve %q: %w", dir, err)
	}
	// The key must not change when the directory comes into existence:
	// Run resolves the path before Bootstrap creates the lock directory,
	// and every later client resolves it after. A plain EvalSymlinks
	// fails on the missing directory, and a fallback to the unresolved
	// path then keys a different socket than the resolved one does
	// whenever an ancestor is a symlink (macOS's /var -> /private/var) or,
	// on Windows, an 8.3 short name (RUNNER~1). fsext.Canonical resolves
	// the nearest existing ancestor and rejoins the missing components,
	// so both calls agree. Windows paths are case-insensitive, and
	// Canonical lower-cases only its missing-path branch, so the key is
	// lower-cased on Windows in every case.
	resolved := fsext.Canonical(abs)
	if runtime.GOOS == "windows" {
		resolved = strings.ToLower(resolved)
	}

	sum := sha256.Sum256([]byte(resolved))
	key := hex.EncodeToString(sum[:])[:16]

	dirPath, envVar := runtimeDir()
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		return "", fmt.Errorf("sockpath: create runtime directory %q: %w", dirPath, err)
	}

	sockPath := filepath.Join(dirPath, key+".sock")
	if limit := maxPathLen(); len(sockPath) > limit {
		return "", fmt.Errorf(
			"sockpath: socket path %q is %d bytes, over this platform's %d-byte limit; set %s to a shorter directory",
			sockPath, len(sockPath), limit, envVar,
		)
	}
	return sockPath, nil
}
