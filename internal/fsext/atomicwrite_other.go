//go:build !windows

package fsext

import (
	"errors"
	"syscall"
)

// isTransientRenameError reports whether err is a rename failure that
// can resolve on its own. Only Windows has such failures.
func isTransientRenameError(error) bool { return false }

// isUnsupportedLinkError reports whether err is a hard-link failure caused
// by the filesystem not supporting hard links at all, rather than the
// link target already existing (os.Link surfaces that case as os.ErrExist,
// checked separately by the caller).
//
//   - ENOSYS: the filesystem driver has no link() implementation (some
//     FUSE mounts).
//   - EPERM: link() is refused outright even though other writes to the
//     directory succeed (some network filesystems and container overlay
//     setups).
//   - EXDEV: tmp and path resolved to different devices. Both are created
//     in the same directory so this should not happen, but a bind mount
//     or union filesystem can still split them.
//
// A false negative here (a real permission or quota error misclassified
// as "supported") just costs one extra syscall: the O_EXCL fallback hits
// the same underlying problem and returns an equivalent error.
func isUnsupportedLinkError(err error) bool {
	return errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EXDEV)
}
