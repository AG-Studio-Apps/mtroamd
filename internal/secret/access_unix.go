//go:build unix

package secret

import (
	"io/fs"
	"syscall"
)

// accessExecutable answers "can THIS uid actually exec path" via
// access(2) X_OK - not just "is any exec bit set". The shell's own PATH
// search skips a candidate it cannot execute and keeps scanning; a mode
// check alone would return e.g. another user's 0710 binary from an
// earlier PATH dir, then fail the exec with EACCES where the plain
// shell would have found the runnable one later on PATH (review
// finding). The mode probe stays as a fast-path cheap reject.
func accessExecutable(path string, info fs.FileInfo) bool {
	if info.Mode()&0o111 == 0 {
		return false
	}
	return syscall.Access(path, 0x1) == nil // X_OK
}
