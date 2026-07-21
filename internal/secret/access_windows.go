//go:build windows

package secret

import "io/fs"

// accessExecutable on Windows: there are no unix exec bits and no
// access(X_OK); a stat-able regular file is treated as executable
// (matching the pre-broker behavior on this platform - shim/broker
// support for the Windows daemon is not wired up yet anyway).
func accessExecutable(_ string, info fs.FileInfo) bool {
	return info.Mode().IsRegular()
}
