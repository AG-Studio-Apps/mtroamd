//go:build !linux && !darwin

package ptysidecar

import "os"

// foregroundComm is implemented on Linux (/proc) and macOS (sysctl).
// Other platforms (e.g. freebsd) report "" — the poller then never
// observes a change and FrameFgState is never emitted (zero cost,
// field simply absent). A freebsd impl would use its own sysctl
// KERN_PROC equivalents.
func foregroundComm(_ *os.File, _ int) string { return "" }

// ResolveForegroundComm is the stub for the shared resolver (see the
// Linux + darwin builds).
func ResolveForegroundComm(_, _ int) string { return "" }
