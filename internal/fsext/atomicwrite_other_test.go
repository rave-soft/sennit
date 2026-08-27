//go:build !windows

package fsext

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsUnsupportedLinkError pins the errno classes AtomicCreateFile treats
// as "this filesystem cannot hard-link" versus errors that must pass
// through unchanged, in particular os.ErrExist, which ErrFileChanged
// depends on.
func TestIsUnsupportedLinkError(t *testing.T) {
	unsupported := []error{syscall.ENOSYS, syscall.EPERM, syscall.EXDEV}
	for _, errno := range unsupported {
		require.True(t, isUnsupportedLinkError(&os.LinkError{Op: "link", Err: errno}), "%v should be treated as unsupported", errno)
	}

	other := []error{syscall.EEXIST, syscall.ENOSPC, syscall.EACCES, syscall.EROFS}
	for _, errno := range other {
		require.False(t, isUnsupportedLinkError(&os.LinkError{Op: "link", Err: errno}), "%v should not be treated as unsupported", errno)
	}

	require.True(t, errors.Is(&os.LinkError{Op: "link", Err: syscall.EEXIST}, os.ErrExist))
}
