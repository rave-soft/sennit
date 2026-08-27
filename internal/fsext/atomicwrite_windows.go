//go:build windows

package fsext

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isTransientRenameError reports whether err is a Windows rename
// failure that can resolve on its own: replacing the destination fails
// while another handle (a concurrent reader, antivirus, or the search
// indexer) is briefly open on it without FILE_SHARE_DELETE.
func isTransientRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// isUnsupportedLinkError reports whether err is a hard-link failure caused
// by the volume not supporting hard links (FAT32/exFAT, some network
// shares), rather than the link target already existing (os.Link surfaces
// that case as os.ErrExist, checked separately by the caller).
//
//   - ERROR_NOT_SUPPORTED / ERROR_INVALID_FUNCTION: the volume's driver has
//     no hard-link support (FAT32, exFAT).
//   - ERROR_ACCESS_DENIED: some network shares refuse hard-link creation
//     outright even though other writes to the directory succeed.
//
// A false negative here (a real permission problem misclassified as
// "unsupported") just costs one extra syscall: the O_EXCL fallback hits
// the same underlying problem and returns an equivalent error.
func isUnsupportedLinkError(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
