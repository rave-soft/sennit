//go:build !windows

package config

import "io/fs"

// markerIsPrivate reports whether the trust marker is readable only by its
// owner. A marker another user can write is a marker another user can forge,
// so trust granted by one is worth nothing.
func markerIsPrivate(info fs.FileInfo) bool {
	return info.Mode().Perm()&0o077 == 0
}
