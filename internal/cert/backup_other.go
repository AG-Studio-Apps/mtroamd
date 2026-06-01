//go:build !linux

package cert

// excludeFromBackup is a no-op on non-Linux platforms. The daemon ships
// only on Linux today; the macOS/iOS client marks its own sensitive state
// (tsnet keys) via NSURLIsExcludedFromBackupKey at the app layer, so there
// is no server-side equivalent to set here.
func excludeFromBackup(_ string) error {
	return nil
}
