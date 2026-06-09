//go:build !linux && !darwin

package ptysidecar

import (
	"errors"
	"os"
)

// foregroundComm is implemented on Linux (/proc) and macOS (sysctl).
// Other platforms (e.g. freebsd) report "" — the poller then never
// observes a change and FrameFgState is never emitted (zero cost,
// field simply absent). A freebsd impl would use its own sysctl
// KERN_PROC equivalents.
func foregroundComm(_ *os.File, _ int) string { return "" }

// ResolveForegroundComm is the stub for the shared resolver (see the
// Linux + darwin builds).
func ResolveForegroundComm(_, _ int) string { return "" }

// foregroundCwd is implemented on Linux (/proc); "" elsewhere
// (FrameFgCwd never emitted, Cwd field simply absent). v1.6.3+.
func foregroundCwd(_ *os.File, _ int) string { return "" }

// killForegroundGroup is implemented on Linux + macOS (fg_kill.go);
// unsupported elsewhere. The FrameKillFg handler logs the error and
// the session is unaffected. v1.6.3+.
func killForegroundGroup(_ *os.File, _ int) error {
	return errors.New("killForegroundGroup: unsupported platform")
}
