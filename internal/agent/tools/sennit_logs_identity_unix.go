//go:build !windows

package tools

import (
	"os"
	"syscall"
)

// fileDevInode returns the device and inode numbers that identify the file
// backing info. They are the strongest cross-restart identity a byte-offset
// cursor can carry: a byte offset is only meaningful for the same file.
func fileDevInode(_ *os.File, info os.FileInfo) (dev, ino uint64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}
