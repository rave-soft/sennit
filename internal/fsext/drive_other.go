//go:build !windows

package fsext

// WindowsWorkingDirDrive returns the drive letter of the current working
// directory, e.g. "C:".
// Falls back to the system drive if the current working directory cannot be
// determined.
func WindowsWorkingDirDrive() string {
	panic("cannot call fsext.WindowsWorkingDirDrive() on non-Windows OS")
}
