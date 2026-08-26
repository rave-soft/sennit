//go:build windows

package config

import "io/fs"

// markerIsPrivate reports whether the trust marker is readable only by its
// owner.
//
// Windows has no Unix permission bits: Go synthesizes Mode().Perm() from the
// read-only attribute alone and reports 0666 for every writable file, so the
// Unix check would reject every marker Trust just wrote — which is how a whole
// column of trust tests went red on windows-latest while Linux and macOS were
// green. Access is governed by the ACL the marker inherits from the
// trusted-projects directory under the user's profile instead.
func markerIsPrivate(fs.FileInfo) bool { return true }
