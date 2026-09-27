// Package testenv provides deterministic environment values for internal tests.
package testenv

import (
	"os"
	"runtime"
	"testing"

	"github.com/rave-soft/sennit/internal/env"
)

type mapEnv map[string]string

// New returns an Env backed by values supplied by the test.
func New(values map[string]string) env.Env {
	if values == nil {
		values = map[string]string{}
	}
	return mapEnv(values)
}

func (m mapEnv) Get(key string) string {
	return m[key]
}

func (m mapEnv) Env() []string {
	values := make([]string, 0, len(m))
	for key, value := range m {
		values = append(values, key+"="+value)
	}
	return values
}

// ShortRuntimeDir returns a short-path directory a test can point
// XDG_RUNTIME_DIR at when it needs sockpath.Path to build a real unix
// socket path.
//
// t.TempDir() nests under a per-test (and per-subtest) directory, which on
// macOS lands under /var/folders/xx/xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/T/ --
// often 90+ bytes by itself. Once sockpath.Path appends its own
// runtime-dir-relative filename, the result can exceed
// sockaddr_un.sun_path's 104-byte limit on darwin/BSD (Linux and Windows
// tolerate 108), even though nothing about the *product* built too long a
// path. /tmp is not nested per-test and is only a few bytes, so a
// directory created directly under it keeps the final socket path well
// inside the limit.
//
// Windows has no XDG_RUNTIME_DIR analogue -- sockpath ignores the
// variable there and falls back to LOCALAPPDATA instead -- and no
// guaranteed /tmp, so on that platform this creates its short directory
// under os.TempDir() instead of the literal "/tmp": t.TempDir() nests
// under a per-test directory long enough, once sockpath appends
// "\sennit\run\<hash>.sock", to push a Windows CI runner's socket path
// over sockaddr_un.sun_path's 108-byte limit too (TestPath_DiffersByDirectory
// and friends hit exactly this before this fix).
func ShortRuntimeDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dir, err := os.MkdirTemp("", "sennit-sock-")
		if err != nil {
			t.Fatalf("testenv: create short runtime dir: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	dir, err := os.MkdirTemp("/tmp", "sennit-sock-")
	if err != nil {
		t.Fatalf("testenv: create short runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ShortSocketDir returns a short-path directory a test can build a
// literal unix socket file path under directly (net.Listen("unix", ...)
// or similar), for a test that isn't going through sockpath.Path (which
// ShortRuntimeDir already shortens XDG_RUNTIME_DIR for -- see its doc
// comment). t.TempDir() nests under a per-test directory whose name
// embeds the full test name, which routinely pushes a
// "<dir>/whatever.sock" path past sockaddr_un.sun_path's 104-byte
// (darwin) / 108-byte (Linux, Windows) limit on CI, even though nothing
// about the code under test built too long a path. A directory created
// directly under the OS temp dir (os.MkdirTemp("", ...), so this works
// on Windows too, unlike a literal "/tmp") keeps the final socket path
// well inside the limit on every OS.
func ShortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sennit-sock-")
	if err != nil {
		t.Fatalf("testenv: create short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
