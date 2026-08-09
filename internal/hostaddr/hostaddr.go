// Package hostaddr resolves and parses the addresses Braid's client and
// server use to find each other: the default Unix socket / named pipe
// location and the URL parsing shared by both sides.
package hostaddr

import (
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// maxUnixSocketPathLen is the maximum length of a Unix domain socket
// path. The macOS sun_path field is 104 bytes; Linux allows 108. We
// use 104 so the resulting path is portable across both platforms.
const maxUnixSocketPathLen = 104

// socketDir returns the directory used for the Braid Unix socket.
// It prefers $XDG_RUNTIME_DIR when set (systemd's per-user runtime
// directory on Linux), and otherwise falls back to [os.TempDir],
// which resolves to the per-user private $TMPDIR on macOS and to
// /tmp on Linux.
func socketDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// ParseHostURL parses a host URL into a [url.URL].
func ParseHostURL(host string) (*url.URL, error) {
	proto, addr, ok := strings.Cut(host, "://")
	if !ok {
		return nil, fmt.Errorf("invalid host format: %s", host)
	}

	var basePath string
	if proto == "tcp" {
		parsed, err := url.Parse("tcp://" + addr)
		if err != nil {
			return nil, fmt.Errorf("invalid tcp address: %v", err)
		}
		addr = parsed.Host
		basePath = parsed.Path
	}
	return &url.URL{
		Scheme: proto,
		Host:   addr,
		Path:   basePath,
	}, nil
}

// DefaultHost returns the default server host.
//
// On Windows the address is a named pipe under \\.\pipe\. On Unix
// platforms the socket lives in the per-user runtime directory
// returned by [socketDir] and is named braid-<uid>.sock, falling
// back to braid.sock when the current uid cannot be determined. If
// the composed path would exceed [maxUnixSocketPathLen] bytes (the
// macOS sun_path limit), we fall back to /tmp/braid-<uid>.sock so
// the socket remains bindable.
func DefaultHost() string {
	sock := "braid.sock"
	usr, err := user.Current()
	if err == nil && usr.Uid != "" {
		sock = fmt.Sprintf("braid-%s.sock", usr.Uid)
	}
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("npipe:////./pipe/%s", sock)
	}
	path := filepath.Join(socketDir(), sock)
	if len(path) > maxUnixSocketPathLen {
		path = filepath.Join("/tmp", sock)
	}
	return "unix://" + path
}
