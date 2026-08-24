//go:build windows

package tools

import (
	"os"

	"golang.org/x/sys/windows"
)

// fileDevInode returns the stable Win32 file identity of an open handle. The
// volume serial number and file index identify the file rather than its path,
// so a replacement at the same path invalidates a cursor while appends to the
// same file do not.
func fileDevInode(file *os.File, _ os.FileInfo) (dev, ino uint64) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, 0
	}
	index := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if index == 0 {
		return 0, 0
	}
	return uint64(info.VolumeSerialNumber), index
}
