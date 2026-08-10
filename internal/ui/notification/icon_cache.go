package notification

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// CacheIcon writes data to <os.UserCacheDir()>/braid/braid.png, skipping the
// write if a file already there has identical content, and returns the
// resulting path. Backends that need a filesystem path rather than raw
// bytes (native OS notifications via beeep) call this once and reuse the
// path across sends instead of writing a fresh temp file on every
// notification. If the icon changes (e.g. a new version ships a redesigned
// mark), the content mismatch is detected and the cached file is
// overwritten.
func CacheIcon(data []byte) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	path := filepath.Join(dir, "braid", "braid.png")

	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create icon cache dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write cached icon: %w", err)
	}
	return path, nil
}
