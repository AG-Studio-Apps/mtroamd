//go:build linux

package cert

import (
	"os"

	"golang.org/x/sys/unix"
)

// fsNodumpFL is FS_NODUMP_FL from <linux/fs.h>. x/sys/unix exports the
// FS_IOC_{GET,SET}FLAGS ioctls but not this flag bit; the value is a
// stable kernel UABI constant.
const fsNodumpFL = 0x00000040

// excludeFromBackup sets the FS_NODUMP inode attribute on path — the
// conventional Linux signal that a file should be skipped by backup
// tooling (dump(8) and the backup utilities that honour it). Best-effort:
// many filesystems (tmpfs, some overlay/network fs) don't support inode
// flags and return ENOTSUP/ENOTTY, which the caller treats as non-fatal.
func excludeFromBackup(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		return err
	}
	if flags&fsNodumpFL != 0 {
		return nil // already excluded
	}
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, flags|fsNodumpFL)
}
